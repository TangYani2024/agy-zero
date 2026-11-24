package metrics

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Event captures a single auditable agy-zero invocation with concrete Before vs After facts.
type Event struct {
	ID             string `json:"id"`
	Timestamp      string `json:"ts"`
	Command        string `json:"cmd"`               // q | map | patch | run | session-start | session-skip
	Workspace      string `json:"ws,omitempty"`      // basename of cwd
	Detail         string `json:"detail,omitempty"`  // concise summary of what ran
	BaselineCalls  int    `json:"base_calls"`        // calls needed originally
	ActualCalls    int    `json:"actual_calls"`      // actual calls made (1 for CLI, 0 for hook)
	RawBytes       int    `json:"raw_bytes"`         // original uncompressed/full bytes
	OutBytes       int    `json:"out_bytes"`         // actual bytes sent to context
	SpillsAvoided  int    `json:"spills_avoided"`    // >8192B spills prevented
	EstTurnsSaved  int    `json:"est_turns_saved"`   // conservative LLM turns avoided
	WallMs         int64  `json:"wall_ms"`           // actual wall time
	SerialMs       int64  `json:"serial_ms"`         // sum of individual command durations (for concurrent run)
	OK             bool   `json:"ok"`
}

func candidateWritePaths() []string {
	if custom := strings.TrimSpace(os.Getenv("AZERO_LOG_FILE")); custom != "" {
		return []string{filepath.Clean(custom)}
	}
	var paths []string
	if pd := strings.TrimSpace(os.Getenv("PLUGIN_DATA")); pd != "" {
		paths = append(paths, filepath.Join(pd, "metrics.jsonl"))
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		// Prefer existing Antigravity plugin_data dirs (mounted rw even inside the agy Linux sandbox,
		// so CLI stats persist across reboots and share the same store as SessionStart hooks).
		for _, appDir := range []string{"antigravity-cli", "antigravity", "antigravity-ide"} {
			pdRoot := filepath.Join(home, ".gemini", appDir, "plugin_data")
			if st, err := os.Stat(pdRoot); err == nil && st.IsDir() {
				paths = append(paths, filepath.Join(pdRoot, "agy-zero", "metrics.jsonl"))
			}
		}
		paths = append(paths, filepath.Join(home, ".gemini", "cache", "agy-zero", "metrics.jsonl"))
	}
	paths = append(paths, filepath.Join(os.TempDir(), "agy-zero-metrics.jsonl"))
	return dedupStrings(paths)
}

func candidateReadPaths() []string {
	if custom := strings.TrimSpace(os.Getenv("AZERO_LOG_FILE")); custom != "" {
		return []string{filepath.Clean(custom)}
	}
	paths := candidateWritePaths()
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		for _, appDir := range []string{"antigravity-cli", "antigravity", "antigravity-ide"} {
			paths = append(paths, filepath.Join(home, ".gemini", appDir, "plugin_data", "agy-zero", "metrics.jsonl"))
		}
	}
	return dedupStrings(paths)
}

func dedupStrings(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		c := filepath.Clean(s)
		if !seen[c] {
			seen[c] = true
			out = append(out, c)
		}
	}
	return out
}

// Record appends one JSONL metrics line. Strictly fail-open (never breaks CLI or hooks).
func Record(ev Event) {
	if os.Getenv("AZERO_NO_LOG") == "1" {
		return
	}
	now := time.Now()
	if ev.Timestamp == "" {
		ev.Timestamp = now.Format(time.RFC3339Nano)
	}
	if ev.ID == "" {
		ev.ID = fmt.Sprintf("%d-%d", now.UnixNano(), os.Getpid())
	}
	line, err := json.Marshal(ev)
	if err != nil {
		return
	}
	line = append(line, '\n')

	for _, p := range candidateWritePaths() {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			continue
		}
		f, err := os.OpenFile(p, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			continue
		}
		_, werr := f.Write(line)
		_ = f.Close()
		if werr == nil {
			return
		}
	}
}

