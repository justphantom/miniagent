# miniagent

一个用 Go 标准库实现的最小 LLM agent。从 stdin 读取一个 prompt，驱动 ReAct 循环（LLM ↔ 工具调用），把过程事件和最终结果以 NDJSON（每行一个 JSON 对象）写到 stdout。

- 后端：OpenAI 兼容的 chat completions 接口（config 模式 provider 各自的完整 chat/models URL）
- 默认非流式：每次 LLM 调用是普通 POST，等完整响应返回；传 `-stream` 改走 SSE，增量发 `text_delta`/`reasoning_delta` 事件
- 会话：默认无状态；`-save-session` 新建并落盘（id 内部生成），`-session <id>` 接续已存在会话；均以 jsonl append-only 落盘（首行 metadata + 每条 message），跨进程接续对话。二者互斥
- 最小重试：仅 429/500/502/503/504 + 网络错误自动重试 2 次（指数退避，支持 `Retry-After`）；其他 4xx/解析错误立即返回
- 权限模式（`-mode`）：default（默认）= 薄软约束（写工具限 workdir 子树、shell 拒 sudo/su 等 11 个提权器）；auto = 无限制。default 不构成安全边界——shell 可经 `cd`/绝对路径越界、写工具可符号链接逃逸，真隔离仍靠调用方（容器/低权限用户）
- 平台：Linux/macOS/Windows。Unix 用 `setpgid`/`killpg`/`flock`/`O_NOFOLLOW`；Windows 用 `CREATE_NEW_PROCESS_GROUP` + `taskkill /T /F`、字节区间锁、Lstat 拒绝最终分量符号链接（`internal/miniagent/platform_windows.go`）
- 通信：stdin 进 / NDJSON 出 / stderr 写日志（`log/slog` 文本格式）
- 工具：`read` / `write` / `edit` / `grep` / `glob` / `codemap` / `shell`，外加 `.miniagent/scripts.json` 声明的项目脚本工具（`script_<name>`）
- 取消：监听 `SIGINT`/`SIGTERM`，通过 context 取消正在进行的 LLM 调用和工具执行；**session 保存期间临时忽略信号**，避免截断 session 文件

## 构建

```bash
make build      # 产出 bin/miniagent，version 来自 git describe
make test       # go test -race ./...
```

> `-version` 取 `git describe`（仅命中 annotated tag）。发版须用 `git tag -a v3.0.0 -m "..."`（annotated）且工作树干净；轻量 tag（`git tag v3.0.0`）或未提交改动会令 version 回落为短 sha。

## 环境变量

| 变量 | 用途 |
|------|------|
| `MINIAGENT_API_KEY` | API 密钥，作为 `Authorization: Bearer <key>` 发送。config `provider.key` 未设时必需 |

## CLI 参数

**始终需要 config**（`-config <path>` 或默认 `~/.miniagent/miniagent.json`）。默认 config 查找路径：`~/.miniagent/miniagent.json`；不存在则报错。显式 `-config` 不存在同样报错。无裸 CLI 模式。

```
-config string           配置文件路径（默认查 ~/.miniagent/miniagent.json；不存在则报错）
-list-models             列出可用模型后退出，统一输出 provider/model_id（静态 models 不发 GET，否则 GET models-url；-provider 可筛选单个 provider）
-log-level string        日志级别：debug|info|warn|error（默认 info）
-max-iterations int      单轮 LLM 调用上限（0=默认 20）
-max-tokens int          单次 LLM 调用的最大输出 token 数（默认 4096）
-mode string             权限模式 default|auto（默认 default）：default 时 workdir 必填、写工具限 workdir、shell 拒 sudo/su 等 11 个提权器；auto 无限制
-model string            LLM model id（须与 -provider 成对传入，同传覆盖 defaults 对；只传其一报错）
-provider string         LLM provider 名（须与 -model 成对传入；-list-models 时单独用于筛选单个 provider）
-result-only             仅输出 result.text（subagent fork 用）；与 -stream、-save-session 互斥。失败输出 "error: <msg>" + 退出码 1
-save-session            新建会话并落盘（id 内部生成，stdout NDJSON 首条 `session` 事件输出；与 -session、-result-only 互斥）
-session string          接续已有会话的 id（在 session.dir 解析为 .jsonl；不存在则报错；仅允许字母/数字/-）
-stream                  流式输出（SSE）：增量发 text_delta/reasoning_delta 事件；默认非流式
-system string           系统提示词（默认为面向工程代码开发的代码向 prompt）
-thinking string         思考级别 off|minimal|low|medium|high|xhigh|max（默认 off，wire 透传 reasoning_effort）
-version                 显示版本号并退出
-workdir string          工作目录（default 模式写工具边界 + shell 的 cwd；也是 .miniagent/ 规则发现根）
```

