# miniagent

一个用 Go 标准库实现的最小 LLM agent。从 stdin 读取一个 prompt，驱动 ReAct 循环（LLM ↔ 工具调用），把过程事件和最终结果以 NDJSON（每行一个 JSON 对象）写到 stdout。

- 后端：OpenAI 兼容的 `/v1/chat/completions` 接口
- 非流式：每次 LLM 调用是普通 POST，等完整响应返回（无 SSE、无增量片段）
- 无状态：单次 stdin → stdout，无历史、无会话；仅当显式传 `-session` 时把 transcript 落盘以接续对话
- 最小重试：仅 429/500/502/503/504 + 网络错误自动重试 2 次（指数退避，支持 `Retry-After`）；其他 4xx/解析错误立即返回
- 无路径边界约束：工具不约束路径（绝对路径可读写任意位置），shell 无黑名单；仅对 `read`/`edit` 的最终路径做符号链接拒绝（`O_NOFOLLOW`），不构成完整安全边界。隔离责任完全交给调用方（容器/cgroup 等）
- 平台：仅 Linux/macOS（Unix）。`platform.go` 用 `//go:build !windows` 隔离 setpgid/killpg/O_NOFOLLOW，未提供 Windows fallback
- 通信：stdin 进 / NDJSON 出 / stderr 写日志（`log/slog` 文本格式）
- 工具：`read` / `write` / `edit` / `grep` / `glob` / `shell`（全部 free 模式）
- 取消：监听 `SIGINT`/`SIGTERM`，通过 context 取消正在进行的 LLM 调用和工具执行

## 构建

```bash
make build      # 产出 bin/miniagent，version 来自 git describe
make test       # go test -race ./...
```

## 环境变量

| 变量 | 用途 |
|------|------|
| `MINIAGENT_API_KEY` | API 密钥，作为 `Authorization: Bearer <key>` 发送。必需（或用 `-key-file` 从文件注入） |
| `MINIAGENT_BASE_URL` | endpoint 根地址（**不含** `/v1` 后缀），作为 `-base-url` 的默认值 |

## CLI 参数

```
-base-url string         endpoint 根地址（不含 /v1），或 $MINIAGENT_BASE_URL
-key-file string         从文件读 API key（首尾空白截断）；优先于 $MINIAGENT_API_KEY，规避 /proc 泄漏
-log-level string        日志级别：debug|info|warn|error（默认 info）
-max-duration duration   整体墙钟上限（覆盖所有 LLM 调用 + 工具执行），0 表示不限（默认 0）
-max-iterations int      单轮 LLM 调用上限（0=默认 20）
-max-tokens int          单次 LLM 调用的最大输出 token 数（默认 4096）
-max-tokens-total int    单轮累计 token（输入+输出）上限（0=不限）；超限以 error 事件 + 退出码 1 终止
-model string            LLM 模型 id（必需）
-session string          会话文件路径（JSON 历史）：存在则加载作为上下文，结束后写回完整 transcript；缺省则无状态
-shell-timeout duration  单条 shell 命令超时（0=默认 60s）；仍受 -max-duration 总上限约束
-system string           系统提示词（默认为面向工程代码开发的代码向 prompt）
-version                 显示版本号并退出
-workdir string          工作目录（工具相对路径基准 + shell 的 cwd；空则继承进程 cwd，工具不做越界校验）
```

### 子命令

仅 `-version`：打印 `miniagent <version>`，退出码 0。其余时间走主对话流程。

### 主对话流程的前置检查

- `-model` 为空 → stderr 报错 `miniagent: --model is required`，退出码 1
- `$MINIAGENT_API_KEY` 为空 → stderr 报错 `miniagent: $MINIAGENT_API_KEY is required`，退出码 1
- stdin 为空 → stderr 报错 `miniagent: stdin is empty (send prompt via pipe or redirect)`，退出码 1

## NDJSON 输出结构

每个事件占一行，JSON 对象，`type` 字段区分种类。所有事件按时间顺序写入 stdout，最后以一个 `result` 或 `error` 事件结束。

