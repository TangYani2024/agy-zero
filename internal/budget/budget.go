package budget

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	HardDefaultMaxBytes = 7600
	MinMaxBytes         = 800
	MaxAllowedBytes     = 40000
)

var (
	ansiRE = regexp.MustCompile(`\x1b(?:\[[0-?]*[ -/]*[@-~]|\][^\x07\x1b]*(?:\x07|\x1b\\)|[@-Z\\-_])`)

	errorLineRE = regexp.MustCompile(`(?i)(?:\berr!|\b(?:errors?|fail(?:ed|ing|ure|ures)?|exception|traceback|panic|fatal|assertion|syntaxerror|typeerror|referenceerror|ts\d{4}|e\d{4}|cannot find|not found|undefined|compilation error)\b|caused by:|<<<\s*(?:failure|error))`)
	benignZeroRE = regexp.MustCompile(`(?i)\b(?:0|no)\s+(?:errors?|warnings?|fail(?:ed|ures?|ing)?)\b|\b(?:errors?|warnings?|failures?|failed)\s*(?::|=)\s*0\b`)
	noiseLineRE  = regexp.MustCompile(`(?i)^(?:\(node:\d+\)\s*ExperimentalWarning:|\(Use ` + "`node --trace-warnings" + `|\[ERROR\]\s*(?:To see the full stack trace|Re-run Maven|For more information|\[Help \d+\]|->\s*\[Help \d+\]))`)
	summaryLineRE = regexp.MustCompile(`(?i)\b(?:passed|pass|ok|tests?|suites?|built in|compiled|done in|finished|0 errors|success)\b`)
)

func DefaultMaxBytes() int {
	if raw := strings.TrimSpace(os.Getenv("AZERO_MAX_BYTES")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			return Clamp(n, MinMaxBytes, MaxAllowedBytes)
		}
	}
	return HardDefaultMaxBytes
}

func Clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func StripANSI(s string) string {
	return ansiRE.ReplaceAllString(s, "")
}

// NormalizeTerminal strips ANSI escapes and resolves carriage-return progress overwrites.
func NormalizeTerminal(raw string) string {
	clean := strings.ReplaceAll(StripANSI(raw), "\r\n", "\n")
	lines := strings.Split(clean, "\n")
	for i, line := range lines {
		if strings.ContainsRune(line, '\r') {
			parts := strings.Split(line, "\r")
			last := ""
			for _, p := range parts {
				if p != "" {
					last = p
				}
			}
			lines[i] = last
		}
	}
	return strings.TrimRight(strings.Join(lines, "\n"), " \t\n")
}

// SliceUTF8Head returns the longest valid UTF-8 prefix <= maxBytes.
func SliceUTF8Head(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	end := maxBytes
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end]
}

// SliceUTF8Tail returns the longest valid UTF-8 suffix <= maxBytes.
func SliceUTF8Tail(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	start := len(s) - maxBytes
	for start < len(s) && !utf8.RuneStart(s[start]) {
		start++
	}
	return s[start:]
}

func TruncateUTF8(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	if maxBytes < 60 {
		if maxBytes <= 3 {
			return SliceUTF8Head(s, maxBytes)
		}
		return SliceUTF8Head(s, maxBytes-3) + "..."
	}
	maxNotice := fmt.Sprintf("\n... [truncated: showed %d of %d bytes]", len(s), len(s))
	cutoff := maxBytes - len(maxNotice)
	if cutoff < 16 {
		return SliceUTF8Head(s, maxBytes-3) + "..."
	}
	head := SliceUTF8Head(s, cutoff)
	if nl := strings.LastIndexByte(head, '\n'); nl > cutoff*6/10 {
		head = head[:nl]
	}
	out := fmt.Sprintf("%s\n... [truncated: showed %d of %d bytes]", head, len(head), len(s))
	if len(out) > maxBytes {
		return SliceUTF8Head(out, maxBytes)
	}
	return out
}

func TruncateHeadTailUTF8(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	if maxBytes < 140 {
		return TruncateUTF8(s, maxBytes)
	}
	maxMarker := fmt.Sprintf("\n... [folded %d middle bytes; total %d bytes] ...\n", len(s), len(s))
	avail := maxBytes - len(maxMarker)
	if avail < 48 {
		return TruncateUTF8(s, maxBytes)
	}
	headBudget := max(20, avail*38/100)
	tailBudget := max(24, avail-headBudget)

	head := SliceUTF8Head(s, headBudget)
	if nl := strings.LastIndexByte(head, '\n'); nl > headBudget/2 {
		head = head[:nl]
	}
	tail := SliceUTF8Tail(s, tailBudget)
	if nl := strings.IndexByte(tail, '\n'); nl >= 0 && len(tail)-(nl+1) > tailBudget/2 {
		tail = tail[nl+1:]
	}
	if len(head)+len(tail) >= len(s) {
		return s
	}
	omitted := len(s) - len(head) - len(tail)
	marker := fmt.Sprintf("\n... [folded %d middle bytes; total %d bytes] ...\n", omitted, len(s))
	res := head + marker + tail
	if len(res) > maxBytes {
		return TruncateUTF8(res, maxBytes)
	}
	return res
}

