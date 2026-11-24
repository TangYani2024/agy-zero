package trace

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"agy-zero/internal/budget"
)

type step struct {
	StepIndex       int        `json:"step_index"`
	Type            string     `json:"type"`
	CreatedAt       string     `json:"created_at"`
	InputTokens     int        `json:"input_tokens"`
	CacheReadTokens int        `json:"cache_read_tokens"`
	OutputTokens    int        `json:"output_tokens"`
	Content         string     `json:"content"`
	ToolCalls       []toolCall `json:"tool_calls"`
}

type toolCall struct {
	Name string `json:"name"`
}

type SessionReport struct {
	Path              string
	Turns             int
	ToolCalls         int
	Turn1Input        int
	MaxContext        int
	TotalUncachedIn   int
	TotalCacheRead    int
	TotalOut          int
	CacheMissSpikes   int
	Over60sGaps       int
	SpillCount        int
	ReadFileCalls     int
	MultiToolTurns    int
	BiggestCtxJump    int
	BiggestJumpStep   int
	BiggestJumpTools  string
}

func DiscoverTranscripts() ([]string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}
	patterns := []string{
		filepath.Join(home, ".gemini", "antigravity-cli", "brain", "*", ".system_generated", "logs", "transcript.jsonl"),
		filepath.Join(home, ".gemini", "antigravity", "brain", "*", ".system_generated", "logs", "transcript.jsonl"),
		filepath.Join(home, ".gemini", "antigravity-ide", "brain", "*", ".system_generated", "logs", "transcript.jsonl"),
	}
	var paths []string
	for _, pat := range patterns {
		matches, _ := filepath.Glob(pat)
		paths = append(paths, matches...)
	}
	type item struct {
		path string
		mod  time.Time
	}
	items := make([]item, 0, len(paths))
	for _, p := range paths {
		if st, err := os.Stat(p); err == nil {
			items = append(items, item{p, st.ModTime()})
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].mod.After(items[j].mod) })
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.path
	}
	return out, nil
}

func AnalyzeFile(path string) (SessionReport, error) {
	f, err := os.Open(path)
	if err != nil {
		return SessionReport{}, err
	}
	defer f.Close()

	rep := SessionReport{Path: path}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)

	var prevTime time.Time
	prevCtx := 0
	prevTools := ""
	userBetween := true

	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var s step
		if err := json.Unmarshal([]byte(line), &s); err != nil {
			continue
		}
		if s.Type == "USER_INPUT" {
			userBetween = true
			continue
		}
		if s.Type == "GENERIC" {
			if strings.Contains(s.Content, "output.txt") && (strings.Contains(s.Content, "truncated") || strings.Contains(s.Content, "saved to")) {
				rep.SpillCount++
			}
			continue
		}
		if s.Type != "PLANNER_RESPONSE" {
			continue
		}

		rep.Turns++
		ctx := s.InputTokens + s.CacheReadTokens
		if rep.Turn1Input == 0 && ctx > 0 {
			rep.Turn1Input = ctx
		}
		if ctx > rep.MaxContext {
			rep.MaxContext = ctx
		}
		rep.TotalUncachedIn += s.InputTokens
		rep.TotalCacheRead += s.CacheReadTokens
		rep.TotalOut += s.OutputTokens

		if prevCtx > 0 && ctx-prevCtx > rep.BiggestCtxJump {
			rep.BiggestCtxJump = ctx - prevCtx
			rep.BiggestJumpStep = s.StepIndex
			rep.BiggestJumpTools = prevTools
		}
		if prevCtx >= 25000 && ctx >= 25000 && s.CacheReadTokens == 0 && s.InputTokens > 0 {
			rep.CacheMissSpikes++
		}
		if t, err := time.Parse(time.RFC3339Nano, s.CreatedAt); err == nil {
			if !prevTime.IsZero() && !userBetween {
				gap := t.Sub(prevTime).Seconds()
				if gap > 60 && gap <= 3600 {
					rep.Over60sGaps++
				}
			}
			prevTime = t
		}
		userBetween = false
		if ctx > 0 {
			prevCtx = ctx
		}

		rep.ToolCalls += len(s.ToolCalls)
		if len(s.ToolCalls) >= 2 {
			rep.MultiToolTurns++
		}
		var names []string
		for _, tc := range s.ToolCalls {
			names = append(names, tc.Name)
			if tc.Name == "read_file" {
				rep.ReadFileCalls++
			}
		}
		prevTools = fmt.Sprintf("%d×[%s]", len(names), strings.Join(names[:min(len(names), 3)], ","))
	}
	return rep, nil
}

