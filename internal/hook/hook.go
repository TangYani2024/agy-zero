package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"agy-zero/internal/budget"
	"agy-zero/internal/metrics"
	"agy-zero/internal/search"
)

const maxSnapshotBytes = 1200

type sessionStartInput struct {
	Cwd            string   `json:"cwd"`
	WorkspacePaths []string `json:"workspacePaths"`
	WorkspaceSnake []string `json:"workspace_paths"`
	WorkspaceRoots []string `json:"workspaceRoots"`
}

type sessionStartOutput struct {
	InjectSteps []injectStep `json:"injectSteps,omitempty"`
}

type injectStep struct {
	SystemMessage *hookSystemMessage `json:"systemMessage,omitempty"`
}

type hookSystemMessage struct {
	SystemMessage string `json:"systemMessage"`
}

var repoMarkers = []string{
	".git", "package.json", "go.mod", "Cargo.toml", "pyproject.toml",
	"pom.xml", "build.gradle", "build.gradle.kts", "Makefile", "CMakeLists.txt",
}

func isHomeOrSystemRoot(dir string) bool {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return true
	}
	if abs == "/" || abs == "/home" || abs == "/Users" {
		return true
	}
	if home, err := os.UserHomeDir(); err == nil && filepath.Clean(home) == filepath.Clean(abs) {
		return true
	}
	return false
}

func hasRepoMarker(dir string) bool {
	for _, m := range repoMarkers {
		if _, err := os.Stat(filepath.Join(dir, m)); err == nil {
			return true
		}
	}
	return false
}

// RunSessionStart reads stdin payload and prints a compact <=1.2 KB snapshot ONLY
// when inside a real code repository; returns {} (0 tokens) for non-repo/chat dirs.
func RunSessionStart(stdin io.Reader, stdout io.Writer) {
	raw, err := io.ReadAll(io.LimitReader(stdin, 256*1024))
	if err != nil || len(bytes.TrimSpace(raw)) == 0 {
		_, _ = io.WriteString(stdout, "{}\n")
		return
	}
	var in sessionStartInput
	if err := json.Unmarshal(raw, &in); err != nil {
		_, _ = io.WriteString(stdout, "{}\n")
		return
	}
	ws := strings.TrimSpace(in.Cwd)
	for _, list := range [][]string{in.WorkspacePaths, in.WorkspaceSnake, in.WorkspaceRoots} {
		if ws != "" {
			break
		}
		for _, p := range list {
			if strings.TrimSpace(p) != "" {
				ws = strings.TrimSpace(p)
				break
			}
		}
	}
	start := time.Now()
	if ws == "" || isHomeOrSystemRoot(ws) || !hasRepoMarker(ws) {
		if ws != "" {
			metrics.Record(metrics.Event{
				Command:   "session-skip",
				Workspace: filepath.Base(ws),
				Detail:    "non-repo",
				WallMs:    time.Since(start).Milliseconds(),
				OK:        true,
			})
		}
		_, _ = io.WriteString(stdout, "{}\n")
		return
	}

	snap := BuildSnapshot(ws)
	if snap == "" {
		_, _ = io.WriteString(stdout, "{}\n")
		return
	}
	out := sessionStartOutput{
		InjectSteps: []injectStep{{SystemMessage: &hookSystemMessage{SystemMessage: snap}}},
	}
	enc, err := json.Marshal(out)
	if err != nil {
		_, _ = io.WriteString(stdout, "{}\n")
		return
	}
	metrics.Record(metrics.Event{
		Command:       "session-start",
		Workspace:     filepath.Base(ws),
		Detail:        "snapshot injected",
		BaselineCalls: 3, // typical cold-start: git status + git log + ls/manifest
		ActualCalls:   0, // runs before turn 1
		RawBytes:      len(snap),
		OutBytes:      len(snap),
		EstTurnsSaved: 2,
		WallMs:        time.Since(start).Milliseconds(),
		OK:            true,
	})
	_, _ = stdout.Write(append(enc, '\n'))
}

