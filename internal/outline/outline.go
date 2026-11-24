package outline

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"agy-zero/internal/budget"
	"agy-zero/internal/metrics"
)

type Symbol struct {
	Kind      string
	Name      string
	StartLine int
	EndLine   int
	Depth     int
}

type Options struct {
	Cwd      string
	Paths    []string
	MaxBytes int
}

func Run(opts Options) (string, error) {
	if len(opts.Paths) == 0 {
		return "", fmt.Errorf("at least one file path is required")
	}
	if len(opts.Paths) > 40 {
		return "", fmt.Errorf("at most 40 files per call")
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
	sections := make([]string, len(opts.Paths))
	origBytes := make([]int, len(opts.Paths))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for i, p := range opts.Paths {
		wg.Add(1)
		go func(idx int, rawPath string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			sections[idx], origBytes[idx] = outlineOne(absCwd, rawPath)
		}(i, p)
	}
	wg.Wait()

	out := budget.FitSections(sections, maxBytes, "\n\n", false)
	totalRaw := 0
	for _, b := range origBytes {
		totalRaw += b
	}
	metrics.Record(metrics.Event{
		Command:       "map",
		Workspace:     filepath.Base(absCwd),
		Detail:        fmt.Sprintf("%d file(s)", len(opts.Paths)),
		BaselineCalls: len(opts.Paths),
		ActualCalls:   1,
		RawBytes:      totalRaw,
		OutBytes:      len(out),
		SpillsAvoided: 0,
		EstTurnsSaved: max(0, (len(opts.Paths)-1)/4),
		WallMs:        time.Since(wallStart).Milliseconds(),
		OK:            true,
	})
	return out, nil
}

func outlineOne(baseCwd, rawPath string) (string, int) {
	abs := rawPath
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(baseCwd, rawPath)
	}
	rel := rawPath
	if r, err := filepath.Rel(baseCwd, abs); err == nil && !strings.HasPrefix(r, "..") {
		rel = filepath.ToSlash(r)
	}
	st, err := os.Stat(abs)
	if err != nil {
		return fmt.Sprintf("## %s\nERROR: %v", rel, err), 0
	}
	if st.IsDir() {
		return fmt.Sprintf("## %s\nERROR: is a directory", rel), 0
	}
	if st.Size() > 5*1024*1024 {
		return fmt.Sprintf("## %s\nERROR: file too large (%d bytes)", rel, st.Size()), 46080
	}
	raw, err := os.ReadFile(abs)
	if err != nil {
		return fmt.Sprintf("## %s\nERROR: %v", rel, err), 0
	}
	// Blind read_file caps at 46,080 B per call.
	blindReadBytes := min(len(raw), 46080)
	text := strings.ReplaceAll(string(raw), "\r\n", "\n")
	lines := strings.Split(text, "\n")
	ext := strings.ToLower(filepath.Ext(abs))

	var syms []Symbol
	switch ext {
	case ".go":
		syms = extractGoAST(abs, text)
	case ".ts", ".tsx", ".mts", ".cts", ".js", ".jsx", ".mjs", ".cjs":
		syms = extractTSJS(lines)
	case ".py", ".pyi":
		syms = extractPython(lines)
	case ".rs":
		syms = extractRust(lines)
	case ".c", ".h", ".cc", ".cpp", ".cxx", ".hpp", ".java", ".kt", ".cs", ".swift":
		syms = extractCLike(lines)
	case ".md", ".mdx", ".markdown":
		syms = extractMarkdown(lines)
	case ".sh", ".bash", ".zsh":
		syms = extractShell(lines)
	case ".json", ".jsonc":
		syms = extractJSON(lines)
	case ".yml", ".yaml":
		syms = extractYAML(lines)
	case ".vue", ".svelte", ".html", ".htm":
		syms = extractMarkup(lines)
	default:
		syms = extractCLike(lines)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "## %s (%d lines)", rel, len(lines))
	if len(syms) == 0 {
		b.WriteString("\n  (no top-level symbols detected)")
		return b.String(), blindReadBytes
	}
	for _, s := range syms {
		indent := "  "
		if s.Depth > 0 {
			indent = "    "
		}
		if s.EndLine > s.StartLine {
			fmt.Fprintf(&b, "\n%sL%d-%d %s %s", indent, s.StartLine, s.EndLine, s.Kind, s.Name)
		} else {
			fmt.Fprintf(&b, "\n%sL%d %s %s", indent, s.StartLine, s.Kind, s.Name)
		}
	}
	return b.String(), blindReadBytes
}