func FormatReport(reports []SessionReport, maxBytes int) string {
	if len(reports) == 0 {
		return "No local Antigravity transcripts found."
	}
	var b strings.Builder
	if len(reports) == 1 {
		r := reports[0]
		tot := r.TotalUncachedIn + r.TotalCacheRead
		hitPct := 0.0
		if tot > 0 {
			hitPct = float64(r.TotalCacheRead) / float64(tot) * 100
		}
		fmt.Fprintf(&b, "# agy-zero Transcript X-Ray\nFile: %s\n", r.Path)
		fmt.Fprintf(&b, "- Turns: %d | Tool calls: %d (multi-tool turns: %d, read_file: %d)\n", r.Turns, r.ToolCalls, r.MultiToolTurns, r.ReadFileCalls)
		fmt.Fprintf(&b, "- Turn-1 fixed prefix: %d tok | Peak context: %d tok\n", r.Turn1Input, r.MaxContext)
		fmt.Fprintf(&b, "- Tokens: uncached_in=%d, cache_read=%d (avg hit %.1f%%), out=%d\n", r.TotalUncachedIn, r.TotalCacheRead, hitPct, r.TotalOut)
		fmt.Fprintf(&b, "- Bottlenecks: full_cache_misses(>25k)=%d | >60s_gaps=%d | output.txt_spills=%d\n", r.CacheMissSpikes, r.Over60sGaps, r.SpillCount)
		if r.BiggestCtxJump > 0 {
			fmt.Fprintf(&b, "- Biggest single-turn context jump: +%d tok at step #%d (preceded by %s)\n", r.BiggestCtxJump, r.BiggestJumpStep, r.BiggestJumpTools)
		}
		return budget.TruncateUTF8(b.String(), maxBytes)
	}

	var sumTurns, sumCalls, sumSpills, sumMiss, sumGaps, sumUncached, sumCache int
	for _, r := range reports {
		sumTurns += r.Turns
		sumCalls += r.ToolCalls
		sumSpills += r.SpillCount
		sumMiss += r.CacheMissSpikes
		sumGaps += r.Over60sGaps
		sumUncached += r.TotalUncachedIn
		sumCache += r.TotalCacheRead
	}
	hitPct := 0.0
	if sumUncached+sumCache > 0 {
		hitPct = float64(sumCache) / float64(sumUncached+sumCache) * 100
	}
	fmt.Fprintf(&b, "# agy-zero Transcript X-Ray (%d sessions)\n", len(reports))
	fmt.Fprintf(&b, "- Total turns: %d | Tool calls: %d | Cache hit: %.1f%%\n", sumTurns, sumCalls, hitPct)
	fmt.Fprintf(&b, "- Full cache misses (>25k): %d | >60s gaps: %d | output.txt spills: %d\n", sumMiss, sumGaps, sumSpills)
	for i, r := range reports {
		if i >= 10 {
			break
		}
		fmt.Fprintf(&b, "  %d. turns=%d peak=%dk spills=%d misses=%d (%s)\n",
			i+1, r.Turns, r.MaxContext/1000, r.SpillCount, r.CacheMissSpikes, filepath.Base(filepath.Dir(filepath.Dir(filepath.Dir(r.Path)))))
	}
	return budget.TruncateUTF8(b.String(), maxBytes)
}