// LoadAll reads and deduplicates events across all writable metric store paths.
func LoadAll(since time.Time) ([]Event, []string) {
	seenID := map[string]bool{}
	var events []Event
	var usedFiles []string

	for _, p := range candidateReadPaths() {
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		usedFiles = append(usedFiles, p)
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 32*1024), 2*1024*1024)
		for sc.Scan() {
			raw := strings.TrimSpace(sc.Text())
			if raw == "" {
				continue
			}
			var ev Event
			if err := json.Unmarshal([]byte(raw), &ev); err != nil {
				continue
			}
			if ev.ID != "" && seenID[ev.ID] {
				continue
			}
			if !since.IsZero() {
				if ts, err := time.Parse(time.RFC3339Nano, ev.Timestamp); err == nil && ts.Before(since) {
					continue
				}
			}
			if ev.ID != "" {
				seenID[ev.ID] = true
			}
			events = append(events, ev)
		}
		_ = f.Close()
	}

	sort.Slice(events, func(i, j int) bool {
		return events[i].Timestamp < events[j].Timestamp
	})
	return events, usedFiles
}

// Reset removes all metrics files across candidate paths.
func Reset() int {
	removed := 0
	for _, p := range candidateReadPaths() {
		if err := os.Remove(p); err == nil {
			removed++
		}
	}
	return removed
}

type CmdBucket struct {
	Count         int
	BaseCalls     int
	ActualCalls   int
	RawBytes      int
	OutBytes      int
	SpillsAvoided int
	TurnsSaved    int
	ConcSavedMs   int64
}

