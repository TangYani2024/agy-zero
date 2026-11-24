package search

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
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
)

type QueryType string

const (
	QueryText  QueryType = "text"
	QueryRegex QueryType = "regex"
	QueryGlob  QueryType = "glob"
	QueryFuzzy QueryType = "fuzzy"
	QueryGit   QueryType = "git"
)

type OutputMode string

const (
	ModeContent OutputMode = "content"
	ModeFiles   OutputMode = "files"
	ModeCount   OutputMode = "count"
)

type Query struct {
	Type          QueryType
	Pattern       string
	SubPath       string
	CaseSensitive bool
	Exts          []string
	Mode          OutputMode
	MaxResults    int
}

type Options struct {
	Cwd      string
	Queries  []Query
	MaxBytes int
}

var DefaultIgnoreDirs = map[string]bool{
	".git": true, ".hg": true, ".svn": true,
	"node_modules": true, "dist": true, "build": true, "out": true, "target": true,
	".next": true, ".nuxt": true, ".svelte-kit": true, ".cache": true, ".turbo": true,
	"coverage": true, "__pycache__": true, ".venv": true, "venv": true,
	".pytest_cache": true, ".mypy_cache": true, ".ruff_cache": true,
	".gradle": true, ".idea": true, "vendor": true,
}

var readonlyGitSubcmds = map[string]bool{
	"status": true, "diff": true, "log": true, "show": true,
	"branch": true, "rev-parse": true, "ls-files": true, "blame": true,
	"shortlog": true, "describe": true, "tag": true, "remote": true,
}

var forbiddenGitFlags = map[string]bool{
	"-c": true, "--config": true, "--config-env": true, "--exec-path": true,
	"--ext-diff": true, "--textconv": true, "--output": true, "--exec": true,
	"--upload-pack": true, "--git-dir": true, "--work-tree": true,
}

var hardenedGitPrefix = []string{
	"--no-optional-locks",
	"-c", "core.fsmonitor=false",
	"-c", "diff.external=",
	"-c", "gpg.program=false",
	"--no-pager",
}

// ParseScopedPattern splits optional `@subpath:pattern` prefix.
func ParseScopedPattern(raw string) (subpath, pattern string) {
	if strings.HasPrefix(raw, "@") {
		if colon := strings.IndexByte(raw, ':'); colon > 1 {
			cand := raw[1:colon]
			if !strings.ContainsAny(cand, " \t\n") {
				return cand, raw[colon+1:]
			}
		}
	}
	return "", raw
}

func Run(opts Options) (string, error) {
	if len(opts.Queries) == 0 {
		return "", errors.New("at least one query is required (-t, -r, -g, -f, or --git)")
	}
	if len(opts.Queries) > 20 {
		return "", errors.New("at most 20 queries per call")
	}
	cwd := opts.Cwd
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return "", err
		}
	}
	absCwd, err := filepath.Abs(cwd)
	if err != nil {
		return "", err
	}
	maxBytes := opts.MaxBytes
	if maxBytes <= 0 {
		maxBytes = budget.DefaultMaxBytes()
	}

	wallStart := time.Now()
	sections := make([]string, len(opts.Queries))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 6)
	for i, q := range opts.Queries {
		wg.Add(1)
		go func(idx int, query Query) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			sections[idx] = runSingle(absCwd, idx+1, query)
		}(i, q)
	}
	wg.Wait()

	rawJoined := strings.Join(sections, "\n\n")
	out := budget.FitSections(sections, maxBytes, "\n\n", false)
	spills := 0
	if len(rawJoined) > 8192 {
		spills = 1
	}
	metrics.Record(metrics.Event{
		Command:       "q",
		Workspace:     filepath.Base(absCwd),
		Detail:        fmt.Sprintf("%d queries", len(opts.Queries)),
		BaselineCalls: len(opts.Queries),
		ActualCalls:   1,
		RawBytes:      len(rawJoined),
		OutBytes:      len(out),
		SpillsAvoided: spills,
		EstTurnsSaved: spills + max(0, (len(opts.Queries)-1)/2),
		WallMs:        time.Since(wallStart).Milliseconds(),
		OK:            true,
	})
	return out, nil
}

