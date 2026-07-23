# miniagent

一个用 Go 标准库实现的最小 LLM agent。从 stdin 读取一个 prompt，驱动 ReAct 循环（LLM ↔ 工具调用），把过程事件和最终结果以 NDJSON（每行一个 JSON 对象）写到 stdout。

- 后端：OpenAI 兼容的 `/v1/chat/completions` 接口
- 流式：默认走 SSE，文本以增量片段实时输出
- 通信：stdin 进 / NDJSON 出 / stderr 写日志（`log/slog` 文本格式）
- 工具：文件读写编辑、shell
- 状态：可选的会话历史
- 取消：监听 `SIGINT`/`SIGTERM`，通过 context 取消正在进行的 LLM 调用和工具执行

## 构建

```bash
make build      # 产出 bin/miniagent，version 来自 git describe
make test       # go test -race ./...
```

## 环境变量

| 变量 | 用途 |
|------|------|
| `MINIAGENT_API_KEY` | API 密钥，作为 `Authorization: Bearer <key>` 发送。运行对话或 `--list-models` 时必需 |
| `MINIAGENT_BASE_URL` | endpoint 根地址（**不含** `/v1` 后缀），作为 `--base-url` 的默认值 |

## CLI 参数

```
-base-url string         endpoint 根地址（不含 /v1），或 $MINIAGENT_BASE_URL
-blocked-patterns string JSON 数组，覆盖内置 shell 黑名单（如 '["rm -rf","mkfs"]'）
-chat-id string          会话隔离 id；空表示无历史
-del-session string      删除 --chat-id 下的会话 <id>，完成后退出
-list-models             调用 /v1/models 列出可用模型，完成后退出
-list-sessions           列出 --chat-id 的所有会话，完成后退出
-max-tokens int          单次 LLM 调用的最大输出 token 数（默认 4096）
-model string            LLM 模型 id（运行对话时必需）
-new-session             为 --chat-id 创建新会话，完成后退出
-permission string       权限模式：default / free（默认 default）
-state-dir string        状态目录（空 = 无状态、无持久化）
-max-parallel-tools int  单步内并行工具的并发上限（默认 8；0 = 不限制）
-max-tokens-budget int   单轮累计 input+output token 上限，超限以 incomplete 终止（默认 0 = 不限制）
-max-history-tokens int  历史裁剪的 token 预算（默认 0 = 沿用内置 6000）
-system string           系统提示词（默认 "你是一个简洁的助手，回答通常不超过 500 字。"）
-use-session string      把 --chat-id 切到会话 <id>，完成后退出
-verbose                 输出 tool_use 和 tool_result 事件（默认只输出 tool_use）
-version                 显示版本号并退出
-workdir string          工作目录（工具路径边界 + shell 的 cwd）
```

### 子命令（互斥，按下列顺序检查，先命中者执行后退出）

1. `-version` → 打印 `miniagent <version>`，退出码 0
2. `-list-models` → 调用 `GET {base-url}/v1/models`，打印 JSON 字符串数组
3. `-list-sessions` → 打印 `--chat-id` 的会话数组
4. `-use-session <id>` → 切换会话，打印 `switched to session <id>`
5. `-del-session <id>` → 删除会话，打印 `deleted session <id>`
6. `-new-session` → 创建新会话，打印 `{"session_id": ...}`
7. 无上述参数 → 走主对话流程

### 主对话流程的前置检查

- `-model` 为空 → stderr 报错 `--model is required`，退出码 1
- `$MINIAGENT_API_KEY` 为空 → stderr 报错 `$MINIAGENT_API_KEY is required`，退出码 1
- stdin 为空 → stderr 报错 `stdin is empty (send prompt via pipe or redirect)`，退出码 1
- `-blocked-patterns` 非 JSON 数组 → stderr 报错解析错误，退出码 1

### 权限模式与工具注册