func FormatStats(events []Event, files []string, recentN int) string {
	if len(events) == 0 {
		return "agy-zero stats: 暂无触发记录（运行 azero q/map/patch/run 或触发 SessionStart 后将自动记录）。"
	}

	order := []string{"q", "map", "patch", "run", "session-start", "session-skip"}
	buckets := make(map[string]*CmdBucket, len(order))
	for _, k := range order {
		buckets[k] = &CmdBucket{}
	}

	var totTriggers, totBaseCalls, totActualCalls, totRawBytes, totOutBytes, totSpills, totTurns int
	var totConcSavedMs int64
	skipCount := 0

	for _, ev := range events {
		b, ok := buckets[ev.Command]
		if !ok {
			b = &CmdBucket{}
			buckets[ev.Command] = b
			order = append(order, ev.Command)
		}
		b.Count++
		if ev.Command == "session-skip" {
			skipCount++
			continue
		}
		totTriggers++
		b.BaseCalls += ev.BaselineCalls
		b.ActualCalls += ev.ActualCalls
		b.RawBytes += ev.RawBytes
		b.OutBytes += ev.OutBytes
		b.SpillsAvoided += ev.SpillsAvoided
		b.TurnsSaved += ev.EstTurnsSaved

		totBaseCalls += ev.BaselineCalls
		totActualCalls += ev.ActualCalls
		totRawBytes += ev.RawBytes
		totOutBytes += ev.OutBytes
		totSpills += ev.SpillsAvoided
		totTurns += ev.EstTurnsSaved
		if ev.SerialMs > ev.WallMs {
			saved := ev.SerialMs - ev.WallMs
			b.ConcSavedMs += saved
			totConcSavedMs += saved
		}
	}

	savedCalls := max(0, totBaseCalls-totActualCalls)
	savedBytes := max(0, totRawBytes-totOutBytes)
	estSavedTok := savedBytes / 4
	// Conservative latency estimate: 1.6s/saved serial tool check + 7.0s/saved LLM turn + real concurrency time
	estSavedSec := float64(savedCalls)*1.6 + float64(totTurns)*7.0 + float64(totConcSavedMs)/1000.0

	var sb strings.Builder
	sb.WriteString("# agy-zero 效果与节省统计\n\n")
	fmt.Fprintf(&sb, "- **有效加速触发**：**%d 次**（另有 **%d 次**非代码目录 SessionStart 跳过，保持 0 Token 开销）\n", totTriggers, skipCount)
	fmt.Fprintf(&sb, "- **工具调用对比**：原本 **%d 次** → 实际 **%d 次**（**省了 %d 次**", totBaseCalls, totActualCalls, savedCalls)
	if totBaseCalls > 0 {
		fmt.Fprintf(&sb, "，-%.1f%%", float64(savedCalls)/float64(totBaseCalls)*100)
	}
	sb.WriteString("）\n")
	fmt.Fprintf(&sb, "- **上下文体积对比**：原本 **%s** → 实际 **%s**（**省了 %s ≈ %d tokens**",
		humanBytes(totRawBytes), humanBytes(totOutBytes), humanBytes(savedBytes), estSavedTok)
	if totRawBytes > 0 {
		fmt.Fprintf(&sb, "，-%.1f%%", float64(savedBytes)/float64(totRawBytes)*100)
	}
	sb.WriteString("）\n")
	fmt.Fprintf(&sb, "- **避开的 `output.txt` 溢盘**：**%d 次** | **保守估算省下 LLM 轮次**：**%d 轮** | **并发实省墙钟**：**%.2fs**\n",
		totSpills, totTurns, float64(totConcSavedMs)/1000.0)
	fmt.Fprintf(&sb, "- **综合估算省时**：**~%.1f 秒** *（口径：省下串行调用 × 1.6s + 省下轮次 × 7.0s + 实测并发省时）*\n\n", estSavedSec)

	sb.WriteString("| 能力 | 触发次数 | 原本调用 → 实际 | 原本输出/读取 → 实际 | 省下字节 | 避开溢盘 |\n")
	sb.WriteString("| :--- | ---: | ---: | ---: | ---: | ---: |\n")
	for _, k := range order {
		b := buckets[k]
		if b.Count == 0 {
			continue
		}
		if k == "session-skip" {
			fmt.Fprintf(&sb, "| `session-skip` (非仓库0注入) | %d | — | 0 B → 0 B | — | — |\n", b.Count)
			continue
		}
		savedB := max(0, b.RawBytes-b.OutBytes)
		fmt.Fprintf(&sb, "| `%s` | %d | %d → %d | %s → %s | %s | %d |\n",
			k, b.Count, b.BaseCalls, b.ActualCalls,
			humanBytes(b.RawBytes), humanBytes(b.OutBytes), humanBytes(savedB), b.SpillsAvoided)
	}

	if recentN > 0 {
		start := max(0, len(events)-recentN)
		sb.WriteString("\n### 最近触发记录\n")
		for i := len(events) - 1; i >= start; i-- {
			ev := events[i]
			tsShort := ev.Timestamp
			if t, err := time.Parse(time.RFC3339Nano, ev.Timestamp); err == nil {
				tsShort = t.Format("01-02 15:04:05")
			}
			if ev.Command == "session-skip" {
				fmt.Fprintf(&sb, "- `%s` **`session-skip`** (%s): 非代码目录返回 `{}`（0 Token）\n", tsShort, ev.Workspace)
				continue
			}
			savedB := max(0, ev.RawBytes-ev.OutBytes)
			savedC := max(0, ev.BaselineCalls-ev.ActualCalls)
			fmt.Fprintf(&sb, "- `%s` **`%s`** (%s) %s | 调用 %d→%d (省%d) | 体积 %s→%s (省%s)\n",
				tsShort, ev.Command, ev.Workspace, ev.Detail,
				ev.BaselineCalls, ev.ActualCalls, savedC,
				humanBytes(ev.RawBytes), humanBytes(ev.OutBytes), humanBytes(savedB))
		}
	}

	if len(files) > 0 {
		fmt.Fprintf(&sb, "\n日志位置: %s\n", strings.Join(files, ", "))
	}
	return sb.String()
}

func humanBytes(n int) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	if n < 1024*1024 {
		return fmt.Sprintf("%.1f KB", float64(n)/1024.0)
	}
	return fmt.Sprintf("%.2f MB", float64(n)/(1024.0*1024.0))
}
