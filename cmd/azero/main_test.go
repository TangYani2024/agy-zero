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