| 模式 | 行为 |
|------|------|
| `default` | 注册 `read_file`/`write_file`/`edit_file`/`shell`（仅当 `-workdir` 非空）。路径被约束在 workspace_root 内，shell 走黑名单过滤 |
| `free` | 与 default 相同的工具集，但 `unrestricted=true`：路径无约束，shell 无黑名单。`-workdir` 为空时仍会注册工具，shell 继承进程 cwd |
| 其他值 | 等同 `default`（`unrestricted` 仅在严格等于 `free` 时为 true） |

> **关于 `free` 模式的边界**：`free` 仅解除路径边界与 shell 黑名单，**不**解除超时（shell 仍 60s）、输出截断（shell 仍 20000 字符上限）、环境变量白名单（子进程仍只继承白名单内的环境变量）。如需完全不受限的 shell，请在操作系统层（容器/cgroup/快照）做隔离。

## NDJSON 输出结构

每个事件占一行，JSON 对象。`type` 字段区分种类。所有事件按时间顺序写入 stdout，最后以一个 `result` 或 `error` 事件结束。

### 事件类型一览

| type | 何时输出 | 字段 |
|------|---------|------|
| `text` | LLM 文本增量（一段回答可多次） | `text` |
| `tool_use` | 每次 LLM 请求工具调用 | `name`, `input` |
| `tool_result` | 工具执行完毕（仅 `-verbose` 时输出） | `name`, `input`, `output`, `is_error` |
| `result` | 主流程成功结束，**终态** | `text`, `model`, `input_tokens`, `output_tokens`, `steps`, `incomplete` |
| `error` | 主流程失败，**终态** | `message` |

> 默认模式（非 `-verbose`）：输出 `tool_use` 和 `text`，不输出 `tool_result`。`result` 和 `error` 在任何模式下都会输出。

### 字段说明

- `name`：工具名，见下文"工具清单"
- `input`：工具参数的原始 JSON 字符串（LLM 透传）
- `output`：工具返回的文本（可能被工具内部截断）
- `is_error`：true 表示工具内部错误（参数缺失、路径越界、超时等），不终止循环
- `text`：`text` 事件里是增量片段（多次拼接成完整回答）；`result` 事件里是完整回答文本（`incomplete=true` 时为空）
- `model`：本次调用使用的模型 id
- `input_tokens` / `output_tokens`：累计的 token 用量
- `steps`：本轮 LLM 调用次数
- `incomplete`：true 表示达到 `maxIterations`（20）上限被强制终止，没有最终文本
- `message`：错误描述

### 输出示例

默认模式（非 verbose）：

```jsonl
{"type":"text","text":"测试"}
{"type":"text","text":"全部通过"}
{"type":"text","text":"。"}
{"type":"tool_use","name":"read_file","input":"{\"path\":\"a.go\"}"}
{"type":"tool_use","name":"shell","input":"{\"command\":\"go test ./...\"}"}
{"type":"result","text":"测试全部通过。","model":"gpt-4o","input_tokens":320,"output_tokens":48,"steps":3}
```

> 流式下 `text` 事件是增量片段，消费方需自行拼接；`result.text` 仍是完整回答。

verbose 模式：

```jsonl
{"type":"tool_use","name":"read_file","input":"{\"path\":\"a.go\"}"}
{"type":"tool_result","name":"read_file","input":"{\"path\":\"a.go\"}","output":"package main\n...","is_error":false}
{"type":"result","text":"...","model":"gpt-4o","input_tokens":320,"output_tokens":48,"steps":2}
```

被 max iterations 截断：

```jsonl
{"type":"result","model":"gpt-4o","input_tokens":8200,"output_tokens":1500,"steps":20,"incomplete":true}
```

> 注意：`result` 事件中 `text`、`input_tokens`、`output_tokens`、`steps` 即使为 0 也会出现键名（无 `omitempty`）；`incomplete` 为 false 时省略（有 `omitempty`）。

