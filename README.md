<p align="center">
  <img src="assets/logo.svg" alt="agy-zero logo" width="72" />
</p>

<h1 align="center">agy-zero</h1>

<p align="center">
  面向 Google Antigravity 的零 MCP、零常驻上下文挤占单二进制加速插件（CLI：<code>azero</code>）
</p>

## 特性

- **零 MCP / 零全局规则税**：无 `mcp_config.json`、无全局 `always_on` 规则；仅包含 1 个按需加载 Skill（空闲约 45 tokens）与 1 个条件化 `SessionStart` 钩子。
- **跨更新稳定**：仅使用官方 `plugin.json` / `hooks.json` / `skills/*/SKILL.md` 契约与内置 `run_command`，不改客户端文件，`agy` 自动升级持续生效。
- **零运行期依赖**：纯 Go 标准库编译为单个本地二进制（无需 Node / `rg`），所有工具运行在标准终端沙箱内，输出统一硬控 `≤ 7,600 B`（避开 8 KiB / 10 KiB `output.txt` 溢盘）。
- **内置效果记录**：自动记录触发次数、原本调用/体积 vs 实际消耗与节省统计（`azero stats`）。

## 安装与构建

### 1. 构建二进制（Linux / macOS / Windows 通用，需 Go >= 1.22）

```bash
go build -ldflags="-s -w" -o bin/azero ./cmd/azero
```

*(仓库已附带 Linux x64 预编译产物 `bin/azero`，Linux 用户可直接跳过编译。)*

### 2. 安装为 Antigravity 官方插件

```bash
agy plugin install .
agy plugin enable agy-zero
```

可选：软链到 `PATH`，方便手动执行及子代理/Swarm 直接调用 `azero`：

```bash
ln -sf ~/.gemini/config/plugins/agy-zero/bin/azero ~/.local/bin/azero
```

验证安装：

```bash
agy plugin validate --strict ~/.gemini/config/plugins/agy-zero
```

## 核心命令

| 命令 | 用途 | 示例 |
| :--- | :--- | :--- |
| **`azero q`** | 单次并发多路检索（文本/正则/Glob/模糊路径/只读 Git） | `azero q -t "NewServer" -r "func .*" -f "host compat" --git "status --short"` |
| **`azero map`** | 批量提取多文件符号与行号（Go 用真 AST，配合内置 `read_file` 精读） | `azero map cmd/azero/main.go internal/patch/patch.go` |
| **`azero patch`** | 单引号 heredoc 多文件原子替换 + 回滚 + 可选 `--verify` | 见下方示例 |
| **`azero run`** | 受控并发执行构建/测试，PASS 折 1 行、FAIL 提取报错与尾部 | `azero run -j 4 -- "test=go test ./..." "vet=go vet ./..."` |
| **`azero stats`** | 查看触发次数、原本 vs 实际对比与累计节省量 | `azero stats --recent 10 --since 7d` |
| **`azero trace`** | 纯离线分析本机 Antigravity `transcript.jsonl` 瓶颈 | `azero trace --latest` |

### `azero patch` 示例

```bash
azero patch --verify "go test ./..." <<'EOF'
@@@ path/to/file.go
<<<
oldCode()
===
newCode()
>>>
@@@+ path/to/new_file.txt
new file content
EOF
```

- `@@@ path[:start-end]`：精确替换（支持多个 `<<< / === / >>>` 块）
- `@@@+ path`：新建文件（已存在则报错）
- `@@@! path`：新建或覆盖文件

## 环境变量

| 变量 | 默认值 | 说明 |
| :--- | :--- | :--- |
| `AZERO_MAX_BYTES` | `7600` | 输出 UTF-8 字节硬上限（低于 `run_command` 8,192 B 截断线） |
| `AZERO_LOG_FILE` | 自动 | 自定义统计日志 JSONL 路径 |
| `AZERO_NO_LOG` | `0` | 设为 `1` 关闭本地触发统计记录 |

## 开发与测试

```bash
go test -v ./...
agy plugin validate --strict .
```

## 开源协议

[MIT](LICENSE)
