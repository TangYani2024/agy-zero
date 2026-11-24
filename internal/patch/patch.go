package patch

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"agy-zero/internal/budget"
	"agy-zero/internal/metrics"
	"agy-zero/internal/runner"
)

type Hunk struct {
	Target      string
	Replacement string
	StartLine   int // 0 = unset, 1-based inclusive
	EndLine     int // 0 = unset, 1-based inclusive
}

type FileSpec struct {
	Path      string
	Create    *string
	Overwrite bool
	Hunks     []Hunk
}

type Options struct {
	Cwd              string
	PatchText        string
	DryRun           bool
	BestEffort       bool
	VerifyCmd        string
	VerifyTimeoutSec int
	MaxBytes         int
}

// Parse parses heredoc patch text:
//
//	@@@ path/to/file[:start-end]
//	<<<
//	old lines
//	===
//	new lines
//	>>>
//	@@@+ path/to/new_file
//	full file content
//	@@@! path/to/overwrite_file
//	full file content
func Parse(raw string) ([]FileSpec, error) {
	norm := strings.ReplaceAll(raw, "\r\n", "\n")
	lines := strings.Split(norm, "\n")

	var specs []FileSpec
	idxByKey := map[string]int{}

	addHunk := func(path string, h Hunk) {
		if idx, ok := idxByKey[path]; ok {
			specs[idx].Hunks = append(specs[idx].Hunks, h)
			return
		}
		idxByKey[path] = len(specs)
		specs = append(specs, FileSpec{Path: path, Hunks: []Hunk{h}})
	}

	i := 0
	for i < len(lines) {
		line := lines[i]
		if strings.TrimSpace(line) == "" {
			i++
			continue
		}
		if strings.HasPrefix(line, "@@@+ ") || strings.HasPrefix(line, "@@@! ") {
			overwrite := strings.HasPrefix(line, "@@@! ")
			p := strings.TrimSpace(line[5:])
			if p == "" {
				return nil, fmt.Errorf("line %d: empty file path after %s", i+1, line[:4])
			}
			i++
			start := i
			for i < len(lines) && !strings.HasPrefix(lines[i], "@@@ ") && !strings.HasPrefix(lines[i], "@@@+ ") && !strings.HasPrefix(lines[i], "@@@! ") {
				i++
			}
			content := strings.Join(lines[start:i], "\n")
			if i == len(lines) && len(lines) > start && lines[len(lines)-1] == "" {
				// Preserve single trailing newline if present in heredoc.
				content = strings.Join(lines[start:i-1], "\n") + "\n"
			}
			c := content
			specs = append(specs, FileSpec{Path: p, Create: &c, Overwrite: overwrite})
			continue
		}
		if strings.HasPrefix(line, "@@@ ") {
			rawSpec := strings.TrimSpace(line[4:])
			filePath, sLine, eLine, err := parsePathAndRange(rawSpec)
			if err != nil {
				return nil, fmt.Errorf("line %d: %w", i+1, err)
			}
			i++
			foundAny := false
			for i < len(lines) {
				if strings.TrimSpace(lines[i]) == "" {
					i++
					continue
				}
				if strings.HasPrefix(lines[i], "@@@ ") || strings.HasPrefix(lines[i], "@@@+ ") || strings.HasPrefix(lines[i], "@@@! ") {
					break
				}
				if strings.TrimSpace(lines[i]) != "<<<" {
					return nil, fmt.Errorf("line %d: expected '<<<' to start hunk for %s (got %q)", i+1, filePath, lines[i])
				}
				i++
				tStart := i
				for i < len(lines) && strings.TrimSpace(lines[i]) != "===" {
					i++
				}
				if i >= len(lines) {
					return nil, fmt.Errorf("file %s: missing '===' separator in hunk", filePath)
				}
				target := strings.Join(lines[tStart:i], "\n")
				i++
				rStart := i
				for i < len(lines) && strings.TrimSpace(lines[i]) != ">>>" {
					i++
				}
				if i >= len(lines) {
					return nil, fmt.Errorf("file %s: missing '>>>' terminator in hunk", filePath)
				}
				repl := strings.Join(lines[rStart:i], "\n")
				i++
				addHunk(filePath, Hunk{
					Target:      target,
					Replacement: repl,
					StartLine:   sLine,
					EndLine:     eLine,
				})
				foundAny = true
			}
			if !foundAny {
				return nil, fmt.Errorf("file %s: header had no '<<< / === / >>>' hunks", filePath)
			}
			continue
		}
		return nil, fmt.Errorf("line %d: unexpected line outside '@@@ <path>' header: %q", i+1, line)
	}
	if len(specs) == 0 {
		return nil, errors.New("patch input is empty")
	}
	return specs, nil
}