> 破坏性变更（provider/model 拆分）：config 的 `defaults`/`compaction`/`memory` 三处模型设置改为 `provider` + `model` 两个独立字段（删除 `provider/id` 拼接串与旧三级回落链），适用**成对规则**：`defaults.provider`/`defaults.model` 必填；`compaction`/`memory` 成对设置（可跨 provider）或整段留空（整体回落 defaults 对），只设其一报错。CLI 同步拆分：`-model` 改为纯 model id，新增 `-provider`，两者须成对传入（不传则以 config 为准）；`-list-models` 的单 provider 筛选从 `-model` 改为 `-provider`。旧格式 config 加载即报错。

> 破坏性变更（session 重构）：`-session` 改为**仅接续**——纯 id（仅允许字母/数字/-，禁 `/`/`.`/`\`），文件须已存在，不存在则报错退出；新增 `-save-session` 新建会话（id 内部生成，作为 stdout NDJSON 首条 `session` 事件输出）；二者互斥。移除 `-session` 的路径双语义与"文件不存在则新建"行为。subagent fork 改无状态（不再落盘会话、不再注入父 session id）。

> v3.2 破坏性变更：删除裸 CLI 模式（`-chat-url`/`-models-url`）与 `-migrate-session` 子命令；`-summary-request`/`-summarizer-prompt`/`-max-tokens-total`/`-context-window`/`-max-duration`/`-shell-timeout` 移出 CLI、改为仅 `config`（`defaults.*` / `run.*`）；`multi_edit` 工具并入 `edit`（`edits` 数组）。这些参数仍在 `miniagent.json` 可配，仅不再暴露为 flag。

> v3 破坏性变更：删除 `-base-url` / `-approve` / `-confine` 与 `$MINIAGENT_BASE_URL`；
> `-model` 支持 `provider/id` 前缀；`-session` 改 jsonl append-only + id 解析；session 文件格式不可向后兼容。

### 子命令

- `-version`：打印 `miniagent <version>`，退出码 0。
- `-list-models`：列出可用模型后退出，每行 `provider/model_id`。

### 主对话流程的前置检查

- 无法确定 endpoint/model（config 缺 provider/defaults.provider/defaults.model，或 config 解析失败）→ stderr 报错，退出码 1
- API key 缺失（provider.key / `$MINIAGENT_API_KEY` 均无）→ stderr 报错，退出码 1
- `default` 模式无 `-workdir` → stderr 报错，退出码 1（用 `-mode auto` 放行）
- `-stream` 与 `-result-only` 同传 → stderr 报错，退出码 1
- `-save-session` 与 `-session` 同传 → stderr 报错，退出码 1
- `-save-session` 与 `-result-only` 同传 → stderr 报错，退出码 1（subagent fork 无状态，不落盘会话）
- stdin 为空 → stderr 报错 `miniagent: stdin is empty (send prompt via pipe or redirect)`，退出码 1

## NDJSON 输出结构

每个事件占一行，JSON 对象，`type` 字段区分种类。所有事件按时间顺序写入 stdout，最后以一个 `result` 或 `error` 事件结束（**终态**）。`text_delta`/`tool_use`/`tool_result` 为中间事件，不标志流程结束。`-save-session` 新建会话时，`session` 事件作为 stdout **首条**输出（在 Run 之前、所有 tool/delta/result 之前），供消费方程序化捕获会话 id。

### 事件类型

| type | 何时输出 | 字段 |
|------|---------|------|
| `session` | `-save-session` 新建会话时，作为 NDJSON **首条**事件（Run 之前） | `id`, `model`, `workdir`, `provider`, `created` |
| `text_delta` / `reasoning_delta` | 流式模式（`-stream`）下 LLM 输出增量 | `step`, `text` |
| `tool_use` | 每次 LLM 请求工具调用（工具执行前） | `name`, `input` |
| `tool_result` | 每次工具执行后 | `name`, `call_id`, `output`(截断), `truncated`, `is_error`, `exit_code`(仅 shell) |
| `result` | 主流程成功结束，**终态** | `text`, `model`, `input_tokens`, `output_tokens`, `steps` |
| `error` | 主流程失败，**终态** | `message` |

工具完整结果经 `trimForHistory` 裁剪后写入历史回灌 LLM；概要（截断到 `maxToolResultEventChars`）经 `tool_result` 事件输出到 stdout 供消费方观察。

### 字段说明

- `name`：工具名，见下文"工具清单"
- `input`：工具参数的原始 JSON 字符串（LLM 透传）
- `text`：完整回答文本。正常结束（`finishStop`）时为最终回复；达到 `maxIterations` 上限时，loop 会先注入一条系统消息请求总结（阶段 3），若 LLM 返回文本则 `text` 为该总结；若仍请求工具调用则回落为 `finishMaxIterations`（`text` 为空）。回答被 `max_tokens` 截断（`finish_reason:length`）时 `text` 是半截文本，无专门字段标记，仅在 stderr 日志有 `llm response truncated` 警告
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

新建会话（`-save-session`，首条为 `session` 事件）：

```jsonl
{"type":"session","id":"20240105-120000-a1b2c3d4e5f6a7b8","model":"openai/gpt-4o","workdir":"/repo","provider":"openai","created":"2024-01-05T12:00:00Z"}
{"type":"tool_use","name":"read","input":"{\"path\":\"a.go\"}"}
{"type":"result","text":"...","model":"gpt-4o","input_tokens":100,"output_tokens":20,"steps":2}
```

## 工具清单

文件与 shell 工具的约束取决于 `-mode`：default 模式下写工具（write/edit）限定在 workdir 子树、shell 拒 sudo/su 等 11 个提权器；auto 模式无任何约束。工具参数为 JSON 对象。

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

精确替换文件中的一段或多段文本。单段形态：`old_string` 须与文件精确匹配（含缩进和换行），缺省要求唯一出现（0 次或多次失败），`replace_all=true` 替换全部。多段事务形态：传 `edits` 数组（与 `old_string`/`new_string` 互斥），按序应用、全部成功才写盘、任一失败不改。拒绝编辑符号链接与非普通文件。

| 参数 | 类型 | 必需 | 说明 |
|------|------|------|------|
| `path` | string | 是 | 相对 `-workdir` 或绝对路径 |
| `old_string` | string | 单段必填 | 原文（精确匹配，含缩进和换行） |
| `new_string` | string | 单段必填 | 新文本 |
| `replace_all` | bool | 否 | true 时替换全部匹配处；缺省要求 old_string 唯一 |
| `edits` | array | 多段必填 | 多段事务替换列表，每项 `{old_string, new_string, replace_all?}`；与 old_string/new_string 互斥 |

约束：文件最大 10 MiB；保留原文件权限。`edits` 与 `old_string`/`new_string` 同传报错。先 `read` 查看内容再编辑。

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

### `codemap`

递归输出带缩进层级的目录树概览（目录标注子条目数），供 agent 低成本感知仓库整体布局。排除 `.git` 与符号链接。

| 参数 | 类型 | 必需 | 说明 |
|------|------|------|------|
| `path` | string | 否 | 根目录，默认 `-workdir` |
| `depth` | integer | 否 | 最大递归深度，默认 3；<=0 不限 |

约束：条目上限 500 条（超限尾部标注截断提示）；操作超时 30s。

### `shell`

通过 `sh -c` 执行命令，stdout+stderr 合并输出。

| 参数 | 类型 | 必需 | 说明 |
|------|------|------|------|
| `command` | string | 是 | shell 命令 |

约束：
- 命令超时 60 秒，超时后整进程组被 `SIGKILL` 清理（防止 `make`/`find` 等派生的孙子进程残留）
- 输出超过 20000 字符截断
- 退出码：成功 `ExitCode=0`；命令非 0 退出 `IsError=false` + `ExitCode=N`（命令的合法结果，非执行失败）；超时/启动失败 `IsError=true` + `ExitCode=-1`（`exitCodeNotSet`）。LLM 据 `ExitCode` 判命令成败
- 子进程**继承父进程环境变量，但显式剥离所有 `MINIAGENT_*` 前缀变量**（`API_KEY`/`BASE_URL` 等，防止 LLM 通过 `echo $MINIAGENT_API_KEY` 读取宿主配置与密钥）；另剥离变量名（大写后）含密钥关键字（KEY/TOKEN/SECRET/PASSWORD/CREDENTIAL/PWD/PASS/PASSPHRASE/AUTH/PAT）的第三方凭证变量（PAT 排除含 PATH 的路径类变量如 `PATH`/`GITHUB_PATH`）；**注意：该防护对 `/proc/<pid>/environ` 无效**——procfs 暴露的是 exec 时刻的环境快照，`cat /proc/$PPID/environ` 仍可读到 key。彻底防护需调用方隔离（容器/独立 UID）；其他第三方工具的敏感变量（如 `DATABASE_URL`）同样会泄漏，调用方需自行评估风险

## 会话（-save-session / -session）

缺省为无状态单次调用（不落盘）。两种落盘方式，**互斥**：

- **`-save-session`**：新建会话并落盘。id 由程序内部生成（`<时间戳>-<8 字节随机 hex>`，仅字母/数字/-），作为 stdout NDJSON **首条 `session` 事件**输出（与 jsonl 首行 metadata 同构），供下次接续。
- **`-session <id>`**：接续**已存在**的会话。id 在 `session.dir` 解析为 `<dir>/<id>.jsonl`；仅允许字母/数字/-（禁 `/`/`.`/`\` 等，杜绝路径穿越与扩展名注入）；文件不存在则报错退出（防 typo 创建垃圾会话）。
- 二者同传 → stderr 报错，退出码 1。

落盘格式与语义：

- 文件格式为 jsonl：首行 `{"type":"session",...}` metadata（id/model/workdir/provider/created），其后每行一条 `{"type":"message",...}`。metadata 一致性：`-session` 指向已存在文件时，workdir/model 不一致仅 stderr warn。
- Run 成功后 **append-only 追加本轮 NewMessages**（user prompt + assistant/tool 往返 + 最终回答 + 可能的 summary），不重写全量（权限 0o600）。
- Run 出错（LLM 失败/取消/超 window 终止）不追加——失败轮不固化，但工具副作用可能已发生且无记录。
- **并发**：同一 session 文件同一时刻仅单写者。append 经 `flock`（Windows 用字节区间锁）跨进程互斥，但 rename 换 inode 场景（机会性 rewrite）下跨进程并发不保证；多进程同写同一 session 应避免。
- **信号保护**：`AppendMessages`/`RewriteMessages` 执行期间临时忽略 `SIGINT`/`SIGTERM`，写完后恢复，避免 session 文件在半写状态被截断。
- **摘要压缩**（config `run.context_window > 0`）：Run 入口先用 `applyCompactionBarrier` 屏障掉最新 `kind=summary` 之前的旧历史（仍留 session 文件）；loop 每步前用 `FitHistory` 估算超 window 80% 时，把中段摘要为单条 `kind=summary` 消息（既进 context 又 append 落盘）。摘要失败/无中段回落有损 `compactHistory`，仍超则裁到最近轮，再超则报错终止（避免循环烧请求）。
- 文件损坏（非法 JSON 行、未知 role、tool 消息缺 `tool_call_id`、tool_calls/tool 配对断裂、超过 4 MiB 上限）→ stderr 报错 + 退出码 1，不静默丢弃历史。
- **信任假设**：session 文件内容原样进入 LLM 上下文，属于可信输入（与 system prompt 同级）；能写该文件的进程即可注入指令。
- 思考内容（reasoning）：wire 解析响应里的 `reasoning_content` / `reasoning`（双兼容），随 assistant 消息进入上下文并以 `reasoning_content` 回灌；**随 session 落盘**（与 content 同级）。
- 多轮接续：首轮 `-save-session`（从 stdout 首条 `session` 事件的 `id` 字段取生成的 id），后续轮 `-session <id>`；每次调用 stdin 的全部内容作为一个 turn 的完整 prompt。

## 退出码

| 码 | 含义 |
|----|------|
| 0 | 正常结束（含达到 `maxIterations` 上限、最终文本为空的场景） |
| 1 | 参数错误、API key 缺失、default 模式缺 workdir、stdin 为空、session 加载/追加失败、`-list-models` 失败、主流程 `error` 事件 |
| 130 | SIGINT/SIGTERM 取消的干净退出（128+SIGINT，POSIX 习惯；不 emit `error` 事件，区别于真故障） |

## 运行隔离（工程实践）

miniagent 的 `-mode default` 是**薄软约束，不构成安全边界**：写工具限定 workdir 子树（`path.Clean`+前缀，**不追符号链接**）、`read`/`grep`/`glob` 在 default 模式下被限制在 workdir 内、shell 词边界拒 11 个提权器（sudo|su|doas|pkexec|gsudo|run0|setpriv|nsenter|unshare|chroot|machinectl，仍可被变量拼接/拆分绕过）。shell 可经 `cd`/绝对路径访问 workdir 外。`-mode auto` 无任何限制。隔离**主要由运行用户的 OS 权限决定**，调用方负责：

- 用**专用低权限用户**运行；workdir 属该用户（或只读挂载），无关路径靠文件系统权限隔离。
- 密钥经 `$MINIAGENT_API_KEY` 环境变量或 config `provider.key` 注入。**注意：无论哪种方式，shell 子进程都可经 `/proc/$PPID/environ` 或读 config 文件拿到 key**（环境变量剥离只挡 `echo $VAR` 这类直读，挡不住 procfs）。因此密钥隔离**依赖运行用户的 OS 权限**：专用低权限用户、config 文件 `0600`、必要时容器/独立 UID。不要再依赖已移除的 `-key-file`。
- **session 保存受信号保护**：收到 `SIGINT`/`SIGTERM` 后，正在进行的 LLM/工具调用会被取消，但 `AppendMessages`/`RewriteMessages` 期间会临时忽略信号，保证 session 文件原子落盘，避免半写截断。
- 需要更强隔离时自行叠加容器 / 独立 UID / `hidepid` / 网络出口白名单等——这些**不在 miniagent 职责内**，由运行环境提供。

> 一句话：miniagent 信任其运行用户的权限边界；default 模式仅拦误操作，越权访问的闸门是 OS 用户与文件权限。此外，工具/输入/输出/请求体均有大小上限（见「内部约束」），`grep` 拒绝复杂正则，`script_<name>` 参数经转义且禁止 `-` 开头参数，以降低误用与注入风险。

## 内部约束（常量）

下列前 5 项可经 `config` 的 `run.*` 覆盖（S4 策略化，`<=0` 或缺省用内置默认）：

| 常量 | 值 | config 覆盖键 | 含义 |
|------|----|------|------|
| `maxIterations` | 20 | —（CLI `-max-iterations`） | 单轮 LLM 调用上限 |
| `maxParallelTools` | 8 | `run.max_parallel_tools` | 单步内并行工具并发上限 |
| `maxToolResultInHistory` | 4000 | `run.max_tool_result_chars` | tool 结果进入历史消息的默认字符数（shell/grep/glob） |
| `maxFileResultInHistory` | 8000 | `run.max_file_result_chars` | read/edit 结果进入历史消息的字符数（代码内容，截断丢准确性） |
| `contextTrimToolChars` | 1000 | — | context 超限降级时把 tool 结果压到的字符数 |
| `contextKeepRecent` | 6 | `run.context_keep_recent` | 摘要/有损压缩保留的最近轮数（首轮之外） |
| `summaryMaxChars` | 5000 | `run.summary_max_chars` | 摘要式压缩单条 summary 的字符上限 |
| `maxGrepMatches` / `maxGlobEntries` | 500 / 500 | grep 命中行 / glob 命中条数上限 |
| `maxReadFileBytes` / `maxReadFileChars` | 1MiB / 262144 | 读文件字节 / 输出字符上限 |
| `maxLineLimit` | 10000 | `read` 的 `limit` 上限 |
| `maxWriteFileBytes` / `maxEditFileBytes` | 10 MiB | 写 / 编辑文件字节上限 |
| `maxShellOutputChars` | 100000 | shell 输出字符上限 |
| `shellTimeout` | 120s | `run.shell_timeout` | shell/script 命令超时（默认值，可被 config 覆盖） |
| `fileOpTimeout` | 30s | `run.file_op_timeout` | read/edit/grep/glob 文件操作超时（默认值，可被 config 覆盖） |
| `writeOpTimeout` | 30s | `run.write_timeout` | write 原子写入超时（默认值，可被 config 覆盖） |

## 项目专属配置（`.miniagent/`）

在 `workdir` 下放 `.miniagent/` 目录，agent 启动时自动发现并把项目专属行为注入——核心引擎本身不感知任何具体项目，只「知道如何发现项目规则」：

项目规则文件（`persona.md`/`rules.md`/`scripts.json`/`memory.jsonl`）采用双层查找：优先从 `workdir/.miniagent/` 读，不存在则回退到 `~/.miniagent/`。优先级：workdir > home > 空。

| 文件 | 作用 |
|------|------|
| `.miniagent/persona.md` | 角色/语气/回答格式。存在时取代默认 system prompt 作身份基线（优先级 persona > rules > defaults.system_prompt） |
| `.miniagent/rules.md` | 项目专属约束（编码规范、禁止事项、审查清单），追加到 system prompt 的「## 项目规则」段 |
| `.miniagent/scripts.json` | `{"scripts":[{"name","command","description"}]}`；每条注册为 `script_<name>` 工具，复用 shell 的安全策略（mode 黑名单/env 剥离/超时/进程组） |
| `.miniagent/memory.jsonl` | 项目级记忆（结构化记录）。`read`/`write` 工具的保留路径 `path="memory"` 路由到此文件：read 渲染记录、write **追加**一条 `{type:"note",content}`（特殊语义：追加而非覆盖）。最近 10 条记忆注入 system prompt |

任一文件存在即触发 system prompt 末尾追加「（已加载 .miniagent/ 项目规则与脚本）」。该目录通常应加入 `.gitignore` 或按团队约定纳入版本控制。

```bash
# 项目仓库结构示例
repo/
  miniagent.json            # provider/defaults/run 配置
  .miniagent/
    persona.md              # 「你是 repo 的 Go 维护者…」
    rules.md                # 「禁止提交未跑 go test 的改动…」
    scripts.json            # {"scripts":[{"name":"test","command":"go test ./...","description":"跑测试"}]}
    memory.jsonl            # {"type":"lesson","content":"…"}…
