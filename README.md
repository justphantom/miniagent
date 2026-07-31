# miniagent

一个用 Go 标准库实现的最小 LLM agent。从 stdin 读取一个 prompt，驱动 ReAct 循环（LLM ↔ 工具调用），把过程事件和最终结果以 NDJSON（每行一个 JSON 对象）写到 stdout。

- 后端：OpenAI 兼容的 `/v1/chat/completions` 接口
- 非流式：每次 LLM 调用是普通 POST，等完整响应返回（无 SSE、无增量片段）
- 无状态：单次 stdin → stdout，无历史、无会话；仅当显式传 `-session` 时把 transcript 落盘以接续对话
- 最小重试：仅 429/500/502/503/504 + 网络错误自动重试 2 次（指数退避，支持 `Retry-After`）；其他 4xx/解析错误立即返回
- 无路径边界约束：工具不约束路径（绝对路径可读写任意位置），shell 无黑名单；仅对 `read`/`edit` 的最终路径做符号链接拒绝（`O_NOFOLLOW`），不构成完整安全边界。隔离责任完全交给调用方（容器/cgroup 等）
- 平台：仅 Linux/macOS（Unix）。`platform.go` 用 `//go:build !windows` 隔离 setpgid/killpg/O_NOFOLLOW，未提供 Windows fallback
- 通信：stdin 进 / NDJSON 出 / stderr 写日志（`log/slog` 文本格式）
- 工具：`read` / `write` / `edit` / `shell`（全部 free 模式）
- 取消：监听 `SIGINT`/`SIGTERM`，通过 context 取消正在进行的 LLM 调用和工具执行

## 构建

```bash
make build      # 产出 bin/miniagent，version 来自 git describe
make test       # go test -race ./...
```

## 环境变量

| 变量 | 用途 |
|------|------|
| `MINIAGENT_API_KEY` | API 密钥，作为 `Authorization: Bearer <key>` 发送。必需 |
| `MINIAGENT_BASE_URL` | endpoint 根地址（**不含** `/v1` 后缀），作为 `-base-url` 的默认值 |

## CLI 参数

```
-base-url string         endpoint 根地址（不含 /v1），或 $MINIAGENT_BASE_URL
-max-duration duration   整体墙钟上限（覆盖所有 LLM 调用 + 工具执行），0 表示不限（默认 0）
-max-tokens int          单次 LLM 调用的最大输出 token 数（默认 4096）
-model string            LLM 模型 id（必需）
-session string          会话文件路径（JSON 历史）：存在则加载作为上下文，结束后写回完整 transcript；缺省则无状态
-system string           系统提示词（默认 "你是一个简洁的助手，回答通常不超过 500 字。"）
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
- `text`：完整回答文本（达到 `maxIterations` 上限被强制终止时为空字符串，键名仍在）
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

4 个工具全部为 free 模式：无路径边界约束、无 shell 黑名单。工具参数为 JSON 对象。

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

精确替换文件中的一段文本。`old_string` 必须在文件中唯一出现（出现 0 次或多次都失败）。拒绝编辑符号链接。

| 参数 | 类型 | 必需 | 说明 |
|------|------|------|------|
| `path` | string | 是 | 相对 `-workdir` 或绝对路径 |
| `old_string` | string | 是 | 原文（精确匹配，含缩进和换行） |
| `new_string` | string | 是 | 新文本 |

约束：文件最大 10 MiB；保留原文件权限。

### `shell`

通过 `sh -c` 执行命令，stdout+stderr 合并输出。

| 参数 | 类型 | 必需 | 说明 |
|------|------|------|------|
| `command` | string | 是 | shell 命令 |

约束：
- 命令超时 60 秒，超时后整进程组被 `SIGKILL` 清理（防止 `make`/`find` 等派生的孙子进程残留）
- 输出超过 20000 字符截断
- 子进程**继承父进程环境变量，但显式剥离所有 `MINIAGENT_*` 前缀变量**（`API_KEY`/`BASE_URL` 等，防止 LLM 通过 `echo $MINIAGENT_API_KEY` 读取宿主配置与密钥）；其他第三方工具的敏感变量（如 `DATABASE_URL`）仍会泄漏，调用方需自行评估风险

## 会话接续（-session）

缺省为无状态单次调用。传 `-session <path>` 后：

- 文件存在则加载其中 `[]Message` 作为历史前缀，自动带入上下文；不存在则视为新会话，结束后创建。
- Run 成功结束后把完整 transcript（历史 + 本轮 user/assistant/tool 往返 + 最终回答）原子写回（temp+rename，权限 0o600）。
- Run 出错（LLM 失败/取消）不写回——失败轮的半成品历史不固化，但工具副作用可能已发生且无记录，消费方需知悉。
- 文件损坏（非法 JSON、未知 role、tool 消息缺 `tool_call_id`）→ stderr 报错 + 退出码 1，不静默丢弃历史。
- 思考内容（reasoning/thinking）不进入上下文也不落盘：`Message` 类型没有 reasoning 字段，序列化历史天然不含思考内容。
- system prompt 不入 session 文件，每轮由 `-system` 提供；各轮应保持一致。
- 历史只增不减，不做自动修剪/摘要；长会话请自行归档或换新 session 文件。同一文件同时只跑一个进程（并发写不会损坏文件，但后到者覆盖先到者）。

## 退出码

| 码 | 含义 |
|----|------|
| 0 | 正常结束（含达到 `maxIterations` 上限、最终文本为空的场景） |
| 1 | 参数错误、API key 缺失、stdin 为空、session 加载/写回失败、主流程 `error` 事件 |

## 内部约束（常量）

| 常量 | 值 | 含义 |
|------|----|------|
| `maxIterations` | 20 | 单轮 LLM 调用上限 |
| `maxParallelTools` | 8 | 单步内并行工具并发上限 |
| `maxToolResultInHistory` | 2000 | 单条 tool 结果进入历史消息的字符数 |
| `maxReadFileBytes` / `maxReadFileChars` | 80000 / 20000 | 读文件字节 / 输出字符上限 |
| `maxLineLimit` | 10000 | `read` 的 `limit` 上限 |
| `maxWriteFileBytes` / `maxEditFileBytes` | 10 MiB | 写 / 编辑文件字节上限 |
| `maxShellOutputChars` | 20000 | shell 输出字符上限 |
| `shellTimeout` | 60s | shell 命令超时 |
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