func parsePathAndRange(raw string) (string, int, int, error) {
	colon := strings.LastIndexByte(raw, ':')
	if colon > 0 {
		rangePart := raw[colon+1:]
		if dash := strings.IndexByte(rangePart, '-'); dash > 0 {
			s, err1 := strconv.Atoi(rangePart[:dash])
			e, err2 := strconv.Atoi(rangePart[dash+1:])
			if err1 == nil && err2 == nil && s >= 1 && e >= s {
				return raw[:colon], s, e, nil
			}
		}
	}
	if raw == "" {
		return "", 0, 0, errors.New("empty file path")
	}
	return raw, 0, 0, nil
}

type plannedWrite struct {
	targetPath   string
	relPath      string
	existed      bool
	origBytes    []byte
	origMode     os.FileMode
	newContent   string
	matchCount   int
	lineDelta    int
	created      bool
}

func Run(opts Options) (string, bool, error) {
	wallStart := time.Now()
	specs, err := Parse(opts.PatchText)
	if err != nil {
		return "Patch parse failed (0 files modified): " + err.Error(), false, nil
	}
	if len(specs) > 50 {
		return "Patch rejected (0 files modified): at most 50 file entries per call", false, nil
	}
	cwd := opts.Cwd
	if cwd == "" {
		cwd, err = os.Getwd()
		if err != nil {
			return "", false, err
		}
	}
	absCwd, err := filepath.Abs(cwd)
	if err != nil {
		return "", false, err
	}
	maxBytes := opts.MaxBytes
	if maxBytes <= 0 {
		maxBytes = budget.DefaultMaxBytes()
	}

	var plans []plannedWrite
	var errs []string

	for _, sp := range specs {
		abs := sp.Path
		if !filepath.IsAbs(abs) {
			abs = filepath.Join(absCwd, sp.Path)
		}
		writePath := abs
		if realP, rerr := filepath.EvalSymlinks(abs); rerr == nil {
			writePath = realP
		}
		rel := sp.Path
		if r, rerr := filepath.Rel(absCwd, abs); rerr == nil && !strings.HasPrefix(r, "..") {
			rel = filepath.ToSlash(r)
		}

		if sp.Create != nil {
			st, serr := os.Stat(writePath)
			existed := false
			var origBytes []byte
			var origMode os.FileMode = 0o644
			if serr == nil {
				if st.IsDir() {
					errs = append(errs, fmt.Sprintf("%s: target is a directory", rel))
					continue
				}
				if !sp.Overwrite {
					errs = append(errs, fmt.Sprintf("%s: file exists (use @@@! to overwrite)", rel))
					continue
				}
				existed = true
				origMode = st.Mode().Perm()
				origBytes, serr = os.ReadFile(writePath)
				if serr != nil {
					errs = append(errs, fmt.Sprintf("%s: %v", rel, serr))
					continue
				}
			} else if !errors.Is(serr, os.ErrNotExist) {
				errs = append(errs, fmt.Sprintf("%s: %v", rel, serr))
				continue
			}
			oldLines := countLines(string(origBytes))
			newLines := countLines(*sp.Create)
			plans = append(plans, plannedWrite{
				targetPath: writePath,
				relPath:    rel,
				existed:    existed,
				origBytes:  origBytes,
				origMode:   origMode,
				newContent: *sp.Create,
				matchCount: 1,
				lineDelta:  newLines - oldLines,
				created:    !existed,
			})
			continue
		}

		st, serr := os.Stat(writePath)
		if serr != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", rel, serr))
			continue
		}
		if st.IsDir() {
			errs = append(errs, fmt.Sprintf("%s: is a directory", rel))
			continue
		}
		rawBytes, rerr := os.ReadFile(writePath)
		if rerr != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", rel, rerr))
			continue
		}
		if bytes.IndexByte(rawBytes[:min(len(rawBytes), 4096)], 0) >= 0 {
			errs = append(errs, fmt.Sprintf("%s: binary file", rel))
			continue
		}
		origText := string(rawBytes)
		isCRLF := strings.Count(origText, "\r\n") > strings.Count(strings.ReplaceAll(origText, "\r\n", ""), "\n")
		normText := strings.ReplaceAll(origText, "\r\n", "\n")

		updated, matches, herrs := applyHunks(normText, sp.Hunks, rel)
		if len(herrs) > 0 {
			errs = append(errs, herrs...)
			continue
		}
		finalText := updated
		if isCRLF {
			finalText = strings.ReplaceAll(updated, "\n", "\r\n")
		}
		plans = append(plans, plannedWrite{
			targetPath: writePath,
			relPath:    rel,
			existed:    true,
			origBytes:  rawBytes,
			origMode:   st.Mode().Perm(),
			newContent: finalText,
			matchCount: matches,
			lineDelta:  countLines(updated) - countLines(normText),
		})
	}

	if len(errs) > 0 && (!opts.BestEffort || len(plans) == 0) {
		msg := "Atomic patch aborted (0 files modified):\n- " + strings.Join(errs, "\n- ")
		return budget.TruncateUTF8(msg, maxBytes), false, nil
	}

	if !opts.DryRun {
		if err := commitWrites(plans); err != nil {
			return fmt.Sprintf("Write failed (rolled back): %v", err), false, nil
		}
	}

	var sumLines []string
	for _, p := range plans {
		act := fmt.Sprintf("updated (%d hunk(s))", p.matchCount)
		if p.created {
			act = "created"
		}
		delta := fmt.Sprintf("%+d", p.lineDelta)
		sumLines = append(sumLines, fmt.Sprintf("- %s: %s, lines %s", p.relPath, act, delta))
	}

	if opts.BestEffort && len(errs) > 0 {
		out := fmt.Sprintf("Partial patch applied (%d written, %d failed):\n\nFailed:\n- %s\n\nApplied:\n%s",
			len(plans), len(errs), strings.Join(errs, "\n- "), strings.Join(sumLines, "\n"))
		return budget.TruncateUTF8(out, maxBytes), false, nil
	}

	header := fmt.Sprintf("Applied patch to %d file(s):", len(plans))
	if opts.DryRun {
		header = fmt.Sprintf("Dry-run validated %d file(s) (0 modified):", len(plans))
	}
	parts := []string{header + "\n" + strings.Join(sumLines, "\n")}

	allOk := true
	rawVerifyBytes := 0
	verifyRan := false
	if !opts.DryRun && strings.TrimSpace(opts.VerifyCmd) != "" {
		verifyRan = true
		tSec := opts.VerifyTimeoutSec
		if tSec <= 0 {
			tSec = 60
		}
		res := runner.ExecShell(absCwd, opts.VerifyCmd, time.Duration(tSec)*time.Second)
		rawVerifyBytes = len(res.Stdout) + len(res.Stderr)
		status := "PASS"
		if res.SpawnErr != "" {
			status = "ERROR"
			allOk = false
		} else if res.TimedOut {
			status = "TIMEOUT"
			allOk = false
		} else if res.ExitCode != 0 {
			status = fmt.Sprintf("FAIL exit %d", res.ExitCode)
			allOk = false
		}
		remBudget := max(400, maxBytes-len(parts[0])-120)
		compact := budget.CompactCommandOutput(res.Stdout, res.Stderr, res.ExitCode, res.TimedOut, res.SpawnErr, remBudget)
		parts = append(parts, fmt.Sprintf("Verify [%s in %dms] `%s`:\n%s", status, res.DurationMs, opts.VerifyCmd, compact))
	}

	finalOut := budget.TruncateUTF8(strings.Join(parts, "\n\n"), maxBytes)
	if !opts.DryRun {
		totalHunks := 0
		for _, p := range plans {
			totalHunks += p.matchCount
		}
		baseCalls := totalHunks
		turnsSaved := 0
		detail := fmt.Sprintf("%d file(s), %d hunk(s)", len(plans), totalHunks)
		if verifyRan {
			baseCalls++
			turnsSaved++
			detail += "+verify"
		}
		spills := 0
		if rawVerifyBytes > 8192 {
			spills = 1
			turnsSaved++
		}
		rawBytes := len(parts[0]) + rawVerifyBytes
		metrics.Record(metrics.Event{
			Command:       "patch",
			Workspace:     filepath.Base(absCwd),
			Detail:        detail,
			BaselineCalls: baseCalls,
			ActualCalls:   1,
			RawBytes:      rawBytes,
			OutBytes:      len(finalOut),
			SpillsAvoided: spills,
			EstTurnsSaved: turnsSaved,
			WallMs:        time.Since(wallStart).Milliseconds(),
			OK:            allOk,
		})
	}
	return finalOut, allOk, nil
}

