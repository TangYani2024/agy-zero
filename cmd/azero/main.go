package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"agy-zero/internal/budget"
	"agy-zero/internal/hook"
	"agy-zero/internal/metrics"
	"agy-zero/internal/outline"
	"agy-zero/internal/patch"
	"agy-zero/internal/runner"
	"agy-zero/internal/search"
	"agy-zero/internal/trace"
)

const version = "0.1.0"

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error {
	*s = append(*s, v)
	return nil
}

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(0)
	}
	switch os.Args[1] {
	case "q", "search":
		os.Exit(cmdSearch(os.Args[2:]))
	case "map", "outline":
		os.Exit(cmdOutline(os.Args[2:]))
	case "patch":
		os.Exit(cmdPatch(os.Args[2:]))
	case "run", "check":
		os.Exit(cmdRun(os.Args[2:]))
	case "trace":
		os.Exit(cmdTrace(os.Args[2:]))
	case "stats", "log", "report":
		os.Exit(cmdStats(os.Args[2:]))
	case "hook":
		os.Exit(cmdHook(os.Args[2:]))
	case "version", "--version", "-v":
		fmt.Println("agy-zero " + version)
	case "help", "-h", "--help":
		printUsage()
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand %q (run 'azero --help')\n", os.Args[1])
		os.Exit(2)
	}
}

func printUsage() {
	fmt.Print(`agy-zero (azero) - Zero-MCP, update-proof Antigravity CLI accelerator

Commands:
  q, search    Batched concurrent text/regex/glob/fuzzy/read-only git search
  map, outline Multi-file symbol outline extractor (real Go AST + multi-lang)
  patch        Atomic multi-file SEARCH/REPLACE heredoc patcher + optional --verify
  run, check   Concurrent shell check runner with compact head-tail error folding
  trace        Local Antigravity transcript.jsonl X-ray (tokens, cache, spills)
  stats, log   Show trigger counts, before-vs-after baseline, and total savings
  hook         Lifecycle hook handlers (session-start | post-tool)
  version      Print version

Global env:
  AZERO_MAX_BYTES Output UTF-8 byte cap (default 7600, safely under 8192 B stdout cap)
  AZERO_LOG_FILE  Override local metrics JSONL path (set AZERO_NO_LOG=1 to disable)
`)
}