```

### 配置示例（miniagent.json）

```json
{
  "providers": [
    {
      "name": "openai",
      "chat_url": "https://api.openai.com/v1/chat/completions",
      "models_url": "https://api.openai.com/v1/models",
      "key": "sk-xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx",
      "models": ["gpt-4o", "gpt-4o-mini"]
    }
  ],
  "session": {"dir": ".sessions"},
  "defaults": {
    "provider": "openai",
    "model": "gpt-4o",
    "thinking": "medium",
    "mode": "default",
    "summary_request": "请按以下格式总结：...",
    "summarizer_prompt": "你是代码审查助手。"
  },
  "run": {
    "workdir": "./repo",
    "max_tokens": 4096,
    "max_iterations": 20,
    "context_window": 128000,
    "max_duration": "5m",
    "shell_timeout": "30s",
    "file_op_timeout": "20s",
    "write_timeout": "10s",
    "http_timeout": "180s",
    "max_tool_result_chars": 2000,
    "max_file_result_chars": 8000,
    "max_parallel_tools": 8,
    "context_keep_recent": 6,
    "summary_max_chars": 2000,
    "summary_max_tokens": 512,
    "max_read_file_bytes": 524288,
    "max_shell_output_chars": 100000,
    "max_session_bytes": 10485760,
    "grep_max_matches": 500,
    "memory_recent_n": 10,
    "context_trim_tool_chars": 1000
  },
  "compaction": {
    "provider": "openai",
    "model": "gpt-4o-mini"
  },
  "memory": {
    "provider": "openai",
    "model": "gpt-4o-mini"
  }
}
```

**关键字段说明**：
- `provider.chat_url` / `provider.models_url`：完整 OpenAI 兼容端点
- `provider.key`：按字面量读取；明文入 config，注意文件权限（建议 `0600`），或改用 `$MINIAGENT_API_KEY`
- `defaults.provider` / `defaults.model`：主会话 provider 名与 model id，**成对必填**（拆分后不再使用 `provider/id` 拼接串）
- `run.*`：覆盖内置常量（`<=0` 用内置默认）；duration 用 `30s`/`5m` 格式
- `compaction.provider` / `compaction.model`：摘要压缩模型。**成对可选**：同设可跨 provider；整段留空则整体回落 defaults 对；只设其一报错
- `memory.*`：会话结束自动抽取项目记忆到 `.miniagent/memory.jsonl`。`memory.provider` / `memory.model` 规则同 compaction（成对可选，同空回落 defaults 对），与主会话/compaction 同 provider 时复用 client（按 provider 名去重）。`auto_update` 默认 `true`，仅在有过工具调用的会话触发，best-effort（失败仅告警）；无 workdir 跳过。`max_per_session` 单会话上限条数（默认 3）；`extract_prompt` 可覆盖默认提示词（**v3.5.1 起仅支持 `%d`(条数)/`%s`(已有记忆) 两个占位符；对话内容改为以 user message 传入，旧版第 3 个 `%s` 占位符已移除**）。抽取不计入 token 预算。注意：抽取自 transcript，已剔除含 API key 字面量的记录，但并非安全边界，密钥隔离仍依赖 OS 权限。

## 完整调用示例

```bash
# 单次问答（默认读取 ~/.miniagent/miniagent.json；key 经 $MINIAGENT_API_KEY 或 config provider.key 注入）
echo "用一句话解释 goroutine" | \
  MINIAGENT_API_KEY=sk-xxx ./bin/miniagent -mode auto

# 显式指定配置文件
MINIAGENT_API_KEY=sk-xxx ./bin/miniagent -config /path/to/miniagent.json ...

# 带工具 + 指定工作目录（default 模式：写工具限 ./repo，shell cwd 为 ./repo，.miniagent/ 从 ./repo 发现）
echo "在当前目录跑测试并总结失败原因" | \
  MINIAGENT_API_KEY=sk-xxx ./bin/miniagent -workdir ./repo

# 思考级别 + 摘要压缩（run.context_window 在 config 配置）
echo "重构这段代码" | \
  MINIAGENT_API_KEY=sk-xxx ./bin/miniagent -workdir . -thinking high

# 限制整体墙钟 5 分钟（防 ReAct 循环失控烧 token；config run.max_duration）
echo "跑全量测试并总结" | \
  MINIAGENT_API_KEY=sk-xxx ./bin/miniagent -mode auto -workdir .

# subagent fork：把可并行子任务再调一次 miniagent（仅输出结果文本）
echo "<子任务>" | ./bin/miniagent -config <path> -workdir . -mode default -result-only  # subagent 无状态，不落盘会话

# 查看版本
./bin/miniagent -version
```
