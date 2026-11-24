package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"agy-zero/internal/budget"
	"agy-zero/internal/metrics"
)

type CommandSpec struct {
	Name       string
	Command    string
	Cwd        string
	TimeoutSec int
}

type ExecResult struct {
	ExitCode   int
	TimedOut   bool
	DurationMs int64
	Stdout     string
	Stderr     string
	SpawnErr   string
}

type Options struct {
	Cwd         string
	Commands    []CommandSpec
	Concurrency int
	TimeoutSec  int
	Full        bool
	MaxBytes    int
}

const maxCaptureBytes = 1024 * 1024 // 1 MiB per stream

type cappedBuffer struct {
	buf       bytes.Buffer
	truncated bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	rem := maxCaptureBytes - c.buf.Len()
	if rem <= 0 {
		c.truncated = true
		return len(p), nil
	}
	if len(p) > rem {
		c.buf.Write(p[:rem])
		c.truncated = true
		return len(p), nil
	}
	return c.buf.Write(p)
}

func ExecShell(cwd, command string, timeout time.Duration) ExecResult {
	start := time.Now()
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := newShellCmd(cwd, command)
	var outBuf, errBuf cappedBuffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	if err := cmd.Start(); err != nil {
		return ExecResult{
			ExitCode:   -1,
			DurationMs: time.Since(start).Milliseconds(),
			SpawnErr:   err.Error(),
		}
	}

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	var waitErr error
	timedOut := false
	select {
	case waitErr = <-done:
	case <-ctx.Done():
		timedOut = true
		waitErr = killProcessTree(cmd, done)
	}

	exitCode := 0
	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = -1
		}
	}
	if timedOut {
		exitCode = -1
	}

	return ExecResult{
		ExitCode:   exitCode,
		TimedOut:   timedOut,
		DurationMs: time.Since(start).Milliseconds(),
		Stdout:     outBuf.buf.String(),
		Stderr:     errBuf.buf.String(),
	}
}

func ParseCommandArg(raw string, idx int) CommandSpec {
	raw = strings.TrimSpace(raw)
	if eq := strings.IndexByte(raw, '='); eq > 0 && eq <= 24 {
		label := raw[:eq]
		validLabel := true
		for i := 0; i < len(label); i++ {
			c := label[i]
			if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_') {
				validLabel = false
				break
			}
		}
		if validLabel && strings.TrimSpace(raw[eq+1:]) != "" {
			// Guard against leading ENV=VAL commands like FOO=bar cmd
			isAllUpper := true
			for i := 0; i < len(label); i++ {
				if label[i] >= 'a' && label[i] <= 'z' {
					isAllUpper = false
					break
				}
			}
			if !isAllUpper {
				return CommandSpec{Name: label, Command: strings.TrimSpace(raw[eq+1:])}
			}
		}
	}
	return CommandSpec{Name: fmt.Sprintf("check-%d", idx+1), Command: raw}
}

