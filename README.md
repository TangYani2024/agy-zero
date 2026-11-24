<p align="center">
  <img src="assets/logo.svg" alt="agy-zero logo" width="72" />
</p>

<h1 align="center">agy-zero</h1>

<p align="center">
  面向 Google Antigravity（CLI / Desktop 2.0 / IDE）的零 MCP、全平台单二进制加速插件（CLI：<code>azero</code>）
</p>

## 特性

- **零 MCP / 零全局规则税**：无 `mcp_config.json`、无全局 `always_on` 规则；仅含 1 个按需 Skill（空闲 ~45 tokens）与 1 个条件化 `SessionStart` 钩子（非代码目录 0 注入）。
- **三系统 + 三客户端全兼容**：支持 Linux / macOS / Windows，以及 Antigravity CLI / Desktop 2.0 / IDE；仅依赖官方插件契约，`agy` 自动更新持续有效。
- **零运行期依赖**：纯 Go 标准库编译为单二进制（无需外部 `rg`），全工具运行在标准终端沙箱内，输出统一硬控 `≤ 7,600 B`。
- **内置效果统计**：自动记录触发次数、原本调用/体积 vs 实际消耗与节省量（`azero stats`）。

## 安装与构建

### 1. 构建二进制（需 Go >= 1.22）

**Linux / macOS：**
```bash
go build -ldflags="-s -w" -o bin/azero ./cmd/azero
```

**Windows (PowerShell)：**
```powershell
go build -ldflags="-s -w" -o bin\azero.exe .\cmd\azero
```

*(仓库默认附带 Linux x64 预编译产物 `bin/azero`；其他平台也可直接从 GitHub Releases 下载对应二进制放入 `bin/`。)*

### 2. 安装为 Antigravity 官方插件

```bash
agy plugin install .
agy plugin enable agy-zero
```

可选：加入 `PATH` 方便手动执行及子代理/Swarm 直接调用：

```bash
# Linux / macOS
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
| **`azero patch`** | 多文件原子替换 + 回滚 + 可选 `--verify` | 见下方示例 |
| **`azero run`** | 受控并发执行构建/测试，PASS 折 1 行、FAIL 提取报错与尾部 | `azero run -j 4 -- "test=go test ./..." "vet=go vet ./..."` |
| **`azero stats`** | 查看触发次数、原本 vs 实际对比与累计节省量 | `azero stats --recent 10 --since 7d` |
| **`azero trace`** | 纯离线分析本机 Antigravity `transcript.jsonl` 瓶颈 | `azero trace --latest` |

### `azero patch` 示例

**Linux / macOS：**
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

**Windows (PowerShell)：**
```powershell
@'
@@@ path/to/file.go
<<<
oldCode()
===
newCode()
>>>
'@ | azero patch --verify "go test ./..."
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
go vet ./...
go test -v ./...
agy plugin validate --strict .
```

## 开源协议

[MIT](LICENSE)
