# miniagent

一个用 Go 标准库实现的最小 LLM agent。从 stdin 读取一个 prompt，驱动 ReAct 循环（LLM ↔ 工具调用），把过程事件和最终结果以 NDJSON（每行一个 JSON 对象）写到 stdout。

- 后端：OpenAI 兼容的 `/v1/chat/completions` 接口
- 非流式：每次 LLM 调用是普通 POST，等完整响应返回（无 SSE、无增量片段）
- 无状态：单次 stdin → stdout，无历史、无会话、无落盘
- 无重试：HTTP 失败直接返回 error
- 无安全边界：工具不约束路径（可读写任意路径），shell 无黑名单；隔离责任完全交给调用方（容器/cgroup 等）
- 通信：stdin 进 / NDJSON 出 / stderr 写日志（`log/slog` 文本格式）
- 工具：`read_file` / `write_file` / `edit_file` / `shell`（全部 free 模式）
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
-max-tokens int          单次 LLM 调用的最大输出 token 数（默认 4096）
-model string            LLM 模型 id（必需）
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
{"type":"tool_use","name":"read_file","input":"{\"path\":\"a.go\"}"}
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

### `read_file`

读取文本文件，输出带行号标注。支持 `offset`/`limit` 按行范围读取。

| 参数 | 类型 | 必需 | 说明 |
|------|------|------|------|
| `path` | string | 是 | 相对 `-workdir` 或绝对路径 |
| `offset` | int | 否 | 起始行（1-based），默认 1 |
| `limit` | int | 否 | 最多返回行数，默认全部，上限 10000 |

约束：单文件最大 80000 字节（超出部分丢弃），输出超过 20000 字符截断。拒绝读取符号链接。

### `write_file`

覆盖写入文件，自动创建父目录，原子替换（temp + rename），保留原文件权限。

| 参数 | 类型 | 必需 | 说明 |
|------|------|------|------|
| `path` | string | 是 | 相对 `-workdir` 或绝对路径 |
| `content` | string | 是 | 完整文件内容 |

约束：`content` 最大 10 MiB。

### `edit_file`

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
- 子进程**继承父进程全部环境变量**（无白名单；`MINIAGENT_API_KEY` 等也会泄漏给子进程，调用方需自行评估风险）

## 退出码

| 码 | 含义 |
|----|------|
| 0 | 正常结束（含达到 `maxIterations` 上限、最终文本为空的场景） |
| 1 | 参数错误、API key 缺失、stdin 为空、主流程 `error` 事件 |

## 内部约束（常量）

| 常量 | 值 | 含义 |
|------|----|------|
| `maxIterations` | 20 | 单轮 LLM 调用上限 |
| `maxParallelTools` | 8 | 单步内并行工具并发上限 |
| `maxToolResultInHistory` | 2000 | 单条 tool 结果进入历史消息的字符数 |
| `maxReadFileBytes` / `maxReadFileChars` | 80000 / 20000 | 读文件字节 / 输出字符上限 |
| `maxLineLimit` | 10000 | `read_file` 的 `limit` 上限 |
| `maxWriteFileBytes` / `maxEditFileBytes` | 10 MiB | 写 / 编辑文件字节上限 |
| `maxShellOutputChars` | 20000 | shell 输出字符上限 |
| `shellTimeout` | 60s | shell 命令超时 |
| `maxChatBodyBytes` | 4 MiB | chat completions 响应 body 上限 |

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

# 查看版本
./bin/miniagent -version
```
