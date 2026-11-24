---
name: agy-zero
description: >-
  Zero-MCP single-binary CLI accelerator (`azero`) for batched code search/readonly-git (`azero q`),
  multi-file symbol outlines (`azero map`), atomic multi-hunk heredoc patching with inline verify
  (`azero patch`), concurrent checks (`azero run`), and local transcript profiling (`azero trace`).
  Use whenever doing >=2 searches, inspecting large (>300-line) files before ranged `read_file`,
  applying multi-hunk/cross-file edits, running >=2 checks, or auditing session token/latency waste.
---

# `agy-zero` (`azero`) — Zero-MCP CLI Accelerator

Bundled binary (relative to this `SKILL.md`): [`../../bin/azero`](../../bin/azero) (Windows: `..\..\bin\azero.exe`), or invoke `azero` directly if on `PATH`. All commands run inside the standard sandbox and hard-cap stdout to `<= 7,600 B` (safely below `run_command`'s `8,192 B` truncation line).

## Tool Routing

- **Single small file (<300 lines) or known line range**: use built-in `read_file(StartLine, EndLine)` (45 KiB / 800 lines inline, 0 vetting).
- **Single-file 1-block edit**: use built-in `edit_file`.
- **>=2 searches / globs / fuzzy paths / read-only git**: `azero q`
- **Unknown large files (>300 lines)**: `azero map <files...>` -> then parallel built-in `read_file(StartLine, EndLine)`.
- **>=2 hunks or >=2 files (or edit + verify)**: `azero patch --verify "<cmd>"`
- **>=2 independent build/lint/test commands**: `azero run -j 4 -- "cmd1" "cmd2"`
- **Inspect real session token/cache/spill bottlenecks or savings**: `azero trace --latest` / `azero stats`

## Quick Commands

### 1. Batched Search & Read-Only Git (`azero q`)
```bash
azero q [-C <dir>] [-i] [--ext ts,go] [--files|--count] [-m 30] \
  -t "literal" -r "regex" -g "*.ts" -f "mcp server" --git "status --short"
```
Prefix any pattern with `@subdir:` to scope a single query (e.g., `-t "@src:NewServer"`).

### 2. Multi-File Symbol Outlines (`azero map`)
```bash
azero map [-C <dir>] src/a.go src/b.ts src/c.py
```
Uses real `go/ast` for Go and lexical scanners for TS/JS/Python/Rust/C/Java/MD/JSON/YAML. Follow up with parallel built-in `read_file`.

### 3. Atomic Multi-File Heredoc Patch + Verify (`azero patch`)

**Linux / macOS (Bash/Zsh):**
```bash
azero patch [-C <dir>] [--dry-run] [--best-effort] [--verify "go test ./..."] <<'EOF'
@@@ path/to/file.go
<<<
exact old block
===
replacement block
>>>
@@@ path/to/file.go:10-40
<<<
scoped old block
===
scoped replacement
>>>
@@@+ path/to/new_file.txt
new file contents
@@@! path/to/overwrite.txt
overwritten file contents
EOF
```

**Windows (PowerShell):**
```powershell
@'
@@@ path/to/file.go
<<<
exact old block
===
replacement block
>>>
'@ | azero patch --verify "go test ./..."
```
All-or-nothing in-memory validation before atomic rename commit (with rollback on write error). Mismatches return line-drift or closest-region hints.

### 4. Concurrent Checks (`azero run`)
```bash
azero run [-C <dir>] [-j 4] [--timeout 60] [--full] -- "typecheck=npm run typecheck" "test=npm test"
```
Default compact mode folds passing checks to 1 line and preserves root-cause errors + failure tail within `<= 7,600 B`.

### 5. Local Transcript X-Ray & Savings Stats (`azero trace` / `azero stats`)
```bash
azero stats [--recent 10] [--since 24h] [--json] [--reset]
azero trace --latest          # inspect most recent conversation transcript.jsonl
azero trace --all --recent 10 # aggregate metrics across last 10 sessions
```

Override byte cap via `AZERO_MAX_BYTES=7600`; override metrics file via `AZERO_LOG_FILE` (or `AZERO_NO_LOG=1`).