// extractGoAST uses Go's real stdlib AST parser (zero regex drift).
func extractGoAST(filename, src string) []Symbol {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, src, parser.SkipObjectResolution)
	if err != nil && file == nil {
		return nil
	}
	var syms []Symbol
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			start := fset.Position(d.Pos()).Line
			end := fset.Position(d.End()).Line
			kind := "fn"
			name := d.Name.Name
			if d.Recv != nil && len(d.Recv.List) > 0 {
				kind = "method"
				name = fmt.Sprintf("(%s).%s", exprName(d.Recv.List[0].Type), name)
			}
			syms = append(syms, Symbol{Kind: kind, Name: name, StartLine: start, EndLine: end})
		case *ast.GenDecl:
			if d.Tok != token.TYPE {
				continue
			}
			for _, spec := range d.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				start := fset.Position(ts.Pos()).Line
				end := fset.Position(ts.End()).Line
				kind := "type"
				switch ts.Type.(type) {
				case *ast.StructType:
					kind = "struct"
				case *ast.InterfaceType:
					kind = "interface"
				}
				syms = append(syms, Symbol{Kind: kind, Name: ts.Name.Name, StartLine: start, EndLine: end})
			}
		}
	}
	return syms
}

func exprName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return "*" + exprName(t.X)
	case *ast.IndexExpr:
		return exprName(t.X)
	case *ast.IndexListExpr:
		return exprName(t.X)
	default:
		return "T"
	}
}

func findBraceEnd(lines []string, startIdx int) int {
	depth := 0
	seen := false
	inBlock := false
	maxScan := min(len(lines), startIdx+1500)
	for i := startIdx; i < maxScan; i++ {
		line := lines[i]
		var quote byte
		escaped := false
		for j := 0; j < len(line); j++ {
			ch := line[j]
			var next byte
			if j+1 < len(line) {
				next = line[j+1]
			}
			if inBlock {
				if ch == '*' && next == '/' {
					inBlock = false
					j++
				}
				continue
			}
			if escaped {
				escaped = false
				continue
			}
			if quote != 0 {
				if ch == '\\' {
					escaped = true
				} else if ch == quote {
					quote = 0
				}
				continue
			}
			if ch == '/' && next == '/' {
				break
			}
			if ch == '/' && next == '*' {
				inBlock = true
				j++
				continue
			}
			if ch == '\'' || ch == '"' || ch == '`' {
				quote = ch
				continue
			}
			if ch == '{' {
				depth++
				seen = true
			} else if ch == '}' {
				depth--
				if seen && depth <= 0 {
					return i + 1
				}
			}
		}
		if !seen && i > startIdx+25 {
			return startIdx + 1
		}
	}
	if seen {
		return maxScan
	}
	return startIdx + 1
}

var (
	tsClassRE = regexp.MustCompile(`^(?:export\s+(?:default\s+)?)?(?:declare\s+)?(?:abstract\s+)?class\s+([A-Za-z_$][\w$]*)`)
	tsIfaceRE = regexp.MustCompile(`^(?:export\s+)?(?:declare\s+)?interface\s+([A-Za-z_$][\w$]*)`)
	tsTypeRE  = regexp.MustCompile(`^(?:export\s+)?(?:declare\s+)?type\s+([A-Za-z_$][\w$]*)`)
	tsEnumRE  = regexp.MustCompile(`^(?:export\s+)?(?:declare\s+)?(?:const\s+)?enum\s+([A-Za-z_$][\w$]*)`)
	tsFuncRE  = regexp.MustCompile(`^(?:export\s+(?:default\s+)?)?(?:declare\s+)?(?:async\s+)?function\*?\s+([A-Za-z_$][\w$]*)`)
	tsArrowRE = regexp.MustCompile(`^(?:export\s+)?(?:const|let|var)\s+([A-Za-z_$][\w$]*)\s*(?::[^=]+)?=\s*(?:async\s+)?(?:function\b|\([^)]*\)\s*(?::[^=]+)?=>|[A-Za-z_$][\w$]*\s*=>)`)
	tsMethRE  = regexp.MustCompile(`^[ \t]{2,4}(?:(?:public|private|protected|static|readonly|override|abstract|async|get|set)\s+)*([A-Za-z_$][\w$]*)\s*(?:<[^>]*>)?\s*\([^)]*\)[^{]*\{`)
)