type span struct {
	start, end int
	repl       string
	hunkIdx    int
}

func computeLineStarts(s string) []int {
	starts := []int{0}
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			starts = append(starts, i+1)
		}
	}
	return starts
}

func offsetToLine(starts []int, off int) int {
	lo, hi := 0, len(starts)-1
	for lo <= hi {
		mid := (lo + hi) / 2
		if starts[mid] <= off {
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}
	return max(1, hi+1)
}

func applyHunks(text string, hunks []Hunk, displayPath string) (string, int, []string) {
	starts := computeLineStarts(text)
	var spans []span
	var errs []string

	for idx, h := range hunks {
		if h.Target == "" {
			errs = append(errs, fmt.Sprintf("%s hunk #%d: empty target", displayPath, idx+1))
			continue
		}
		winStart, winEnd := 0, len(text)
		hasRange := h.StartLine > 0 || h.EndLine > 0
		if hasRange {
			sl := max(1, h.StartLine)
			el := len(starts)
			if h.EndLine > 0 && h.EndLine < el {
				el = h.EndLine
			}
			if sl > len(starts) || el < sl {
				errs = append(errs, fmt.Sprintf("%s hunk #%d: invalid line range %d-%d (file has %d lines)", displayPath, idx+1, sl, el, len(starts)))
				continue
			}
			winStart = starts[sl-1]
			if el < len(starts) {
				winEnd = starts[el]
			}
		}

		var found []int
		pos := winStart
		for pos <= winEnd-len(h.Target) {
			f := strings.Index(text[pos:winEnd], h.Target)
			if f < 0 {
				break
			}
			absOff := pos + f
			found = append(found, absOff)
			pos = absOff + max(1, len(h.Target))
		}

		if len(found) == 0 {
			hint := ""
			if hasRange {
				if g := strings.Index(text, h.Target); g >= 0 {
					hint = fmt.Sprintf(" Exact match exists outside range at L%d.", offsetToLine(starts, g))
				}
			}
			if hint == "" {
				hint = closestSnippet(text, h.Target)
			}
			errs = append(errs, fmt.Sprintf("%s hunk #%d: target not found.%s", displayPath, idx+1, hint))
			continue
		}
		if len(found) > 1 {
			var lineNums []string
			for _, f := range found[:min(len(found), 6)] {
				lineNums = append(lineNums, fmt.Sprintf("L%d", offsetToLine(starts, f)))
			}
			errs = append(errs, fmt.Sprintf("%s hunk #%d: target matched %d times (%s); add surrounding context or :start-end range", displayPath, idx+1, len(found), strings.Join(lineNums, ", ")))
			continue
		}
		spans = append(spans, span{start: found[0], end: found[0] + len(h.Target), repl: h.Replacement, hunkIdx: idx})
	}

	if len(errs) > 0 {
		return text, 0, errs
	}
	sort.Slice(spans, func(i, j int) bool {
		if spans[i].start != spans[j].start {
			return spans[i].start < spans[j].start
		}
		return spans[i].end < spans[j].end
	})
	for i := 1; i < len(spans); i++ {
		if spans[i].start < spans[i-1].end {
			errs = append(errs, fmt.Sprintf("%s: hunk #%d and hunk #%d overlap near L%d",
				displayPath, spans[i-1].hunkIdx+1, spans[i].hunkIdx+1, offsetToLine(starts, spans[i].start)))
		}
	}
	if len(errs) > 0 {
		return text, 0, errs
	}

	var b strings.Builder
	cur := 0
	for _, sp := range spans {
		b.WriteString(text[cur:sp.start])
		b.WriteString(sp.repl)
		cur = sp.end
	}
	b.WriteString(text[cur:])
	return b.String(), len(spans), nil
}

func closestSnippet(text, target string) string {
	tLines := strings.Split(target, "\n")
	anchor := ""
	for _, l := range tLines {
		tr := strings.TrimSpace(l)
		if len(tr) > len(anchor) {
			anchor = tr
		}
	}
	if len(anchor) < 4 {
		return ""
	}
	fLines := strings.Split(text, "\n")
	bestIdx, bestScore := -1, 0
	words := strings.Fields(anchor)
	for i, fl := range fLines {
		tr := strings.TrimSpace(fl)
		if tr == "" {
			continue
		}
		sc := 0
		if tr == anchor {
			sc = 100
		} else if strings.Contains(tr, anchor) || strings.Contains(anchor, tr) && len(tr) >= 6 {
			sc = 60
		} else {
			for _, w := range words {
				if len(w) >= 3 && strings.Contains(tr, w) {
					sc += 10
				}
			}
		}
		if sc > bestScore {
			bestScore = sc
			bestIdx = i
		}
	}
	if bestIdx < 0 || bestScore < 20 {
		return ""
	}
	from := max(0, bestIdx-2)
	to := min(len(fLines)-1, bestIdx+4)
	var b strings.Builder
	fmt.Fprintf(&b, "\n  Closest lines (%d-%d):", from+1, to+1)
	for i := from; i <= to; i++ {
		l := fLines[i]
		if len(l) > 140 {
			l = budget.SliceUTF8Head(l, 137) + "..."
		}
		fmt.Fprintf(&b, "\n    %d: %s", i+1, l)
	}
	return b.String()
}

func commitWrites(plans []plannedWrite) error {
	var committed []plannedWrite
	for _, p := range plans {
		dir := filepath.Dir(p.targetPath)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			rollback(committed)
			return err
		}
		tmp := fmt.Sprintf("%s.azero-tmp-%d-%d", p.targetPath, os.Getpid(), time.Now().UnixNano())
		if err := os.WriteFile(tmp, []byte(p.newContent), p.origMode); err != nil {
			_ = os.Remove(tmp)
			rollback(committed)
			return err
		}
		if err := os.Rename(tmp, p.targetPath); err != nil {
			_ = os.Remove(tmp)
			if werr := os.WriteFile(p.targetPath, []byte(p.newContent), p.origMode); werr != nil {
				rollback(committed)
				return werr
			}
		}
		committed = append(committed, p)
	}
	return nil
}

func rollback(committed []plannedWrite) {
	for i := len(committed) - 1; i >= 0; i-- {
		p := committed[i]
		if p.existed {
			_ = os.WriteFile(p.targetPath, p.origBytes, p.origMode)
		} else {
			_ = os.Remove(p.targetPath)
		}
	}
}

func countLines(s string) int {
	if s == "" {
		return 0
	}
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return 1
	}
	return strings.Count(s, "\n") + 1
}