func Run(opts Options) (string, bool, error) {
	if len(opts.Commands) == 0 {
		return "", false, errors.New("at least one command is required")
	}
	if len(opts.Commands) > 12 {
		return "", false, errors.New("at most 12 commands per call")
	}
	cwd := opts.Cwd
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return "", false, err
		}
	}
	absCwd, err := filepath.Abs(cwd)
	if err != nil {
		return "", false, err
	}
	conc := budget.Clamp(opts.Concurrency, 1, 8)
	if opts.Concurrency <= 0 {
		conc = 4
	}
	defTimeout := opts.TimeoutSec
	if defTimeout <= 0 {
		defTimeout = 60
	}
	maxBytes := opts.MaxBytes
	if maxBytes <= 0 {
		maxBytes = budget.DefaultMaxBytes()
	}

	type outcome struct {
		spec   CommandSpec
		res    ExecResult
		passed bool
	}
	outcomes := make([]outcome, len(opts.Commands))
	wallStart := time.Now()

	var wg sync.WaitGroup
	sem := make(chan struct{}, conc)
	for i, spec := range opts.Commands {
		if spec.Name == "" {
			spec.Name = fmt.Sprintf("check-%d", i+1)
		}
		cmdCwd := absCwd
		if spec.Cwd != "" {
			if filepath.IsAbs(spec.Cwd) {
				cmdCwd = spec.Cwd
			} else {
				cmdCwd = filepath.Join(absCwd, spec.Cwd)
			}
		}
		tSec := spec.TimeoutSec
		if tSec <= 0 {
			tSec = defTimeout
		}
		wg.Add(1)
		go func(idx int, s CommandSpec, dir string, dur time.Duration) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			r := ExecShell(dir, s.Command, dur)
			ok := r.SpawnErr == "" && !r.TimedOut && r.ExitCode == 0
			outcomes[idx] = outcome{spec: s, res: r, passed: ok}
		}(i, spec, cmdCwd, time.Duration(tSec)*time.Second)
	}
	wg.Wait()
	wallMs := time.Since(wallStart).Milliseconds()

	passedCount := 0
	rawTotalBytes := 0
	spillsAvoided := 0
	var serialMs int64
	for _, o := range outcomes {
		if o.passed {
			passedCount++
		}
		cmdRaw := len(o.res.Stdout) + len(o.res.Stderr)
		rawTotalBytes += cmdRaw
		if cmdRaw > 8192 {
			spillsAvoided++
		}
		serialMs += o.res.DurationMs
	}
	recordRun := func(finalOut string) {
		metrics.Record(metrics.Event{
			Command:       "run",
			Workspace:     filepath.Base(absCwd),
			Detail:        fmt.Sprintf("%d/%d passed", passedCount, len(outcomes)),
			BaselineCalls: len(outcomes),
			ActualCalls:   1,
			RawBytes:      rawTotalBytes,
			OutBytes:      len(finalOut),
			SpillsAvoided: spillsAvoided,
			EstTurnsSaved: spillsAvoided,
			WallMs:        wallMs,
			SerialMs:      serialMs,
			OK:            passedCount == len(outcomes),
		})
	}
	failedCount := len(outcomes) - passedCount
	header := fmt.Sprintf("Checks: %d/%d passed", passedCount, len(outcomes))
	if failedCount > 0 {
		header += fmt.Sprintf(", %d failed", failedCount)
	}
	header += fmt.Sprintf(" (wall: %dms)", wallMs)

	bodyBudget := max(200, maxBytes-len(header)-2)
	blocks := make([]string, len(outcomes))

	if opts.Full {
		for i, o := range outcomes {
			badge := "PASS"
			if o.res.SpawnErr != "" {
				badge = "ERROR"
			} else if o.res.TimedOut {
				badge = "TIMEOUT"
			} else if !o.passed {
				badge = fmt.Sprintf("FAIL (exit %d)", o.res.ExitCode)
			}
			body := strings.TrimSpace(strings.Join([]string{
				budget.NormalizeTerminal(o.res.Stdout),
				budget.NormalizeTerminal(o.res.Stderr),
			}, "\n"))
			if o.res.SpawnErr != "" {
				body = "spawn error: " + o.res.SpawnErr
			} else if body == "" {
				body = "(no output)"
			}
			blocks[i] = fmt.Sprintf("## [%s %dms] %s\n$ %s\n%s", badge, o.res.DurationMs, o.spec.Name, o.spec.Command, body)
		}
		out := budget.TruncateUTF8(header+"\n\n"+budget.FitSections(blocks, bodyBudget, "\n\n", true), maxBytes)
		recordRun(out)
		return out, failedCount == 0, nil
	}

	// Compact mode: PASS gets 1-line summary; FAIL water-fills remaining budget.
	passBytes := 0
	failIdxs := make([]int, 0, failedCount)
	for i, o := range outcomes {
		if o.passed {
			summary := budget.CompactCommandOutput(o.res.Stdout, o.res.Stderr, 0, false, "", 200)
			blocks[i] = fmt.Sprintf("- [PASS %dms] %s: %s", o.res.DurationMs, o.spec.Name, summary)
			passBytes += len(blocks[i]) + 2
		} else {
			failIdxs = append(failIdxs, i)
		}
	}
	if len(failIdxs) > 0 {
		failPool := max(300, bodyBudget-passBytes)
		perFail := failPool / len(failIdxs)
		for _, idx := range failIdxs {
			o := outcomes[idx]
			badge := fmt.Sprintf("FAIL exit %d", o.res.ExitCode)
			if o.res.SpawnErr != "" {
				badge = "ERROR"
			} else if o.res.TimedOut {
				badge = "TIMEOUT"
			}
			prefix := fmt.Sprintf("## [%s %dms] %s\n$ %s\n", badge, o.res.DurationMs, o.spec.Name, o.spec.Command)
			body := budget.CompactCommandOutput(o.res.Stdout, o.res.Stderr, o.res.ExitCode, o.res.TimedOut, o.res.SpawnErr, max(200, perFail-len(prefix)))
			blocks[idx] = prefix + body
		}
	}

	out := budget.TruncateUTF8(header+"\n\n"+budget.FitSections(blocks, bodyBudget, "\n\n", true), maxBytes)
	recordRun(out)
	return out, failedCount == 0, nil
}