func cmdSearch(args []string) int {
	fs := flag.NewFlagSet("q", flag.ContinueOnError)
	cwd := fs.String("C", "", "workspace root directory")
	ignoreCase := fs.Bool("i", false, "case-insensitive search")
	extsRaw := fs.String("ext", "", "comma-separated extensions (e.g. go,ts,py)")
	filesOnly := fs.Bool("files", false, "list matching file paths only")
	countOnly := fs.Bool("count", false, "show match counts per file")
	maxRes := fs.Int("m", 30, "max results per query")
	maxBytes := fs.Int("max-bytes", budget.DefaultMaxBytes(), "max UTF-8 output bytes")

	var texts, regexes, globs, fuzzies, gits stringList
	fs.Var(&texts, "t", "literal text query (repeatable; optional @subdir: prefix)")
	fs.Var(&regexes, "r", "regex query (repeatable; optional @subdir: prefix)")
	fs.Var(&globs, "g", "glob query (repeatable; optional @subdir: prefix)")
	fs.Var(&fuzzies, "f", "fuzzy path query (repeatable; optional @subdir: prefix)")
	fs.Var(&gits, "git", "read-only git query (repeatable; e.g. 'status --short')")

	if err := fs.Parse(args); err != nil {
		return 2
	}
	var exts []string
	if strings.TrimSpace(*extsRaw) != "" {
		exts = strings.Split(*extsRaw, ",")
	}
	mode := search.ModeContent
	if *filesOnly {
		mode = search.ModeFiles
	} else if *countOnly {
		mode = search.ModeCount
	}

	var queries []search.Query
	add := func(qt search.QueryType, list []string) {
		for _, raw := range list {
			sub, pat := search.ParseScopedPattern(raw)
			queries = append(queries, search.Query{
				Type:          qt,
				Pattern:       pat,
				SubPath:       sub,
				CaseSensitive: !*ignoreCase,
				Exts:          exts,
				Mode:          mode,
				MaxResults:    *maxRes,
			})
		}
	}
	add(search.QueryText, texts)
	add(search.QueryRegex, regexes)
	add(search.QueryGlob, globs)
	add(search.QueryFuzzy, fuzzies)
	add(search.QueryGit, gits)

	out, err := search.Run(search.Options{
		Cwd:      *cwd,
		Queries:  queries,
		MaxBytes: *maxBytes,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	fmt.Println(out)
	return 0
}

func cmdOutline(args []string) int {
	fs := flag.NewFlagSet("map", flag.ContinueOnError)
	cwd := fs.String("C", "", "workspace root directory")
	maxBytes := fs.Int("max-bytes", budget.DefaultMaxBytes(), "max UTF-8 output bytes")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	out, err := outline.Run(outline.Options{
		Cwd:      *cwd,
		Paths:    fs.Args(),
		MaxBytes: *maxBytes,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	fmt.Println(out)
	return 0
}

func cmdPatch(args []string) int {
	fs := flag.NewFlagSet("patch", flag.ContinueOnError)
	cwd := fs.String("C", "", "workspace root directory")
	filePath := fs.String("file", "", "read patch from file instead of stdin")
	dryRun := fs.Bool("dry-run", false, "validate without writing files")
	bestEffort := fs.Bool("best-effort", false, "commit valid files even if others fail")
	verifyCmd := fs.String("verify", "", "optional verification shell command after commit")
	verifyTimeout := fs.Int("verify-timeout", 60, "verify command timeout seconds")
	maxBytes := fs.Int("max-bytes", budget.DefaultMaxBytes(), "max UTF-8 output bytes")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	var raw []byte
	var err error
	if *filePath != "" {
		raw, err = os.ReadFile(*filePath)
	} else {
		raw, err = io.ReadAll(os.Stdin)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error reading patch:", err)
		return 1
	}
	out, ok, err := patch.Run(patch.Options{
		Cwd:              *cwd,
		PatchText:        string(raw),
		DryRun:           *dryRun,
		BestEffort:       *bestEffort,
		VerifyCmd:        *verifyCmd,
		VerifyTimeoutSec: *verifyTimeout,
		MaxBytes:         *maxBytes,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	fmt.Println(out)
	if !ok {
		return 1
	}
	return 0
}

func cmdRun(args []string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	cwd := fs.String("C", "", "workspace root directory")
	conc := fs.Int("j", 4, "max concurrent commands (1..8)")
	timeoutSec := fs.Int("timeout", 60, "per-command timeout seconds")
	full := fs.Bool("full", false, "preserve stdout of passing commands within byte cap")
	maxBytes := fs.Int("max-bytes", budget.DefaultMaxBytes(), "max UTF-8 output bytes")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	rawCmds := fs.Args()
	specs := make([]runner.CommandSpec, len(rawCmds))
	for i, c := range rawCmds {
		specs[i] = runner.ParseCommandArg(c, i)
	}
	out, ok, err := runner.Run(runner.Options{
		Cwd:         *cwd,
		Commands:    specs,
		Concurrency: *conc,
		TimeoutSec:  *timeoutSec,
		Full:        *full,
		MaxBytes:    *maxBytes,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 1
	}
	fmt.Println(out)
	if !ok {
		return 1
	}
	return 0
}

func cmdTrace(args []string) int {
	fs := flag.NewFlagSet("trace", flag.ContinueOnError)
	filePath := fs.String("file", "", "specific transcript.jsonl path")
	latest := fs.Bool("latest", false, "analyze most recently modified local transcript")
	all := fs.Bool("all", false, "aggregate recent local transcripts")
	recent := fs.Int("recent", 10, "max sessions for --all")
	maxBytes := fs.Int("max-bytes", budget.DefaultMaxBytes(), "max UTF-8 output bytes")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	var targets []string
	if *filePath != "" {
		targets = append(targets, *filePath)
	}
	if len(fs.Args()) > 0 {
		targets = append(targets, fs.Args()...)
	}
	if len(targets) == 0 {
		found, err := trace.DiscoverTranscripts()
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			return 1
		}
		if len(found) == 0 {
			fmt.Println("No local Antigravity transcripts found.")
			return 0
		}
		if *all && !*latest {
			n := min(len(found), max(1, *recent))
			targets = found[:n]
		} else {
			targets = found[:1]
		}
	}
	var reports []trace.SessionReport
	for _, p := range targets {
		if r, err := trace.AnalyzeFile(p); err == nil && r.Turns > 0 {
			reports = append(reports, r)
		}
	}
	fmt.Println(trace.FormatReport(reports, *maxBytes))
	return 0
}

func parseSinceArg(raw string) (time.Time, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, nil
	}
	if strings.HasSuffix(raw, "d") {
		if days, err := strconv.Atoi(strings.TrimSuffix(raw, "d")); err == nil && days > 0 {
			return time.Now().Add(-time.Duration(days) * 24 * time.Hour), nil
		}
	}
	if d, err := time.ParseDuration(raw); err == nil {
		return time.Now().Add(-d), nil
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02"} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid --since value %q (try 24h, 7d, or 2026-11-24)", raw)
}

func cmdStats(args []string) int {
	fs := flag.NewFlagSet("stats", flag.ContinueOnError)
	recent := fs.Int("recent", 5, "number of recent trigger records to display")
	sinceRaw := fs.String("since", "", "time filter (e.g. 24h, 7d, 2026-11-24)")
	asJSON := fs.Bool("json", false, "output raw events as JSON")
	reset := fs.Bool("reset", false, "clear local metrics history")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *reset {
		n := metrics.Reset()
		fmt.Printf("Cleared %d metrics log file(s).\n", n)
		return 0
	}
	since, err := parseSinceArg(*sinceRaw)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 2
	}
	events, files := metrics.LoadAll(since)
	if *asJSON {
		enc, _ := json.MarshalIndent(events, "", "  ")
		fmt.Println(string(enc))
		return 0
	}
	fmt.Print(metrics.FormatStats(events, files, *recent))
	return 0
}

func cmdHook(args []string) int {
	if len(args) == 0 {
		fmt.Println("{}")
		return 0
	}
	switch args[0] {
	case "session-start":
		hook.RunSessionStart(os.Stdin, os.Stdout)
		return 0
	case "post-tool":
		hook.RunPostTool(os.Stdin, os.Stdout)
		return 0
	default:
		fmt.Println("{}")
		return 0
	}
}