func runSingle(baseCwd string, num int, q Query) string {
	root := baseCwd
	if q.SubPath != "" {
		if filepath.IsAbs(q.SubPath) {
			root = filepath.Clean(q.SubPath)
		} else {
			root = filepath.Join(baseCwd, q.SubPath)
		}
	}
	header := fmt.Sprintf("## Q%d [%s] %q", num, q.Type, q.Pattern)
	if q.SubPath != "" {
		header += fmt.Sprintf(" (in %s)", q.SubPath)
	}
	if q.MaxResults <= 0 {
		q.MaxResults = 30
	}
	if q.MaxResults > 200 {
		q.MaxResults = 200
	}

	switch q.Type {
	case QueryGit:
		out, err := runGit(root, q.Pattern)
		if err != nil {
			return header + "\nERROR: " + err.Error()
		}
		if strings.TrimSpace(out) == "" {
			return header + "\n(no output)"
		}
		return header + "\n" + strings.TrimRight(out, "\n")
	case QueryGlob:
		out, err := runGlob(baseCwd, root, q)
		if err != nil {
			return header + "\nERROR: " + err.Error()
		}
		return header + "\n" + out
	case QueryFuzzy:
		out, err := runFuzzy(baseCwd, root, q)
		if err != nil {
			return header + "\nERROR: " + err.Error()
		}
		return header + "\n" + out
	case QueryText, QueryRegex:
		out, err := runContentSearch(baseCwd, root, q)
		if err != nil {
			return header + "\nERROR: " + err.Error()
		}
		return header + "\n" + out
	default:
		return header + "\nERROR: unsupported query type"
	}
}

func walkRepoFiles(root string, exts []string, maxFiles int) ([]string, bool, error) {
	st, err := os.Stat(root)
	if err != nil {
		return nil, false, err
	}
	if !st.IsDir() {
		return []string{root}, false, nil
	}
	extSet := make(map[string]bool, len(exts))
	for _, e := range exts {
		e = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(e), "."))
		if e != "" {
			extSet["."+e] = true
		}
	}
	var files []string
	truncated := false
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			if p != root {
				if DefaultIgnoreDirs[name] || (strings.HasPrefix(name, ".") && name != ".agents") {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if strings.HasPrefix(name, ".") {
			return nil
		}
		if len(extSet) > 0 && !extSet[strings.ToLower(filepath.Ext(name))] {
			return nil
		}
		files = append(files, p)
		if len(files) >= maxFiles {
			truncated = true
			return fs.SkipAll
		}
		return nil
	})
	if err != nil && !errors.Is(err, fs.SkipAll) {
		return nil, false, err
	}
	sort.Strings(files)
	return files, truncated, nil
}

func relDisplay(base, abs string) string {
	if rel, err := filepath.Rel(base, abs); err == nil && !strings.HasPrefix(rel, "..") {
		return filepath.ToSlash(rel)
	}
	return filepath.ToSlash(abs)
}