### 事件类型

| type | 何时输出 | 字段 |
|------|---------|------|
| `tool_use` | 每次 LLM 请求工具调用（工具执行前） | `name`, `input` |
| `result` | 主流程成功结束，**终态** | `text`, `model`, `input_tokens`, `output_tokens`, `steps` |
| `error` | 主流程失败，**终态** | `message` |

工具的执行结果不输出到 stdout（仅写入历史消息回灌给 LLM）。

### 字段说明

- `name`：工具名，见下文"工具清单"
- `input`：工具参数的原始 JSON 字符串（LLM 透传）
- `text`：完整回答文本（达到 `maxIterations` 上限被强制终止时为空字符串，键名仍在）。注意：回答被 `max_tokens` 截断（`finish_reason:length`）时 `text` 是半截文本，无专门字段标记，仅在 stderr 日志有 `llm response truncated` 警告
- `model`：本次调用使用的模型 id
- `input_tokens` / `output_tokens`：累计的 token 用量
- `steps`：本轮 LLM 调用次数
- `message`：错误描述

> `result` 事件中 `text`/`model`/`input_tokens`/`output_tokens`/`steps` 均不带 `omitempty`，为 0 也会出现键名，方便消费方稳定 parse。

### 输出示例

正常带工具调用：

```jsonl
{"type":"tool_use","name":"read","input":"{\"path\":\"a.go\"}"}
{"type":"tool_use","name":"shell","input":"{\"command\":\"go test ./...\"}"}
{"type":"result","text":"测试全部通过。","model":"gpt-4o","input_tokens":320,"output_tokens":48,"steps":3}
```

纯文本无工具：

```jsonl
{"type":"result","text":"goroutine 是 Go 运行时管理的轻量级线程。","model":"gpt-4o","input_tokens":24,"output_tokens":18,"steps":1}
```

达到 maxIterations 上限（无最终文本，仍输出累计 usage）：

```jsonl
{"type":"tool_use","name":"shell","input":"{\"command\":\"...\"}"}
{"type":"result","text":"","model":"gpt-4o","input_tokens":8200,"output_tokens":1500,"steps":20}
```

## 工具清单

6 个工具全部为 free 模式：无路径边界约束、无 shell 黑名单。工具参数为 JSON 对象。

### `read`

读取文本文件，输出统一带行号标注（`N │ line` 格式，便于 `edit` 定位）。支持 `offset`/`limit` 按行范围读取。

| 参数 | 类型 | 必需 | 说明 |
|------|------|------|------|
| `path` | string | 是 | 相对 `-workdir` 或绝对路径 |
| `offset` | int | 否 | 起始行（1-based），默认 1 |
| `limit` | int | 否 | 最多返回行数，默认全部，上限 10000 |

约束：单文件最大 80000 字节（超出部分丢弃），输出超过 20000 字符截断。拒绝读取符号链接、目录、非 regular 文件（FIFO/设备/socket）、二进制内容（含 NUL 字节）。`offset` 超出文件行数返回 IsError。

### `write`

覆盖写入文件，自动创建父目录，原子替换（temp + rename），保留原文件权限。

| 参数 | 类型 | 必需 | 说明 |
|------|------|------|------|
| `path` | string | 是 | 相对 `-workdir` 或绝对路径 |
| `content` | string | 是 | 完整文件内容 |

约束：`content` 最大 10 MiB。

### `edit`

精确替换文件中的一段文本。`old_string` 须与文件精确匹配（含缩进和换行）。缺省要求唯一出现（出现 0 次或多次均失败）；设 `replace_all=true` 则替换全部匹配。拒绝编辑符号链接与非普通文件。

| 参数 | 类型 | 必需 | 说明 |
|------|------|------|------|
| `path` | string | 是 | 相对 `-workdir` 或绝对路径 |
| `old_string` | string | 是 | 原文（精确匹配，含缩进和换行） |
| `new_string` | string | 是 | 新文本 |
| `replace_all` | bool | 否 | true 时替换全部匹配处；缺省要求 old_string 唯一 |