func extractTSJS(lines []string) []Symbol {
	var syms []Symbol
	classEnd := -1
	control := map[string]bool{"if": true, "for": true, "while": true, "switch": true, "catch": true, "else": true, "return": true}
	for i, line := range lines {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "//") || strings.HasPrefix(t, "*") || strings.HasPrefix(t, "/*") {
			continue
		}
		if line[0] != ' ' && line[0] != '\t' {
			if m := tsClassRE.FindStringSubmatch(line); m != nil {
				end := findBraceEnd(lines, i)
				syms = append(syms, Symbol{"class", m[1], i + 1, end, 0})
				classEnd = end
			} else if m := tsIfaceRE.FindStringSubmatch(line); m != nil {
				syms = append(syms, Symbol{"interface", m[1], i + 1, findBraceEnd(lines, i), 0})
			} else if m := tsTypeRE.FindStringSubmatch(line); m != nil {
				end := i + 1
				if strings.ContainsRune(line, '{') {
					end = findBraceEnd(lines, i)
				}
				syms = append(syms, Symbol{"type", m[1], i + 1, end, 0})
			} else if m := tsEnumRE.FindStringSubmatch(line); m != nil {
				syms = append(syms, Symbol{"enum", m[1], i + 1, findBraceEnd(lines, i), 0})
			} else if m := tsFuncRE.FindStringSubmatch(line); m != nil {
				syms = append(syms, Symbol{"fn", m[1], i + 1, findBraceEnd(lines, i), 0})
			} else if m := tsArrowRE.FindStringSubmatch(line); m != nil {
				syms = append(syms, Symbol{"fn", m[1], i + 1, findBraceEnd(lines, i), 0})
			}
		} else if i+1 <= classEnd {
			if m := tsMethRE.FindStringSubmatch(line); m != nil && !control[m[1]] {
				syms = append(syms, Symbol{"method", m[1], i + 1, findBraceEnd(lines, i), 1})
			}
		}
	}
	return syms
}

var pySymRE = regexp.MustCompile(`^([ \t]*)(async\s+def|def|class)\s+([A-Za-z_]\w*)`)

func extractPython(lines []string) []Symbol {
	indentOf := func(l string) int {
		n := 0
		for i := 0; i < len(l); i++ {
			if l[i] == ' ' {
				n++
			} else if l[i] == '\t' {
				n += 4
			} else {
				break
			}
		}
		return n
	}
	type raw struct {
		indent int
		kind   string
		name   string
		idx    int
	}
	var items []raw
	for i, l := range lines {
		m := pySymRE.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		ind := indentOf(l)
		if ind > 4 {
			continue
		}
		kind := "fn"
		if m[2] == "class" {
			kind = "class"
		} else if ind > 0 {
			kind = "method"
		}
		items = append(items, raw{ind, kind, m[3], i})
	}
	syms := make([]Symbol, 0, len(items))
	for _, it := range items {
		end := it.idx + 1
		for j := it.idx + 1; j < len(lines); j++ {
			tr := strings.TrimSpace(lines[j])
			if tr == "" || strings.HasPrefix(tr, "#") {
				continue
			}
			if indentOf(lines[j]) <= it.indent {
				break
			}
			end = j + 1
		}
		depth := 0
		if it.indent > 0 {
			depth = 1
		}
		syms = append(syms, Symbol{it.kind, it.name, it.idx + 1, end, depth})
	}
	return syms
}