func runContentSearch(baseCwd, root string, q Query) (string, error) {
	if q.Pattern == "" {
		return "", errors.New("empty pattern")
	}
	if q.Mode == "" {
		q.Mode = ModeContent
	}
	var re *regexp.Regexp
	var needleLower []byte
	var needleExact []byte
	if q.Type == QueryRegex {
		pat := q.Pattern
		if !q.CaseSensitive {
			pat = "(?i)" + pat
		}
		var err error
		re, err = regexp.Compile(pat)
		if err != nil {
			return "", fmt.Errorf("invalid regex: %w", err)
		}
	} else {
		needleExact = []byte(q.Pattern)
		if !q.CaseSensitive {
			needleLower = bytes.ToLower(needleExact)
		}
	}

	files, walkTrunc, err := walkRepoFiles(root, q.Exts, 8000)
	if err != nil {
		return "", err
	}

	type matchLine struct {
		lineNo int
		text   string
	}
	type fileHit struct {
		rel   string
		count int
		lines []matchLine
	}

	results := make([]*fileHit, len(files))
	var wg sync.WaitGroup
	workers := min(8, max(1, len(files)))
	ch := make(chan int, len(files))
	for i := range files {
		ch <- i
	}
	close(ch)

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range ch {
				fp := files[idx]
				info, err := os.Stat(fp)
				if err != nil || info.Size() > 2*1024*1024 || info.Size() == 0 {
					continue
				}
				raw, err := os.ReadFile(fp)
				if err != nil || isBinary(raw) {
					continue
				}
				// Fast pre-check for literal text
				if re == nil {
					if q.CaseSensitive {
						if !bytes.Contains(raw, needleExact) {
							continue
						}
					} else {
						if !bytes.Contains(bytes.ToLower(raw), needleLower) {
							continue
						}
					}
				} else if !re.Match(raw) {
					continue
				}

				hit := &fileHit{rel: relDisplay(baseCwd, fp)}
				sc := bufio.NewScanner(bytes.NewReader(raw))
				sc.Buffer(make([]byte, 64*1024), 2*1024*1024)
				lineNo := 0
				for sc.Scan() {
					lineNo++
					lb := sc.Bytes()
					matched := false
					if re != nil {
						matched = re.Match(lb)
					} else if q.CaseSensitive {
						matched = bytes.Contains(lb, needleExact)
					} else {
						matched = bytes.Contains(bytes.ToLower(lb), needleLower)
					}
					if matched {
						hit.count++
						if q.Mode == ModeContent && len(hit.lines) < 25 {
							txt := strings.TrimRight(string(lb), "\r")
							if len(txt) > 180 {
								txt = budget.SliceUTF8Head(txt, 177) + "..."
							}
							hit.lines = append(hit.lines, matchLine{lineNo: lineNo, text: txt})
						}
					}
				}
				if hit.count > 0 {
					results[idx] = hit
				}
			}
		}()
	}
	wg.Wait()

	var hits []*fileHit
	for _, h := range results {
		if h != nil {
			hits = append(hits, h)
		}
	}
	if len(hits) == 0 {
		return "(0 matches)", nil
	}

	var b strings.Builder
	shown := 0
	truncated := walkTrunc
	switch q.Mode {
	case ModeFiles:
		for _, h := range hits {
			if shown >= q.MaxResults {
				truncated = true
				break
			}
			b.WriteString(h.rel)
			b.WriteByte('\n')
			shown++
		}
	case ModeCount:
		for _, h := range hits {
			if shown >= q.MaxResults {
				truncated = true
				break
			}
			fmt.Fprintf(&b, "%s: %d\n", h.rel, h.count)
			shown++
		}
	default:
		for _, h := range hits {
			if shown >= q.MaxResults {
				truncated = true
				break
			}
			for _, ml := range h.lines {
				if shown >= q.MaxResults {
					truncated = true
					break
				}
				fmt.Fprintf(&b, "%s:%d: %s\n", h.rel, ml.lineNo, ml.text)
				shown++
			}
		}
	}
	if truncated {
		fmt.Fprintf(&b, "... [truncated; showing %d results across %d matching files]", shown, len(hits))
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

func isBinary(data []byte) bool {
	n := min(len(data), 4096)
	return bytes.IndexByte(data[:n], 0) >= 0
}

func globToRegex(pat string) (*regexp.Regexp, error) {
	pat = strings.TrimPrefix(filepath.ToSlash(strings.TrimSpace(pat)), "./")
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(pat); i++ {
		switch pat[i] {
		case '*':
			if i+1 < len(pat) && pat[i+1] == '*' {
				if i+2 < len(pat) && pat[i+2] == '/' {
					b.WriteString("(?:.*/)?")
					i += 2
				} else {
					b.WriteString(".*")
					i++
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		case '.', '+', '^', '$', '(', ')', '|', '[', ']', '{', '}', '\\':
			b.WriteByte('\\')
			b.WriteByte(pat[i])
		default:
			b.WriteByte(pat[i])
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}

func runGlob(baseCwd, root string, q Query) (string, error) {
	re, err := globToRegex(q.Pattern)
	if err != nil {
		return "", err
	}
	files, walkTrunc, err := walkRepoFiles(root, q.Exts, 8000)
	if err != nil {
		return "", err
	}
	var matched []string
	for _, fp := range files {
		rel := relDisplay(baseCwd, fp)
		if re.MatchString(rel) || re.MatchString(filepath.Base(rel)) {
			matched = append(matched, rel)
		}
	}
	if len(matched) == 0 {
		return "(0 files)", nil
	}
	truncated := walkTrunc
	if len(matched) > q.MaxResults {
		matched = matched[:q.MaxResults]
		truncated = true
	}
	out := strings.Join(matched, "\n")
	if truncated {
		out += fmt.Sprintf("\n... [truncated at %d files]", q.MaxResults)
	}
	return out, nil
}

func fuzzySubseq(haystack, needle string) bool {
	j := 0
	for i := 0; i < len(haystack) && j < len(needle); i++ {
		if haystack[i] == needle[j] {
			j++
		}
	}
	return j == len(needle)
}

func runFuzzy(baseCwd, root string, q Query) (string, error) {
	tokens := strings.Fields(strings.ToLower(q.Pattern))
	if len(tokens) == 0 {
		return "", errors.New("empty fuzzy query")
	}
	files, _, err := walkRepoFiles(root, q.Exts, 8000)
	if err != nil {
		return "", err
	}
	type scored struct {
		rel   string
		score int
	}
	var hits []scored
	for _, fp := range files {
		rel := relDisplay(baseCwd, fp)
		low := strings.ToLower(rel)
		base := strings.ToLower(filepath.Base(rel))
		ok := true
		sc := 0
		for _, tok := range tokens {
			if strings.Contains(base, tok) {
				sc += 20
			} else if strings.Contains(low, tok) {
				sc += 10
			} else if fuzzySubseq(base, tok) {
				sc += 6
			} else if fuzzySubseq(low, tok) {
				sc += 3
			} else {
				ok = false
				break
			}
		}
		if ok {
			sc -= len(rel) / 16
			hits = append(hits, scored{rel: rel, score: sc})
		}
	}
	if len(hits) == 0 {
		return "(0 matches)", nil
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		return hits[i].rel < hits[j].rel
	})
	limit := min(len(hits), q.MaxResults)
	lines := make([]string, limit)
	for i := 0; i < limit; i++ {
		lines[i] = hits[i].rel
	}
	out := strings.Join(lines, "\n")
	if len(hits) > limit {
		out += fmt.Sprintf("\n... [showing top %d of %d]", limit, len(hits))
	}
	return out, nil
}

func tokenizeArgs(input string) ([]string, error) {
	var tokens []string
	var cur strings.Builder
	var quote byte
	escaped := false
	for i := 0; i < len(input); i++ {
		ch := input[i]
		if escaped {
			cur.WriteByte(ch)
			escaped = false
			continue
		}
		if ch == '\\' && quote != '\'' {
			escaped = true
			continue
		}
		if quote != 0 {
			if ch == quote {
				quote = 0
			} else {
				cur.WriteByte(ch)
			}
			continue
		}
		if ch == '\'' || ch == '"' {
			quote = ch
			continue
		}
		if ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' {
			if cur.Len() > 0 {
				tokens = append(tokens, cur.String())
				cur.Reset()
			}
			continue
		}
		cur.WriteByte(ch)
	}
	if quote != 0 {
		return nil, errors.New("unterminated quote in git query")
	}
	if cur.Len() > 0 {
		tokens = append(tokens, cur.String())
	}
	return tokens, nil
}

func ValidateReadonlyGitArgs(pattern string) ([]string, error) {
	tokens, err := tokenizeArgs(strings.TrimSpace(pattern))
	if err != nil {
		return nil, err
	}
	if len(tokens) > 0 && tokens[0] == "git" {
		tokens = tokens[1:]
	}
	if len(tokens) == 0 {
		return nil, errors.New("empty git query")
	}
	subcmd := tokens[0]
	if strings.HasPrefix(subcmd, "-") || !readonlyGitSubcmds[subcmd] {
		return nil, fmt.Errorf("git subcommand %q is not allowed (read-only only)", subcmd)
	}
	rest := tokens[1:]
	for _, arg := range rest {
		if arg == "&&" || arg == "||" || arg == ";" || arg == "|" {
			return nil, fmt.Errorf("shell operator %q not allowed in git query", arg)
		}
		flag := arg
		if eq := strings.IndexByte(arg, '='); eq >= 0 {
			flag = arg[:eq]
		}
		if forbiddenGitFlags[flag] || strings.HasPrefix(flag, "--ext") || strings.HasPrefix(flag, "--textc") || (strings.HasPrefix(arg, "-c") && len(arg) > 2) {
			return nil, fmt.Errorf("flag %q forbidden in read-only git query", arg)
		}
	}
	switch subcmd {
	case "status":
		for _, arg := range rest {
			if arg == "--" {
				break
			}
			if strings.HasPrefix(arg, "--v") || (strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "--") && strings.ContainsRune(arg[1:], 'v')) {
				return nil, errors.New("git status -v/--verbose forbidden in read-only mode")
			}
		}
	case "branch":
		listMode := false
		for _, arg := range rest {
			if arg == "--list" || arg == "-l" || arg == "--show-current" || arg == "-a" || arg == "--all" || arg == "-r" || arg == "--remotes" || arg == "-v" || arg == "--verbose" {
				if arg == "--list" || arg == "-l" {
					listMode = true
				}
				continue
			}
			if strings.HasPrefix(arg, "-") {
				return nil, fmt.Errorf("flag %q not allowed in read-only git branch", arg)
			}
			if !listMode {
				return nil, fmt.Errorf("positional arg %q requires --list in read-only git branch", arg)
			}
		}
	case "tag":
		listMode := false
		for _, arg := range rest {
			if arg == "--list" || arg == "-l" {
				listMode = true
				continue
			}
			if strings.HasPrefix(arg, "-") && arg != "--sort" && !strings.HasPrefix(arg, "--sort=") {
				return nil, fmt.Errorf("flag %q not allowed in read-only git tag", arg)
			}
			if !strings.HasPrefix(arg, "-") && !listMode {
				return nil, fmt.Errorf("positional arg %q requires --list in read-only git tag", arg)
			}
		}
	case "remote":
		for _, arg := range rest {
			if arg != "-v" && arg != "--verbose" && arg != "show" && arg != "get-url" && arg != "-n" && arg != "--push" && arg != "--all" {
				if strings.HasPrefix(arg, "-") || strings.ContainsAny(arg, ":/\\") {
					return nil, fmt.Errorf("arg %q not allowed in read-only git remote", arg)
				}
			}
		}
	}
	return append(append([]string{}, hardenedGitPrefix...), tokens...), nil
}

func runGit(cwd, pattern string) (string, error) {
	args, err := ValidateReadonlyGitArgs(pattern)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = cwd
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err = cmd.Run()
	if err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", errors.New(msg)
		}
		return "", err
	}
	return budget.NormalizeTerminal(stdout.String()), nil
}