func BuildSnapshot(ws string) string {
	var wg sync.WaitGroup
	var gitPart, manifestPart, treePart string

	wg.Add(3)
	go func() {
		defer wg.Done()
		gitPart = gitSummary(ws)
	}()
	go func() {
		defer wg.Done()
		manifestPart = manifestSummary(ws)
	}()
	go func() {
		defer wg.Done()
		treePart = twoLevelTree(ws)
	}()
	wg.Wait()

	parts := []string{fmt.Sprintf("[agy-zero snapshot] Workspace: %s", ws)}
	if gitPart != "" {
		parts = append(parts, gitPart)
	}
	if manifestPart != "" {
		parts = append(parts, manifestPart)
	}
	if treePart != "" {
		parts = append(parts, treePart)
	}
	return budget.TruncateUTF8(strings.Join(parts, "\n"), maxSnapshotBytes)
}

func runFastGit(ws string, args ...string) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	full := append([]string{"--no-optional-locks", "-c", "core.fsmonitor=false", "-c", "diff.external=", "--no-pager"}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Dir = ws
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(out)), true
}

func gitSummary(ws string) string {
	var wg sync.WaitGroup
	var branch, status, commits string
	var okB, okS bool

	wg.Add(3)
	go func() {
		defer wg.Done()
		branch, okB = runFastGit(ws, "rev-parse", "--abbrev-ref", "HEAD")
	}()
	go func() {
		defer wg.Done()
		status, okS = runFastGit(ws, "status", "--porcelain=v1")
	}()
	go func() {
		defer wg.Done()
		commits, _ = runFastGit(ws, "log", "-n", "3", "--format=%h %s")
	}()
	wg.Wait()

	if !okB && !okS {
		return ""
	}
	if branch == "" {
		branch = "HEAD"
	}
	lines := []string{fmt.Sprintf("Git: `%s`", branch)}
	if status == "" {
		lines = append(lines, "Tree: clean")
	} else {
		dirty := strings.Split(status, "\n")
		shown := dirty
		more := ""
		if len(dirty) > 6 {
			shown = dirty[:6]
			more = fmt.Sprintf(" (+%d)", len(dirty)-6)
		}
		lines = append(lines, fmt.Sprintf("Dirty (%d): %s%s", len(dirty), strings.Join(shown, ", "), more))
	}
	if commits != "" {
		for _, c := range strings.Split(commits, "\n") {
			c = strings.TrimSpace(c)
			if c != "" {
				lines = append(lines, "  - "+budget.SliceUTF8Head(c, 68))
			}
		}
	}
	return strings.Join(lines, "\n")
}

func manifestSummary(ws string) string {
	var hints []string
	if raw, err := os.ReadFile(filepath.Join(ws, "package.json")); err == nil {
		var pkg struct {
			Name    string            `json:"name"`
			Scripts map[string]string `json:"scripts"`
		}
		if json.Unmarshal(raw, &pkg) == nil {
			var names []string
			for k := range pkg.Scripts {
				names = append(names, k)
			}
			sort.Strings(names)
			if len(names) > 10 {
				names = names[:10]
			}
			if len(names) > 0 {
				hints = append(hints, fmt.Sprintf("package.json (%s) scripts: %s", pkg.Name, strings.Join(names, ", ")))
			}
		}
	}
	if raw, err := os.ReadFile(filepath.Join(ws, "go.mod")); err == nil {
		for _, l := range strings.Split(string(raw), "\n") {
			if strings.HasPrefix(l, "module ") {
				hints = append(hints, "go.mod ("+strings.TrimSpace(strings.TrimPrefix(l, "module "))+")")
				break
			}
		}
	}
	if _, err := os.Stat(filepath.Join(ws, "Cargo.toml")); err == nil {
		hints = append(hints, "Cargo.toml")
	}
	if _, err := os.Stat(filepath.Join(ws, "pyproject.toml")); err == nil {
		hints = append(hints, "pyproject.toml")
	}
	if len(hints) == 0 {
		return ""
	}
	return "Project: " + strings.Join(hints, " | ")
}