var rustSymRE = regexp.MustCompile(`^(?:pub(?:\([^)]*\))?\s+)?(?:async\s+)?(?:unsafe\s+)?(fn|struct|enum|trait|mod|impl)\s+([A-Za-z_][\w:<>,\s]*)`)

func extractRust(lines []string) []Symbol {
	var syms []Symbol
	for i, line := range lines {
		if len(line) == 0 || line[0] == ' ' || line[0] == '\t' {
			continue
		}
		if m := rustSymRE.FindStringSubmatch(line); m != nil {
			name := strings.Fields(m[2])[0]
			name = strings.TrimRight(name, "{(;:")
			syms = append(syms, Symbol{m[1], name, i + 1, findBraceEnd(lines, i), 0})
		}
	}
	return syms
}

var cLikeRE = regexp.MustCompile(`^(?:(?:public|private|protected|internal|static|final|abstract|virtual|inline|extern|export|async)\s+)*(class|struct|interface|enum|namespace|func|def)\s+([A-Za-z_]\w*)`)

func extractCLike(lines []string) []Symbol {
	var syms []Symbol
	for i, line := range lines {
		if len(line) == 0 || line[0] == ' ' || line[0] == '\t' {
			continue
		}
		if m := cLikeRE.FindStringSubmatch(line); m != nil {
			syms = append(syms, Symbol{m[1], m[2], i + 1, findBraceEnd(lines, i), 0})
		}
	}
	return syms
}

var mdHeadRE = regexp.MustCompile(`^(#{1,4})\s+(.+)$`)

func extractMarkdown(lines []string) []Symbol {
	var syms []Symbol
	inFence := false
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			continue
		}
		if m := mdHeadRE.FindStringSubmatch(line); m != nil {
			depth := 0
			if len(m[1]) >= 3 {
				depth = 1
			}
			syms = append(syms, Symbol{m[1], strings.TrimSpace(m[2]), i + 1, i + 1, depth})
		}
	}
	return syms
}

var shFuncRE = regexp.MustCompile(`^(?:function\s+)?([A-Za-z_][\w-]*)\s*\(\s*\)`)

func extractShell(lines []string) []Symbol {
	var syms []Symbol
	for i, line := range lines {
		if m := shFuncRE.FindStringSubmatch(line); m != nil {
			syms = append(syms, Symbol{"fn", m[1], i + 1, findBraceEnd(lines, i), 0})
		}
	}
	return syms
}

var jsonKeyRE = regexp.MustCompile(`^  "([^"]+)"\s*:`)

func extractJSON(lines []string) []Symbol {
	var syms []Symbol
	for i, line := range lines {
		if m := jsonKeyRE.FindStringSubmatch(line); m != nil {
			end := i + 1
			if strings.ContainsRune(line, '{') {
				end = findBraceEnd(lines, i)
			}
			syms = append(syms, Symbol{"key", m[1], i + 1, end, 0})
		}
	}
	return syms
}

var yamlKeyRE = regexp.MustCompile(`^([A-Za-z0-9_.-]+)\s*:`)

func extractYAML(lines []string) []Symbol {
	var syms []Symbol
	for i, line := range lines {
		if m := yamlKeyRE.FindStringSubmatch(line); m != nil {
			syms = append(syms, Symbol{"key", m[1], i + 1, i + 1, 0})
		}
	}
	return syms
}

var markupTagRE = regexp.MustCompile(`^<(template|script|style|section|main|header|footer|nav)\b[^>]*>`)

func extractMarkup(lines []string) []Symbol {
	var syms []Symbol
	for i, line := range lines {
		tr := strings.TrimSpace(line)
		if m := markupTagRE.FindStringSubmatch(tr); m != nil {
			tag := m[1]
			end := i + 1
			closeTag := "</" + tag + ">"
			for j := i; j < len(lines); j++ {
				if strings.Contains(lines[j], closeTag) {
					end = j + 1
					break
				}
			}
			syms = append(syms, Symbol{"<" + tag + ">", tag, i + 1, end, 0})
		}
	}
	return syms
}