### 子命令的输出（非 NDJSON）

- `-version`：`miniagent <version>`（纯文本）
- `-list-models`：`["model-a","model-b"]`（紧凑 JSON 字符串数组，单行）
- `-list-sessions`：缩进 JSON 数组，元素含 `id`/`current`/`bytes`/`mod_time`
- `-use-session`：纯文本 `switched to session <id>`
- `-del-session`：纯文本 `deleted session <id>`
- `-new-session`：`{"session_id": "<id>}`

子命令输出序列化失败时 stderr 报错并退出码 1。

## 工具清单

LLM 可见工具由权限模式和状态配置决定。工具参数为 JSON 对象。

### `read_file`

读取 workspace_root 内的文本文件，输出带行号标注。支持 `offset`/`limit` 按行范围读取。

| 参数 | 类型 | 必需 | 说明 |
|------|------|------|------|
| `path` | string | 是 | 相对 workspace_root 或绝对路径 |
| `offset` | int | 否 | 起始行（1-based），默认 1 |
| `limit` | int | 否 | 最多返回行数，默认全部，上限 10000 |

约束：单文件最大 80000 字节（`maxReadFileBytes`），输出超过 20000 字符截断。

### `write_file`

覆盖写入文件，自动创建父目录，原子替换（temp + rename），保留原文件权限。

| 参数 | 类型 | 必需 | 说明 |
|------|------|------|------|
| `path` | string | 是 | 路径 |
| `content` | string | 是 | 完整文件内容 |

约束：`content` 最大 10 MiB。

### `edit_file`

精确替换文件中的一段文本。`old_string` 必须在文件中唯一出现。出现 0 次或多次都失败。拒绝编辑符号链接。

| 参数 | 类型 | 必需 | 说明 |
|------|------|------|------|
| `path` | string | 是 | 路径 |
| `old_string` | string | 是 | 原文（精确匹配，含缩进和换行） |
| `new_string` | string | 是 | 新文本 |

约束：文件最大 10 MiB。

### `shell`

通过 `sh -c` 执行命令，stdout+stderr 合并输出。超时或取消时整进程组被 `SIGKILL` 清理，防止 `make`/`find` 等派生的孙子进程残留。

| 参数 | 类型 | 必需 | 说明 |
|------|------|------|------|
| `command` | string | 是 | shell 命令 |

约束：
- 命令超时 60 秒（`shellTimeout`），超时后整组进程被 SIGKILL
- 输出超过 20000 字符截断
- `default` 模式：路径必须落在 workspace_root；命令会过黑名单过滤（`rm -rf`、`mkfs`、`dd if=` 等）+ 危险管道模式（`| sh`、`base64 -d |` 等）
- `free` 模式：无约束、无黑名单
- 子进程只继承白名单环境变量（`PATH`/`HOME`/`USER`/`SHELL`/`LANG`/`LC_*`/`TERM`/`PWD`/`TMPDIR`/`TZ`/`EDITOR`/`PAGER`/`GOPATH`/`GOROOT`/`CGO_ENABLED`/`GOFLAGS`/`GOOS`/`GOARCH` 等）

## 状态与持久化

当 `-state-dir=<D>` 且 `-chat-id=<C>` 非空时，下列数据落盘：

```
<D>/
└── miniagent/
    └── history/
        ├── <sanitized-chat-id>__<session-id>.jsonl   # 会话历史，每行一条 Message
        └── <sanitized-chat-id>.cur                    # 当前 session id 指针
```

- `sanitizeChatID` 把非 `[a-zA-Z0-9._-]` 字符替换为 `_`；空 chatID 会处理为 `x`
- `session-id` 格式 `20060102-150405`（秒级时间戳），同秒内冲突时追加 `-<nanosecond>`
- 历史按 token 预算（`maxHistoryTokens=6000`）自动裁剪：先丢整 turn，再截断末尾内容；裁剪保证不破坏 assistant(tool_calls) ↔ tool 的配对
- 所有写入走 temp 文件 + `rename` 原子替换
- 目录权限：`miniagent/` 及其子目录为 `0o750`；数据文件（`.jsonl`、`.cur`）为 `0o600`