func twoLevelTree(ws string) string {
	entries, err := os.ReadDir(ws)
	if err != nil {
		return ""
	}
	var dirs, files []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") || search.DefaultIgnoreDirs[name] {
			continue
		}
		if e.IsDir() {
			dirs = append(dirs, name)
		} else {
			files = append(files, name)
		}
	}
	sort.Strings(dirs)
	sort.Strings(files)

	var lines []string
	for i, d := range dirs {
		if i >= 10 {
			lines = append(lines, fmt.Sprintf("  ... (+%d dirs)", len(dirs)-i))
			break
		}
		subs, err := os.ReadDir(filepath.Join(ws, d))
		if err != nil {
			lines = append(lines, "  "+d+"/")
			continue
		}
		var names []string
		for _, s := range subs {
			sn := s.Name()
			if strings.HasPrefix(sn, ".") || search.DefaultIgnoreDirs[sn] {
				continue
			}
			if s.IsDir() {
				sn += "/"
			}
			names = append(names, sn)
		}
		sort.Strings(names)
		if len(names) > 5 {
			names = append(names[:5], fmt.Sprintf("+%d", len(names)-5))
		}
		if len(names) == 0 {
			lines = append(lines, "  "+d+"/")
		} else {
			lines = append(lines, fmt.Sprintf("  %s/ (%s)", d, strings.Join(names, ", ")))
		}
	}
	if len(files) > 0 {
		shown := files
		more := ""
		if len(files) > 8 {
			shown = files[:8]
			more = fmt.Sprintf(", +%d", len(files)-8)
		}
		lines = append(lines, "  files: "+strings.Join(shown, ", ")+more)
	}
	if len(lines) == 0 {
		return ""
	}
	return "Layout:\n" + strings.Join(lines, "\n")
}

// Experimental opt-in PostToolUse spill condenser (returns {} unless AZERO_POST_TOOL_CONDENSE=1).
var spillFileRE = regexp.MustCompile(`file://([^\s"']+/output\.txt)|saved to:\s*([^\s"']+/output\.txt)`)

type postToolInput struct {
	Result string `json:"result"`
	Error  string `json:"error"`
}

func RunPostTool(stdin io.Reader, stdout io.Writer) {
	if os.Getenv("AZERO_POST_TOOL_CONDENSE") != "1" {
		_, _ = io.WriteString(stdout, "{}\n")
		return
	}
	raw, err := io.ReadAll(io.LimitReader(stdin, 512*1024))
	if err != nil || !bytes.Contains(raw, []byte("output.txt")) {
		_, _ = io.WriteString(stdout, "{}\n")
		return
	}
	var in postToolInput
	if err := json.Unmarshal(raw, &in); err != nil {
		_, _ = io.WriteString(stdout, "{}\n")
		return
	}
	m := spillFileRE.FindStringSubmatch(in.Result)
	if m == nil {
		_, _ = io.WriteString(stdout, "{}\n")
		return
	}
	outPath := m[1]
	if outPath == "" {
		outPath = m[2]
	}
	full, err := os.ReadFile(outPath)
	if err != nil || len(full) == 0 {
		_, _ = io.WriteString(stdout, "{}\n")
		return
	}
	exitCode := 0
	if in.Error != "" {
		exitCode = 1
	}
	condensed := budget.CompactCommandOutput(string(full), "", exitCode, false, "", 7200)
	condensed += fmt.Sprintf("\n[agy-zero condensed %dB -> %dB from %s]", len(full), len(condensed), outPath)
	enc, err := json.Marshal(map[string]string{"overwriteResult": condensed})
	if err != nil {
		_, _ = io.WriteString(stdout, "{}\n")
		return
	}
	_, _ = stdout.Write(append(enc, '\n'))
}