// WaterFill allocates total across items: smaller items keep full size and donate unused share.
func WaterFill(sizes []int, total int) []int {
	n := len(sizes)
	out := make([]int, n)
	if n == 0 || total <= 0 {
		return out
	}
	type pair struct{ idx, size int }
	order := make([]pair, n)
	for i, sz := range sizes {
		if sz < 0 {
			sz = 0
		}
		order[i] = pair{idx: i, size: sz}
	}
	sort.Slice(order, func(i, j int) bool {
		if order[i].size != order[j].size {
			return order[i].size < order[j].size
		}
		return order[i].idx < order[j].idx
	})
	rem := total
	for i := 0; i < n; i++ {
		left := n - i
		share := rem / left
		if order[i].size <= share {
			out[order[i].idx] = order[i].size
			rem -= order[i].size
		} else {
			for j := i; j < n; j++ {
				alloc := rem / (n - j)
				out[order[j].idx] = alloc
				rem -= alloc
			}
			break
		}
	}
	return out
}

func FitSections(sections []string, maxBytes int, sep string, headTail bool) string {
	if len(sections) == 0 || maxBytes <= 0 {
		return ""
	}
	trunc := TruncateUTF8
	if headTail {
		trunc = TruncateHeadTailUTF8
	}
	joined := strings.Join(sections, sep)
	if len(joined) <= maxBytes {
		return joined
	}
	sepTotal := len(sep) * (len(sections) - 1)
	if sepTotal >= maxBytes {
		return trunc(joined, maxBytes)
	}
	sizes := make([]int, len(sections))
	for i, s := range sections {
		sizes[i] = len(s)
	}
	budgets := WaterFill(sizes, maxBytes-sepTotal)
	fitted := make([]string, len(sections))
	for i, s := range sections {
		fitted[i] = trunc(s, budgets[i])
	}
	res := strings.Join(fitted, sep)
	if len(res) > maxBytes {
		return trunc(res, maxBytes)
	}
	return res
}

func isErrorLine(line string) bool {
	cleaned := benignZeroRE.ReplaceAllString(line, " ")
	return errorLineRE.MatchString(cleaned)
}

// CompactCommandOutput produces a concise summary for passing commands or
// error-focused + tail-preserved diagnostics for failing commands within maxBytes.
func CompactCommandOutput(stdout, stderr string, exitCode int, timedOut bool, spawnErr string, maxBytes int) string {
	if spawnErr != "" {
		return TruncateUTF8("spawn error: "+spawnErr, maxBytes)
	}
	normOut := NormalizeTerminal(stdout)
	normErr := NormalizeTerminal(stderr)
	combined := strings.TrimSpace(strings.Join(nonEmpty(normOut, normErr), "\n"))
	if combined == "" {
		if exitCode == 0 && !timedOut {
			return "(ok, no output)"
		}
		if timedOut {
			return "(timed out, no output)"
		}
		return fmt.Sprintf("(exit %d, no output)", exitCode)
	}

	rawLines := strings.Split(combined, "\n")
	if exitCode == 0 && !timedOut {
		// Return last meaningful summary line, else last non-empty line.
		lastNonEmpty := ""
		bestSummary := ""
		for _, l := range rawLines {
			t := strings.TrimSpace(l)
			if t == "" {
				continue
			}
			lastNonEmpty = t
			if summaryLineRE.MatchString(t) {
				bestSummary = t
			}
		}
		pick := bestSummary
		if pick == "" {
			pick = lastNonEmpty
		}
		return TruncateUTF8(pick, min(maxBytes, 240))
	}

	// Failure / timeout: strip known noise lines, keep error blocks + last 15 lines.
	lines := make([]string, 0, len(rawLines))
	for _, l := range rawLines {
		if noiseLineRE.MatchString(l) {
			continue
		}
		lines = append(lines, l)
	}
	if len(lines) == 0 {
		lines = rawLines
	}
	full := strings.Join(lines, "\n")
	if len(full) <= maxBytes {
		return full
	}

	keep := make([]bool, len(lines))
	errHits := 0
	for i, l := range lines {
		if isErrorLine(l) {
			errHits++
			from := max(0, i-1)
			to := min(len(lines)-1, i+3)
			for j := from; j <= to; j++ {
				keep[j] = true
			}
		}
	}
	tailStart := max(0, len(lines)-15)
	for i := tailStart; i < len(lines); i++ {
		keep[i] = true
	}
	if errHits == 0 {
		return TruncateHeadTailUTF8(full, maxBytes)
	}

	var b strings.Builder
	prev := -1
	for i, ok := range keep {
		if !ok {
			continue
		}
		if prev == -1 && i > 0 {
			fmt.Fprintf(&b, "... (%d lines omitted) ...\n", i)
		} else if prev != -1 && i > prev+1 {
			fmt.Fprintf(&b, "... (%d lines omitted) ...\n", i-prev-1)
		}
		b.WriteString(lines[i])
		b.WriteByte('\n')
		prev = i
	}
	res := strings.TrimRight(b.String(), "\n")
	if len(res) > maxBytes {
		return TruncateHeadTailUTF8(res, maxBytes)
	}
	return res
}

func nonEmpty(items ...string) []string {
	out := make([]string, 0, len(items))
	for _, s := range items {
		if strings.TrimSpace(s) != "" {
			out = append(out, s)
		}
	}
	return out
}