## 退出码

| 码 | 含义 |
|----|------|
| 0 | 正常结束（含 `-result.incomplete=true` 的截断场景） |
| 1 | 参数错误、API key 缺失、stdin 为空、子命令失败、主流程 error 事件 |

## HTTP 重试策略

LLM 调用（`POST /v1/chat/completions`）与 `--list-models`（`GET /v1/models`）共用同一套重试规则：

- 重试条件：HTTP 状态码 ∈ {429, 500, 502, 503, 504}
- 退避序列：1s / 2s / 4s，最多 3 次（共 4 次请求）
- `Retry-After` 头（秒数或 HTTP-date）或响应体 `error.retry_after`（秒，浮点）大于当前退避时采纳
- 单次退避上限 60s
- 响应 body 上限：LLM 调用 1 MiB，`--list-models` 4 MiB（恰好上限不截断，超过则报错）
- ctx 取消立即返回 `context.Canceled` / `context.DeadlineExceeded`
- 流式重试边界：仅在收到首个 200 响应前重试（连接失败或上述可重试状态码）；一旦进入 SSE 流读取，`text` 增量已透传给消费方，不再重试，直接返回 error。流式请求复用 `http.Client.Timeout`（默认 120s），该超时覆盖整个流读过程

## 内部约束（常量）

| 常量 | 值 | 含义 |
|------|----|------|
| `maxIterations` | 20 | 单轮对话的 LLM 调用上限 |
| `maxHistoryTokens` | 6000 | 历史裁剪的 token 预算（可被 `-max-history-tokens` 覆盖） |
| `MaxParallelTools` | 8 | 单步内并行工具并发上限（`-max-parallel-tools`，0 = 不限） |
| `MaxTokensBudget` | 0 | 单轮累计 token 上限（`-max-tokens-budget`，0 = 不限） |
| `maxToolResultInHistory` | 2000 | 单条 tool 结果进入历史的字符数 |
| `maxReadFileBytes` / `maxReadFileChars` | 80000 / 20000 | 读文件字节 / 字符上限 |
| `maxLineLimit` | 10000 | `read_file` 的 `limit` 上限 |
| `maxWriteFileBytes` / `maxEditFileBytes` | 10 MiB | 写 / 编辑文件字节上限 |
| `maxShellOutputChars` | 20000 | shell 输出字符上限 |
| `shellTimeout` | 60s | shell 命令超时 |
| `maxModelsBodyBytes` | 4 MiB | `--list-models` 响应 body 上限 |
| `maxRetryDelay` | 60s | HTTP 单次退避上限 |

## 完整调用示例

```bash
# 单次无状态问答
echo "用一句话解释 goroutine" | MINIAGENT_API_KEY=sk-xxx \
  ./bin/miniagent -model gpt-4o -base-url https://api.openai.com

# 带工具 + 会话状态
echo "在 ./repo 下跑测试并总结失败原因" | MINIAGENT_API_KEY=sk-xxx \
  ./bin/miniagent -model gpt-4o -base-url https://api.openai.com \
  -workdir ./repo -state-dir ~/.miniagent -chat-id task-1 -permission default -verbose

# 查看会话状态
./bin/miniagent -state-dir ~/.miniagent -chat-id task-1 -list-sessions

# 新建/切换/删除会话
./bin/miniagent -state-dir ~/.miniagent -chat-id task-1 -new-session
./bin/miniagent -state-dir ~/.miniagent -chat-id task-1 -use-session 20260101-120000
./bin/miniagent -state-dir ~/.miniagent -chat-id task-1 -list-sessions

# 查看版本
./bin/miniagent -version
```
