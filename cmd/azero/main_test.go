package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"agy-zero/internal/budget"
	"agy-zero/internal/hook"
	"agy-zero/internal/metrics"
	"agy-zero/internal/outline"
	"agy-zero/internal/patch"
	"agy-zero/internal/runner"
	"agy-zero/internal/search"
)

func TestMain(m *testing.M) {
	// Keep tests isolated from real user metrics files.
	tmp, _ := os.MkdirTemp("", "azero-test-metrics-*")
	_ = os.Setenv("AZERO_LOG_FILE", filepath.Join(tmp, "metrics.jsonl"))
	code := m.Run()
	_ = os.RemoveAll(tmp)
	os.Exit(code)
}

func TestBudgetWaterFillAndUTF8(t *testing.T) {
	alloc := budget.WaterFill([]int{100, 2000, 3000}, 1000)
	if alloc[0] != 100 || alloc[1]+alloc[2] != 900 {
		t.Fatalf("unexpected waterfill: %v", alloc)
	}
	cjk := strings.Repeat("中文测试", 500)
	tr := budget.TruncateHeadTailUTF8(cjk, 300)
	if len(tr) > 300 {
		t.Fatalf("exceeded byte cap: %d > 300", len(tr))
	}
	if !strings.Contains(tr, "folded") {
		t.Fatalf("expected folded marker, got: %s", tr)
	}
}

func TestReadonlyGitGuard(t *testing.T) {
	if _, err := search.ValidateReadonlyGitArgs("status --short"); err != nil {
		t.Fatalf("expected status --short valid: %v", err)
	}
	for _, bad := range []string{
		"push origin main",
		"status -v",
		"diff --ext-diff",
		"branch -D foo",
		"remote add evil https://example.com/a.git",
		"log && rm -rf /",
	} {
		if _, err := search.ValidateReadonlyGitArgs(bad); err == nil {
			t.Fatalf("expected %q to be rejected", bad)
		}
	}
}

func TestSearchAndOutline(t *testing.T) {
	dir := t.TempDir()
	goSrc := "package demo\n\ntype Server struct{}\n\nfunc (s *Server) Start() error {\n\treturn nil\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "server.go"), []byte(goSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := search.Run(search.Options{
		Cwd: dir,
		Queries: []search.Query{
			{Type: search.QueryText, Pattern: "Start()", CaseSensitive: true},
			{Type: search.QueryFuzzy, Pattern: "srv"},
		},
		MaxBytes: 4000,
	})
	if err != nil || !strings.Contains(out, "server.go:5:") {
		t.Fatalf("unexpected search out: %v %s", err, out)
	}
	ol, err := outline.Run(outline.Options{Cwd: dir, Paths: []string{"server.go"}, MaxBytes: 4000})
	if err != nil || !strings.Contains(ol, "L5-7 method (*Server).Start") || !strings.Contains(ol, "L3 struct Server") {
		t.Fatalf("unexpected Go AST outline: %v\n%s", err, ol)
	}
}