约束：文件最大 10 MiB；保留原文件权限。

### `grep`

递归正则搜索文本文件内容，输出 `path:lineno:line`（与 `grep -n` 一致）。跳过 `.git`、符号链接与二进制文件。

| 参数 | 类型 | 必需 | 说明 |
|------|------|------|------|
| `pattern` | string | 是 | 正则表达式（Go regexp 语法，如 `foo`、`(?i)error`） |
| `path` | string | 否 | 搜索根目录，默认 `-workdir` |
| `glob` | string | 否 | 文件名 include 过滤（filepath.Match，如 `*.go`） |

约束：命中行上限 200、输出超 20000 字符截断；操作超时 30s。

### `glob`

递归列举匹配通配的文件路径，每行一个（相对 `-workdir`）。排除 `.git` 与符号链接。

| 参数 | 类型 | 必需 | 说明 |
|------|------|------|------|
| `pattern` | string | 是 | filepath.Match 通配（`*` `?` `[...]`，不跨 `/`、无 `**`） |
| `path` | string | 否 | 根目录，默认 `-workdir` |

约束：命中上限 500 条；操作超时 30s。

### `shell`

通过 `sh -c` 执行命令，stdout+stderr 合并输出。

| 参数 | 类型 | 必需 | 说明 |
|------|------|------|------|
| `command` | string | 是 | shell 命令 |

约束：
- 命令超时 60 秒，超时后整进程组被 `SIGKILL` 清理（防止 `make`/`find` 等派生的孙子进程残留）
- 输出超过 20000 字符截断
- 子进程**继承父进程环境变量，但显式剥离所有 `MINIAGENT_*` 前缀变量**（`API_KEY`/`BASE_URL` 等，防止 LLM 通过 `echo $MINIAGENT_API_KEY` 读取宿主配置与密钥）；**注意：该防护对 `/proc/<pid>/environ` 无效**——procfs 暴露的是 exec 时刻的环境快照，`cat /proc/$PPID/environ` 仍可读到 key。彻底防护需调用方隔离（容器/独立 UID），或用 `-key-file` 不经环境变量传 key（key 不在进程 env，`/proc/$PPID/environ` 读不到）；其他第三方工具的敏感变量（如 `DATABASE_URL`）同样会泄漏，调用方需自行评估风险

## 会话接续（-session）

缺省为无状态单次调用。传 `-session <path>` 后：

- 文件存在则加载其中 `[]Message` 作为历史前缀，自动带入上下文；不存在则视为新会话，结束后创建。
- Run 成功结束后把完整 transcript（历史 + 本轮 user/assistant/tool 往返 + 最终回答）原子写回（temp+rename，权限 0o600）。
- Run 出错（LLM 失败/取消）不写回——失败轮的半成品历史不固化，但工具副作用可能已发生且无记录，消费方需知悉。
- 文件损坏（非法 JSON、未知 role、tool 消息缺 `tool_call_id`、tool_calls/tool 配对断裂、超过 4 MiB 大小上限）→ stderr 报错 + 退出码 1，不静默丢弃历史。
- **信任假设**：session 文件内容原样进入 LLM 上下文，属于可信输入（与 system prompt 同级）；能写该文件的进程即可注入指令。
- 思考内容（reasoning）：wire 解析响应里的 `reasoning_content` / `reasoning`（双兼容），随 assistant 消息进入上下文并以 `reasoning_content` 回灌；**随 session 落盘**（与 content 同级）。若不希望落盘，需在传入前清零 `Message.Reasoning`。
- system prompt 不入 session 文件，每轮由 `-system` 提供；各轮应保持一致。
- 历史只增不减，不做自动修剪/摘要；长会话请自行归档或换新 session 文件。同一文件同时只跑一个进程（并发写不会损坏文件，但后到者覆盖先到者）。

## 退出码