func TestPatchAtomicAndRollbackAndVerify(t *testing.T) {
	dir := t.TempDir()
	f1 := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(f1, []byte("line1\noldVal\nline3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Atomic abort when second hunk fails -> a.txt untouched
	badPatch := "@@@ a.txt\n<<<\noldVal\n===\nnewVal\n>>>\n<<<\nnonexistent\n===\nx\n>>>\n"
	out, ok, _ := patch.Run(patch.Options{Cwd: dir, PatchText: badPatch})
	if ok || !strings.Contains(out, "0 files modified") {
		t.Fatalf("expected atomic abort: ok=%v out=%s", ok, out)
	}
	got, _ := os.ReadFile(f1)
	if !strings.Contains(string(got), "oldVal") {
		t.Fatalf("file was modified despite atomic abort: %s", got)
	}

	// Valid multi-op patch + create + inline verify
	goodPatch := "@@@ a.txt\n<<<\noldVal\n===\nnewVal\n>>>\n@@@+ b.txt\nhello\n"
	verifyCmd := "grep -q newVal a.txt && echo verified"
	if runtime.GOOS == "windows" {
		verifyCmd = "findstr /C:newVal a.txt && echo verified"
	}
	out, ok, _ = patch.Run(patch.Options{Cwd: dir, PatchText: goodPatch, VerifyCmd: verifyCmd})
	if !ok || !strings.Contains(out, "Verify [PASS") {
		t.Fatalf("expected patch+verify pass: ok=%v out=%s", ok, out)
	}
	bBytes, err := os.ReadFile(filepath.Join(dir, "b.txt"))
	if err != nil || strings.TrimSpace(string(bBytes)) != "hello" {
		t.Fatalf("expected b.txt created, got %q err=%v", string(bBytes), err)
	}
}

func TestRunnerCompactAndFail(t *testing.T) {
	dir := t.TempDir()
	failCmd := "echo noise; echo 'ERROR: boom happened'; exit 1"
	if runtime.GOOS == "windows" {
		failCmd = "echo noise & echo ERROR: boom happened & exit /b 1"
	}
	out, ok, err := runner.Run(runner.Options{
		Cwd: dir,
		Commands: []runner.CommandSpec{
			{Name: "ok-cmd", Command: "echo all 12 tests passed"},
			{Name: "fail-cmd", Command: failCmd},
		},
		MaxBytes: 4000,
	})
	if err != nil || ok || !strings.Contains(out, "[PASS") || !strings.Contains(out, "ERROR: boom happened") {
		t.Fatalf("unexpected runner out: ok=%v err=%v\n%s", ok, err, out)
	}
}

func TestSessionStartConditionalGate(t *testing.T) {
	// Empty non-repo dir -> must emit {} (0 token overhead)
	nonRepo := t.TempDir()
	nonRepoJSON, _ := json.Marshal(map[string]string{"cwd": nonRepo})
	var buf bytes.Buffer
	hook.RunSessionStart(bytes.NewReader(nonRepoJSON), &buf)
	if strings.TrimSpace(buf.String()) != "{}" {
		t.Fatalf("expected {} for non-repo dir, got %q", buf.String())
	}

	// Repo dir with go.mod -> emits compact <=1200B injectSteps snapshot
	repo := t.TempDir()
	_ = os.WriteFile(filepath.Join(repo, "go.mod"), []byte("module example.com/demo\n"), 0o644)
	repoJSON, _ := json.Marshal(map[string]string{"cwd": repo})
	buf.Reset()
	hook.RunSessionStart(bytes.NewReader(repoJSON), &buf)
	var parsed map[string]any
	if err := json.Unmarshal(buf.Bytes(), &parsed); err != nil {
		t.Fatalf("invalid hook JSON: %v %s", err, buf.String())
	}
	if _, has := parsed["injectSteps"]; !has {
		t.Fatalf("expected injectSteps for repo dir, got %s", buf.String())
	}
}

func TestMetricsRecordingAndStats(t *testing.T) {
	logFile := filepath.Join(t.TempDir(), "m.jsonl")
	t.Setenv("AZERO_LOG_FILE", logFile)

	dir := t.TempDir()
	bigSrc := "package demo\n" + strings.Repeat("// filler comment line for size\n", 400) + "func Target() {}\n"
	_ = os.WriteFile(filepath.Join(dir, "big.go"), []byte(bigSrc), 0o644)

	if _, err := outline.Run(outline.Options{Cwd: dir, Paths: []string{"big.go"}, MaxBytes: 4000}); err != nil {
		t.Fatal(err)
	}
	if _, err := search.Run(search.Options{
		Cwd: dir,
		Queries: []search.Query{
			{Type: search.QueryText, Pattern: "Target", CaseSensitive: true},
			{Type: search.QueryGlob, Pattern: "*.go"},
		},
		MaxBytes: 4000,
	}); err != nil {
		t.Fatal(err)
	}

	events, files := metrics.LoadAll(time.Time{})
	if len(events) < 2 {
		t.Fatalf("expected >=2 recorded events, got %d", len(events))
	}
	summary := metrics.FormatStats(events, files, 5)
	if !strings.Contains(summary, "有效加速触发") || !strings.Contains(summary, "`map`") || !strings.Contains(summary, "`q`") {
		t.Fatalf("unexpected stats output:\n%s", summary)
	}
}

func TestMetricsLoadsPluginDataAndTracePositionalArg(t *testing.T) {
	// Verify LoadAll scans ~/.gemini/antigravity-cli/plugin_data/agy-zero/metrics.jsonl when AZERO_LOG_FILE is unset
	fakeHome := t.TempDir()
	t.Setenv("AZERO_LOG_FILE", "")
	t.Setenv("PLUGIN_DATA", "")
	t.Setenv("HOME", fakeHome)
	t.Setenv("USERPROFILE", fakeHome)

	pdFile := filepath.Join(fakeHome, ".gemini", "antigravity-cli", "plugin_data", "agy-zero", "metrics.jsonl")
	if err := os.MkdirAll(filepath.Dir(pdFile), 0o755); err != nil {
		t.Fatal(err)
	}
	ev := metrics.Event{
		ID:            "plugin-data-ev-1",
		Timestamp:     time.Now().Format(time.RFC3339Nano),
		Command:       "session-skip",
		Workspace:     "demo",
		OK:            true,
	}
	raw, _ := json.Marshal(ev)
	if err := os.WriteFile(pdFile, append(raw, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	events, _ := metrics.LoadAll(time.Time{})
	found := false
	for _, e := range events {
		if e.ID == "plugin-data-ev-1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected LoadAll to read plugin_data metrics, got %+v", events)
	}

	// Verify Record persists directly into existing plugin_data dir when PLUGIN_DATA & AZERO_LOG_FILE are unset
	metrics.Record(metrics.Event{
		ID:            "plugin-data-write-1",
		Command:       "q",
		Workspace:     "demo",
		BaselineCalls: 2,
		ActualCalls:   1,
		OK:            true,
	})
	pdRaw, err := os.ReadFile(pdFile)
	if err != nil || !strings.Contains(string(pdRaw), "plugin-data-write-1") {
		t.Fatalf("expected Record to persist into plugin_data file %s, err=%v content=%s", pdFile, err, string(pdRaw))
	}

	// Verify cmdTrace accepts positional transcript path
	trFile := filepath.Join(t.TempDir(), "transcript.jsonl")
	trLine := `{"step_index":1,"type":"PLANNER_RESPONSE","input_tokens":2243,"cache_read_tokens":0,"output_tokens":12}` + "\n"
	if err := os.WriteFile(trFile, []byte(trLine), 0o644); err != nil {
		t.Fatal(err)
	}
	if code := cmdTrace([]string{trFile}); code != 0 {
		t.Fatalf("expected cmdTrace positional arg exit 0, got %d", code)
	}
}

func TestRegressionBugFixes(t *testing.T) {
	// 1. Strict readonly git remote + branch -vv
	for _, good := range []string{"remote", "remote -v", "remote show origin", "remote get-url origin", "branch -vv"} {
		if _, err := search.ValidateReadonlyGitArgs(good); err != nil {
			t.Fatalf("expected %q allowed, got %v", good, err)
		}
	}
	for _, bad := range []string{"remote remove origin", "remote rm origin", "remote rename origin upstream", "remote set-url origin https://x", "remote prune origin"} {
		if _, err := search.ValidateReadonlyGitArgs(bad); err == nil {
			t.Fatalf("expected %q rejected", bad)
		}
	}

	// 2. Search finds .github & dotfiles while skipping .git
	dir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(dir, ".github", "workflows"), 0o755)
	_ = os.MkdirAll(filepath.Join(dir, ".git"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, ".github", "workflows", "ci.yml"), []byte("name: CI_MARKER\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, ".gitignore"), []byte("CI_MARKER_IGNORE\n"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("CI_MARKER_GIT\n"), 0o644)
	qOut, err := search.Run(search.Options{
		Cwd:     dir,
		Queries: []search.Query{{Type: search.QueryText, Pattern: "CI_MARKER", CaseSensitive: true}},
	})
	if err != nil || !strings.Contains(qOut, ".github/workflows/ci.yml") || !strings.Contains(qOut, ".gitignore") || strings.Contains(qOut, ".git/HEAD") {
		t.Fatalf("unexpected dotfile search result: err=%v\n%s", err, qOut)
	}

	// 3. Outline: TS single-line arrow, 4-space JSON, nested HTML <section>
	tsSrc := "export const add = (a: number, b: number) => a + b;\nexport function realFn() {\n  return 1;\n}\n"
	jsonSrc := "{\n    \"alpha\": {\n        \"nested\": 1\n    },\n    \"beta\": true\n}\n"
	htmlSrc := "<section id=\"outer\">\n  <section id=\"inner\">\n  </section>\n</section>\n"
	_ = os.WriteFile(filepath.Join(dir, "a.ts"), []byte(tsSrc), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "b.json"), []byte(jsonSrc), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "c.html"), []byte(htmlSrc), 0o644)
	mOut, err := outline.Run(outline.Options{Cwd: dir, Paths: []string{"a.ts", "b.json", "c.html"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(mOut, "L1 fn add\n") || !strings.Contains(mOut, "L2-4 fn realFn") {
		t.Fatalf("TS single-line arrow leaked into next fn:\n%s", mOut)
	}
	if !strings.Contains(mOut, "key alpha") || !strings.Contains(mOut, "key beta") {
		t.Fatalf("4-space JSON keys missing:\n%s", mOut)
	}
	if !strings.Contains(mOut, "L1-4 <section> section") {
		t.Fatalf("nested HTML section end line wrong:\n%s", mOut)
	}

	// 4. Patch rejects duplicate/mixed create + hunk on same path; allows indented === inside code
	if _, err := patch.Parse("@@@+ dup.txt\nhi\n@@@ dup.txt\n<<<\nhi\n===\nbye\n>>>\n"); err == nil {
		t.Fatalf("expected error when mixing @@@+ and @@@ on same file")
	}
	indentedFile := filepath.Join(dir, "ind.txt")
	_ = os.WriteFile(indentedFile, []byte("header\n  ===\nfooter\n"), 0o644)
	_, ok, _ := patch.Run(patch.Options{
		Cwd:       dir,
		PatchText: "@@@ ind.txt\n<<<\nheader\n  ===\n===\nHEADER\n  ===\n>>>\n",
	})
	if !ok {
		t.Fatalf("expected indented === inside hunk to succeed")
	}

	// 5. Hook detects parent .git from subdirectory
	sub := filepath.Join(dir, "packages", "web")
	_ = os.MkdirAll(sub, 0o755)
	subJSON, _ := json.Marshal(map[string]string{"cwd": sub})
	var hBuf bytes.Buffer
	hook.RunSessionStart(bytes.NewReader(subJSON), &hBuf)
	if strings.TrimSpace(hBuf.String()) == "{}" {
		t.Fatalf("expected SessionStart snapshot when inside subdirectory of .git repo")
	}

	// 6. Runner ParseCommandArg does not split lowercase env prefixes
	sp1 := runner.ParseCommandArg("http_proxy=http://127.0.0.1:8080 curl https://example.com", 0)
	if sp1.Name != "check-1" || !strings.HasPrefix(sp1.Command, "http_proxy=") {
		t.Fatalf("unexpected parse for http_proxy: %+v", sp1)
	}
	sp2 := runner.ParseCommandArg("debug=1 npm test", 1)
	if sp2.Name != "check-2" || sp2.Command != "debug=1 npm test" {
		t.Fatalf("unexpected parse for debug=1: %+v", sp2)
	}
	sp3 := runner.ParseCommandArg("unit=go test ./...", 2)
	if sp3.Name != "unit" || sp3.Command != "go test ./..." {
		t.Fatalf("unexpected parse for labeled cmd: %+v", sp3)
	}
}