| 码 | 含义 |
|----|------|
| 0 | 正常结束（含达到 `maxIterations` 上限、最终文本为空的场景） |
| 1 | 参数错误、API key 缺失、stdin 为空、session 加载/写回失败、主流程 `error` 事件 |

## 运行隔离（工程实践）

miniagent **不在代码层做任何隔离**：`read`/`write`/`edit`/`grep`/`glob` 接受绝对路径、`shell` 是完整 `sh -c`、无路径边界与命令黑名单。隔离**完全由运行用户的 OS 权限决定**，调用方负责：

- 用**专用低权限用户**运行；workdir 属该用户（或只读挂载），无关路径靠文件系统权限隔离。
- 密钥**用 `-key-file` 从文件注入**（只读挂载、`0600`），而非环境变量——这样 key 不在进程 env，shell 子进程经 `/proc/$PPID/environ` 读不到它。`-key-file` 文件若可被 group/other 读会 stderr 警告。
- 需要更强隔离时自行叠加容器 / 独立 UID / `hidepid` / 网络出口白名单等——这些**不在 miniagent 职责内**，由运行环境提供。

> 一句话：miniagent 信任其运行用户的权限边界；越权访问的唯一闸门是 OS 用户与文件权限。

## 内部约束（常量）

| 常量 | 值 | 含义 |
|------|----|------|
| `maxIterations` | 20 | 单轮 LLM 调用上限（默认值，可被 `-max-iterations` 覆盖） |
| `maxParallelTools` | 8 | 单步内并行工具并发上限 |
| `maxToolResultInHistory` | 2000 | tool 结果进入历史消息的默认字符数（shell/grep/glob） |
| `maxFileResultInHistory` | 8000 | read/edit 结果进入历史消息的字符数（代码内容，截断丢准确性） |
| `contextTrimToolChars` | 1000 | context 超限降级时把 tool 结果压到的字符数 |
| `maxGrepMatches` / `maxGlobEntries` | 200 / 500 | grep 命中行 / glob 命中条数上限 |
| `maxReadFileBytes` / `maxReadFileChars` | 80000 / 20000 | 读文件字节 / 输出字符上限 |
| `maxLineLimit` | 10000 | `read` 的 `limit` 上限 |
| `maxWriteFileBytes` / `maxEditFileBytes` | 10 MiB | 写 / 编辑文件字节上限 |
| `maxShellOutputChars` | 20000 | shell 输出字符上限 |
| `shellTimeout` | 60s | shell 命令超时（默认值，可被 `-shell-timeout` 覆盖） |
| `maxChatBodyBytes` | 4 MiB | chat completions 响应 body 上限 |
| `maxRetries` | 2 | LLM 调用最大重试次数（仅 429/500/502/503/504 + 网络错） |
| `retryBaseDelay` / `retryMaxDelay` | 500ms / 8s | 重试指数退火基线 / 单次封顶 |

## 完整调用示例

```bash
# 单次无状态问答
echo "用一句话解释 goroutine" | MINIAGENT_API_KEY=sk-xxx \
  ./bin/miniagent -model gpt-4o -base-url https://api.openai.com

# 带工具 + 指定工作目录（工具相对路径基于 ./repo，shell 的 cwd 为 ./repo）
echo "在当前目录跑测试并总结失败原因" | MINIAGENT_API_KEY=sk-xxx \
  ./bin/miniagent -model gpt-4o -base-url https://api.openai.com -workdir ./repo

# 自定义系统提示词与 token 上限
echo "重构这段代码" | MINIAGENT_API_KEY=sk-xxx \
  ./bin/miniagent -model gpt-4o -base-url https://api.openai.com \
  -system "你是资深 Go 工程师" -max-tokens 8192

# 限制整体墙钟 5 分钟（防止 ReAct 循环失控烧 token）
echo "跑全量测试并总结" | MINIAGENT_API_KEY=sk-xxx \
  ./bin/miniagent -model gpt-4o -base-url https://api.openai.com -max-duration 5m

# 查看版本
./bin/miniagent -version
```
