# miniagent

一个用 Go 标准库实现的最小 LLM agent。从 stdin 读取一个 prompt，驱动 ReAct 循环（LLM ↔ 工具调用），把过程事件和最终结果以 NDJSON（每行一个 JSON 对象）写到 stdout。

- 后端：OpenAI Chat Completions（config 模式 provider 各自的完整 chat/models URL）
- 默认非流式：每次 LLM 调用是普通 POST，等完整响应返回；传 `-stream` 改走 SSE，增量发 `text_delta`/`reasoning_delta` 事件
- 会话：默认无状态；`-save-session` 新建并落盘（id 内部生成），`-session <id>` 接续已存在会话；均以 jsonl append-only 落盘（首行 metadata + 每条 message），跨进程接续对话。二者互斥
- 最小重试：仅 429/500/502/503/504 + 网络错误自动重试 2 次（指数退避，支持 `Retry-After`）；其他 4xx/解析错误立即返回
- 平台：Linux/macOS/Windows。Unix 用 `setpgid`/`killpg`/`flock`/`O_NOFOLLOW`；Windows 用 `CREATE_NEW_PROCESS_GROUP` + `taskkill /T /F`、字节区间锁、Lstat 拒绝最终分量符号链接（`miniagent/platform_windows.go`）
- 通信：stdin 进 / NDJSON 出 / stderr 写日志（`log/slog` 文本格式）
- 工具：单模式 8 个：`read` / `write` / `edit` / `grep` / `glob` / `ast` / `web` / `shell`。隔离全交运行用户的 OS 权限（容器/低权 UID/文件权限），agent 层不做安全保障
- 取消：监听 `SIGINT`/`SIGTERM`，通过 context 取消正在进行的 LLM 调用和工具执行；**session 保存期间临时忽略信号**，避免截断 session 文件

## 架构：极简核心 + 开放钩子

miniagent 的核心是一个**无策略的 ReAct 循环**（`miniagent.Run`）：发 prompt → LLM 回工具则执行回灌 → 直到最终文本或撞迭代上限。核心**不做任何上下文管理**——压缩、记忆、溢出检测、token 估算都**不在循环里**。一切上下文策略经 `LoopHooks` 外挂：

| 钩子 | 触发点 | 用途 |
|------|--------|------|
| `BeforeLLM(StepInput) (StepOutput, error)` | 每步调 LLM 前 | 改写消息视图、压缩、注入记忆/RAG、提交持久化摘要、累加用量 |
| `AfterLLM(ctx, step, resp) error` | 每步 LLM 响应后 | 用量记账、静默溢出判定 |
| `OnBudget(ctx, step, BudgetInput, *Usage) error` | 每步用量累计后 | 零 usage 本地估算 fallback + 预算熔断（nil=不估算不熔断，见 `NewDefaultOnBudget`） |
| `OnLLMError(ctx, step, msgs, err) ([]Message, bool, error)` | LLM 调用失败时 | 失败恢复（典型：`ErrContextLength` 收紧历史重试一次；nil=error 直接上抛） |
| `OnToolUse(name, input) error` | 工具执行前 | 审批/拒绝（返回 `ErrToolDenied` 仅拒该工具，不终止循环） |
| `OnToolResult(name, callID, ToolResult) error` | 工具执行后 | 结果观察、事件下发 |
| `ShapeToolResult(name, callID, step, ToolResult) (string, error)` | 结果入历史前 | 覆盖 tool 消息 content（截断/落盘/RAG 摘要；nil=内置默认成型） |
| `OnDelta(step, kind, text) error` | 流式增量 | 实时输出 |

> 钩子契约、错误语义、不变量、时序的权威描述见 [ARCHITECTURE.md §4–§5](./ARCHITECTURE.md) 与 [HOOKS.md](./HOOKS.md)（每条溯源到 `file:line`）。本表为速查。

`StepOutput.View` 是发给 LLM 的消息；`Commit=true` 则同时替换运行 transcript（压缩语义）；`Persist` 追加到持久化增量；`ExtraUsage` 计入用量；`Compacted=true` 标记压缩（交互层据此 rewrite session）。

**provider 也是外挂**：`Run` 依赖 `LLM` 接口（`Do` 非流式 + `DoStream` 流式），不绑死任何供应商。内置 OpenAI Chat Completions provider；自定义 provider 实现 `LLM` 即可替换（本地、mock 等），核心零改动。压缩的摘要调用用更窄的 `Doer`（仅 `Do`）。工具是 `Tool.Call` 函数字段，核心不认识任何具体工具——`cfg.Tools []Tool` 由调用方自由组装。

**极简模式**：`BeforeLLM=nil` → 核心原样发送 transcript，零上下文管理。要压缩，挂 `NewCompaction`：

> 注：所有核心包已移出 `internal/`，外部 Go 模块可直接 `import "github.com/justphantom/miniagent/miniagent"`。

```go
import "github.com/justphantom/miniagent/miniagent/compaction"

before, _ := compaction.NewCompaction(compaction.CompactionOptions{
    Chat: chat, ContextWindow: 120000, Model: model, Auto: true, MaxTokens: maxTokens,
})
hooks := miniagent.LoopHooks{BeforeLLM: before} // 第二返回值 after 自 4.2.0 起=nil（溢出检测已并入 before）
result, err := miniagent.Run(ctx, chat, cfg, prompt, hooks, logger)
```

`compaction.NewCompaction` 封装摘要压缩引擎（超窗摘要中段 + 有损 fallback + 主动裁剪 + 静默溢出检测），是「压缩作为外挂」的默认实现，独立成 `miniagent/compaction` 子包。**不挂它即得无压缩的极简 agent；想换压缩策略，实现自己的 `BeforeLLM` 即可，核心零改动。** CLI 默认装配 `compaction.NewCompaction`，故命令行行为不变。

**自定义外挂示例**（注入一条长期记忆，仅本轮可见、不进 transcript）：

```go
hooks.BeforeLLM = func(ctx context.Context, in miniagent.StepInput) (miniagent.StepOutput, error) {
    view := append([]miniagent.Message{{Role: "user", Content: memoryNote}}, in.Msgs...)
    return miniagent.StepOutput{View: view}, nil // Commit=false：注入不持久化
}
```

会话压缩、会话记忆、RAG、用量记账等都可经这对钩子自行搭建，无需改核心。`OnToolUse`/`OnToolResult` 可外挂工具审批与执行审计；工具本身是 `Tool.Call` 函数字段，自由增删。

## 构建

```bash
make build      # 产出 bin/miniagent，version 来自 git describe
make test       # go test -race ./...
make verify     # verify-gate 五步（gofmt/build/vet/test -race/lint）
```

> `make verify` 含 `golangci-lint run`，非 Go 工具链自带，须先安装（无网环境注意预装）。

## 部署（systemd）

```bash
make build
sudo make deploy   # 装 bin/miniagent 为 systemd WebUI 服务并 enable --now
```

`deploy/deploy.sh` 完成：创建系统用户/组、装 `/usr/local/bin/miniagent`、播种 `/etc/miniagent/miniagent.json`（首次从 `config.example.json` 复制，**启动前需编辑填入 provider key**）、创建数据目录、渲染并安装 `miniagent.service`。**部署变量（workdir/config/user/group/session-dir）从 `deploy/.env.example` 读取，局域覆盖放 `deploy/.env`（git-ignored）。** 服务以 `-serve` 运行（WebUI），unit 含 `NoNewPrivileges`/`ProtectSystem`/`PrivateTmp` 硬化；agent 层 shell 仍无约束。

> `-version` 取 `git describe`（仅命中 annotated tag）。发版须用 `git tag -a v3.0.0 -m "..."`（annotated）且工作树干净；轻量 tag（`git tag v3.0.0`）或未提交改动会令 version 回落为短 sha。

## 环境变量

| 变量 | 用途 |
|------|------|
| `MINIAGENT_API_KEY` | API 密钥，作为 `Authorization: Bearer <key>` 发送。config `provider.key` 未设时必需 |
| `MINIAGENT_AUTO_APPROVE` | `-confirm-destructive` 在非交互/subagent 模式下默认拒绝破坏性工具；设为 `1` 则放行（等价于自动确认） |

## CLI 参数

**始终需要 config**（`-config <path>` 或默认 `~/.miniagent/miniagent.json`）。默认 config 查找路径：`~/.miniagent/miniagent.json`；不存在则报错。显式 `-config` 不存在同样报错。无裸 CLI 模式。

```
-config string           配置文件路径（默认查 ~/.miniagent/miniagent.json；不存在则报错）
-confirm-destructive     opt-in：写/edit 及危险 shell 执行前交互确认；非交互/subagent 模式下破坏性工具默认拒绝，除非 MINIAGENT_AUTO_APPROVE=1
-list-models             列出可用模型后退出，逐行输出 NDJSON 事件 {"type":"model","provider":"...","model":"..."}（静态 models 不发 GET，否则 GET models-url；-provider 可筛选单个 provider）
-log-level string        日志级别：debug|info|warn|error（默认 info）
-max-iterations int      单轮 LLM 调用上限（0=默认 20）
-metrics-step            每步输出 metrics NDJSON 到 stderr（step/transcript 长度/token 花费/压缩/LLM 请求数）
-model string            LLM model id（须与 -provider 成对传入，同传覆盖 defaults 对；只传其一报错）
-provider string         LLM provider 名（须与 -model 成对传入；-list-models 时单独用于筛选单个 provider）
-replay string           回放指定会话（读 session 文件重显过程，不调 LLM；与 -save-session/-session/-result-only 互斥）
-result-only             仅输出 result.text（subagent fork 用）；与 -stream、-save-session 互斥。失败输出 "error: <msg>" + 退出码 1
-save-session            新建会话并落盘（id 内部生成，stdout NDJSON 首条 `session` 事件输出；与 -session、-result-only 互斥）
-session string          接续已有会话的 id（在 session.dir 解析为 .jsonl；不存在则报错；仅允许字母/数字/-）
-serve                  启动 WebUI HTTP 服务（config web.listen，默认 127.0.0.1:8787；鉴权 web.key/$MINIAGENT_WEB_KEY；与 -list-models/-replay/-save-session/-session 互斥）
-stream                  流式输出（SSE）：增量发 text_delta/reasoning_delta 事件；默认非流式
-thinking string         思考级别（默认 off；启用时 provider 必声明 thinking{field,map}，wire 必经映射，见 config.example.json）
-version                 显示版本号并退出
-workdir string          工作目录（必填，须绝对路径；写工具边界 + shell 的 cwd，唯一来源，不从 config 取）
```

> 破坏性变更（-list-models 输出）：由纯文本 `provider/model_id` 行改为逐行 NDJSON 事件 `{"type":"model","provider":"...","model":"..."}`（model id 含 `/` 时文本拆分有歧义）；解析该输出的消费方需改为逐行 JSON 解析。部分失败语义不变：成功条目照常输出，退出码 1。

> 破坏性变更（provider/model 拆分）：config 的 `defaults`/`compaction` 两处模型设置改为 `provider` + `model` 两个独立字段（删除 `provider/id` 拼接串与旧三级回落链），适用**成对规则**：`defaults.provider`/`defaults.model` 必填；`compaction` 成对设置（可跨 provider）或整段留空（整体回落 defaults 对），只设其一报错。CLI 同步拆分：`-model` 改为纯 model id，新增 `-provider`，两者须成对传入（不传则以 config 为准）；`-list-models` 的单 provider 筛选从 `-model` 改为 `-provider`。旧格式 config 加载即报错。

> 破坏性变更（session 重构）：`-session` 改为**仅接续**——纯 id（仅允许字母/数字/-，禁 `/`/`.`/`\`），文件须已存在，不存在则报错退出；新增 `-save-session` 新建会话（id 内部生成，作为 stdout NDJSON 首条 `session` 事件输出）；二者互斥。移除 `-session` 的路径双语义与"文件不存在则新建"行为。subagent fork 改无状态（不再落盘会话、不再注入父 session id）。

> v3.2 破坏性变更：删除裸 CLI 模式（`-chat-url`/`-models-url`）与 `-migrate-session` 子命令；`-summary-request`/`-summarizer-prompt`/`-max-tokens-total`/`-context-window`/`-max-duration`/`-shell-timeout` 移出 CLI、改为仅 `config`（`defaults.*` / `run.*`）；`multi_edit` 工具并入 `edit`（`edits` 数组）。这些参数仍在 `miniagent.json` 可配，仅不再暴露为 flag。

> v3 破坏性变更：删除 `-base-url` / `-approve` / `-confine` 与 `$MINIAGENT_BASE_URL`；
> `-model` 支持 `provider/id` 前缀；`-session` 改 jsonl append-only + id 解析；session 文件格式不可向后兼容。

> **升级到 4.2.0（4 项 breaking）**：
> - **模型参数分层**：`models` 改对象数组 `[{"name":"x"}]`（旧 `["x"]` 加载报错）；`max_tokens`/`context_window`/`thinking` level 支持 `model > provider > global`，`http_timeout` 仅 `provider > global`；**取消 `-max-tokens` CLI**（改 config `run.max_tokens` 或 provider/model 级；三层全未配则不发 `max_tokens`、回落模型默认）。
> - **取消 `-system` CLI 与全局 `~/.miniagent/` 规则**：system prompt 仅来自 config `defaults.system_prompt`（未配则内置默认）+ 项目级 `workdir/.miniagent/`（**注：该层已于 v4.4.0 移除，见下文「项目专属配置」**）。全局 persona/rules 物进 `defaults.system_prompt`。
> - **提示词模板 config 化**：新增 `subagent_guidance`（`{config_path}`/`{mode}`）、`summary_create`/`update_instruction`（`{max_chars}`）、`summary_template`；`summarizer_prompt` 占位符 `%v`→`{max_chars}`。
> - **新增 `-replay <id>`**：离线回放 session 为同构 NDJSON 事件流。
>
> 详见 [CHANGELOG](./CHANGELOG.md)。

### 子命令

- `-version`：打印 `miniagent <version>`，退出码 0。
- `-list-models`：列出可用模型后退出，每行一条 NDJSON `model` 事件（`provider`/`model` 分离字段，model id 含 `/` 时也可靠解析）。
- `-serve`：启动 WebUI HTTP 服务（详见下文「WebUI（-serve）」）。

## WebUI（-serve）

`miniagent -serve` 启动内置 HTTP 服务（纯 stdlib + `go:embed` 单页，无构建链），把同一个 agent 通过浏览器暴露为对话界面。**注意：agent 层零安全（shell 无约束），服务是长驻进程——请只在自己可信的环境开放。前端已做 XSS 防护（markdown 渲染先 HTML 实体转义再替换），但 agent 层无安全保障，恶意用户仍可通过 shell 工具执行任意命令。**

- **鉴权**：登录页输入访问密钥，请求带 `x-api-key` 头（常数时间比较）。密钥取 `web.key` 或 `$MINIAGENT_WEB_KEY`；两者皆空则仅允许 **loopback** listen（无鉴权，本机信任），**非 loopback（如 `0.0.0.0:8787`）无 key 时启动直接拒绝**。远程开放建议配反向代理 + TLS。
- **配置**（`web` 段）：`listen`（默认 `127.0.0.1:8787`）、`key`、`allowed_hosts`（Host 头白名单的额外条目，反代域名/外部 IP 场景）。
- **请求来源防护**：全部请求校验 `Host` 头 ∈ 白名单（listen 地址 + loopback 变体；通配监听再加 hostname 与本机网卡地址；`web.allowed_hosts` 追加），拒绝 DNS rebinding；浏览器跨站请求（`Sec-Fetch-Site`/`Origin`）与 `POST /api/turn` 非 `application/json` Content-Type 一律 403/415，阻断无鉴权 loopback 模式下的 CSRF。
- **界面**：左侧会话列表（点击载入最近 200 条历史；会话项可删除 ✕、显示最后 assistant 消息预览），右侧聊天区（新会话/续跑表单 prompt/workdir 必填，provider/model/thinking 下拉——数据源 `GET /api/models`）。**markdown 渲染**（标题/粗体/斜体/代码/列表/引用/表格/删除线，XSS 实体转义保护），**流式累积→一次性渲染**（无闪烁）。**暗亮主题切换**（🌙/☀️，localStorage 记忆）。**中断**：发送中按钮变「停止」，AbortController 断开流（后端 req ctx 取消 → 存已执行部分）。**智能滚动**：上翻不拽回 + 「↓ 新消息」浮动按钮。**长文本折叠**：>20 行自动折叠+展开。**代码块复制按钮**。**header 显示**当前会话 id、累计 token 量（in=N out=M）、版本号、模型名。
- **API**：`POST /api/turn`（NDJSON 流式，契约与 CLI stdout 逐字节一致；`session` 空则新建、非空则续跑）、`GET /api/sessions`、`GET /api/sessions/{id}`、`GET /api/models`；除公开探测的 `GET /api/whoami` 外全部走 `x-api-key` 鉴权。
- **streaming**：`run.stream` 配置控制是否发 `text_delta`/`reasoning_delta`（默认非流式，界面显示最终文本与工具事件）。

### 主对话流程的前置检查

- 无法确定 endpoint/model（config 缺 provider/defaults.provider/defaults.model，或 config 解析失败）→ stderr 报错，退出码 1
- API key 缺失（provider.key / `$MINIAGENT_API_KEY` 均无）→ stderr 报错，退出码 1
- 缺 `-workdir` 或非绝对路径 → stderr 报错，退出码 1（workdir 恒必填，所有模式；唯一来源为该 flag）
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
| `text_delta` / `reasoning_delta` | 流式模式（`-stream`）下 LLM 输出增量 | `step`, `text`, `ts` |
| `tool_use` | 每次 LLM 请求工具调用（工具执行前） | `name`, `input`, `ts` |
| `tool_result` | 每次工具执行后 | `name`, `call_id`, `output`(截断), `truncated`, `is_error`, `exit_code`(仅 shell) |
| `result` | 主流程成功结束，**终态** | `text`, `model`, `input_tokens`, `output_tokens`, `steps`, `llm_requests`, `finish`, `compacted`, `thinking_downgraded` |
| `error` | 主流程失败，**终态** | `message` |

工具完整结果经 `trimForHistory` 裁剪后写入历史回灌 LLM；概要（截断到 `maxToolResultEventChars`）经 `tool_result` 事件输出到 stdout 供消费方观察。

### 字段说明

- `name`：工具名，见下文"工具清单"
- `input`：工具参数的原始 JSON 字符串（LLM 透传）
- `text`：完整回答文本。正常结束（`finishStop`）时为最终回复；达到 `maxIterations` 上限时，loop 会先注入一条系统消息请求总结（阶段 3），若 LLM 返回文本则 `text` 为该总结；若仍请求工具调用则回落为 `finishMaxIterations`（`text` 为空）。回答被 `max_tokens` 截断（`finish_reason:length`）时 `text` 是半截文本，无专门字段标记，仅在 stderr 日志有 `llm response truncated` 警告
- `model`：本次调用使用的模型 id
- `input_tokens` / `output_tokens`：累计的 token 用量
- `steps`：本轮 LLM 调用次数
- `llm_requests`：本次实际发出的 LLM 请求数（含降级/重试/总结步）
- `compacted`：本轮是否触发过摘要压缩（`true` 表示窗口溢出后执行了 `FitHistory` 摘要或 fallback 裁剪）
- `thinking_downgraded`：本轮是否因端点不支持 thinking 而降级（`true` 表示 thinking 字段被丢弃后重试了一次）
- `message`：错误描述

> `result` 事件中 `text`/`model`/`input_tokens`/`output_tokens`/`steps`/`llm_requests`/`finish`/`compacted`/`thinking_downgraded` 均不带 `omitempty`，为 0 也会出现键名，方便消费方稳定 parse。

### 输出示例

正常带工具调用：

```jsonl
{"type":"tool_use","name":"read","input":"{\"path\":\"a.go\"}"}
{"type":"tool_use","name":"shell","input":"{\"command\":\"go test ./...\"}"}
{"type":"result","text":"测试全部通过。","model":"gpt-4o","input_tokens":320,"output_tokens":48,"steps":3,"finish":"stop"}
```

纯文本无工具：

```jsonl
{"type":"result","text":"goroutine 是 Go 运行时管理的轻量级线程。","model":"gpt-4o","input_tokens":24,"output_tokens":18,"steps":1,"finish":"stop"}
```

达到 maxIterations 上限（无最终文本，仍输出累计 usage）：

```jsonl
{"type":"tool_use","name":"shell","input":"{\"command\":\"...\"}"}
{"type":"result","text":"","model":"gpt-4o","input_tokens":8200,"output_tokens":1500,"steps":20,"finish":"max_iterations"}
```

新建会话（`-save-session`，首条为 `session` 事件）：

```jsonl
{"type":"session","id":"20240105-120000-a1b2c3d4e5f6a7b8","model":"openai/gpt-4o","workdir":"/repo","provider":"openai","created":"2024-01-05T12:00:00Z"}
{"type":"tool_use","name":"read","input":"{\"path\":\"a.go\"}"}
{"type":"result","text":"...","model":"gpt-4o","input_tokens":100,"output_tokens":20,"steps":2,"finish":"stop"}
```

## 工具清单

单模式 8 个工具：`read` / `write` / `edit` / `grep` / `glob` / `ast` / `shell` / `web`。隔离全交运行用户的 OS 权限（容器/低权 UID/文件权限），agent 层不做 confine/白名单/`.git` 封锁等安全保障。工具参数为 JSON 对象。

> **v4.4.0 破坏性变更**：移除内置工具 `codemap`（目录树概览，与 glob+read 功能重叠）与 `todo`（`todo_create`/`todo_update`/`todo_list`，进程内任务清单，与核心零策略冲突），内置工具 10→6。迁移：`codemap` 改用 `glob`（结构）+ `read`（内容）组合；`todo` 改由模型在正文跟踪任务。详见 [CHANGELOG](./CHANGELOG.md)。

### `read`

读取文本文件，输出统一带行号标注（`N │ line` 格式，便于 `edit` 定位）。支持 `offset`/`limit` 按行范围读取。

| 参数 | 类型 | 必需 | 说明 |
|------|------|------|------|
| `path` | string | 是 | 相对 `-workdir` 或绝对路径 |
| `offset` | int | 否 | 起始行（1-based），默认 1 |
| `limit` | int | 否 | 最多返回行数，默认全部，上限 10000 |

约束：单文件最大 1 MiB（超出部分丢弃），输出超过 262144 字符截断。拒绝读取目录、非 regular 文件（FIFO/设备/socket）、二进制内容（含 NUL 字节），并拒最终分量符号链接（中间目录 symlink 仍跟随）。`offset` 超出文件行数返回 IsError。

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

约束：命中行上限 500、输出超 100000 字符截断；操作超时 30s。跳过 `.git`、符号链接、二进制与非 regular 文件（FIFO/设备/socket）。

### `glob`

递归列举匹配通配的文件路径，每行一个（相对 `-workdir`）。排除 `.git` 与符号链接。

| 参数 | 类型 | 必需 | 说明 |
|------|------|------|------|
| `pattern` | string | 是 | 通配模式：不含 `/` 时按文件名匹配（任意深度，如 `*.go`）；含 `/` 时按相对路径逐段匹配，`**` 段匹配任意层目录（如 `**/app.css`、`internal/**`） |
| `path` | string | 否 | 根目录，默认 `-workdir` |

约束：命中上限 500 条；操作超时 30s。

### `ast`

递归搜索 Go 源文件的**符号声明**（parser 解析 AST，不受注释/字符串干扰），输出 `path:lineno:kind Name`（方法为 `method (R) Name`）。查定义用此工具，查引用用 `grep`。

| 参数 | 类型 | 必需 | 说明 |
|------|------|------|------|
| `pattern` | string | 是 | 匹配符号名的正则（Go regexp 语法，如 `Tool`、`^Run`、`(?i)client`） |
| `kind` | string | 否 | 声明种类过滤：`func` / `method` / `type` / `interface` / `struct` / `const` / `var`（interface/struct 是 type 的细分） |
| `path` | string | 否 | 搜索根目录，默认 `-workdir` |
| `glob` | string | 否 | 文件名 include 过滤（filepath.Match，如 `*_test.go`） |

约束：命中上限 500 条；跳过 `.git`、`vendor`、`testdata`、非 `.go` 文件；操作超时同 `file_op_timeout`（默认 30s）；正则同 `grep` 的复杂度限制（防 ReDoS）。

### `shell`

通过 `sh -c` 执行命令，stdout+stderr 合并输出。

### `web`

GET 抓取 URL 并转为文本入上下文（查文档 / API 参考 / issue）。SSRF 防护内置（拒私网/环回/链路本地含云 metadata/组播/受限广播及 v4-mapped v6，DNS 全 IP 校验，重定向每跳重查）；超时 `run.web_timeout`（默认 30s）；仅 GET/HTTP(S)；拒非 text/* 与 application/json；body 1MiB 封顶，输出限幅+截断标记。非安全边界：GET 查询参数可携数据外传、响应内容直入上下文（prompt injection 面）。

- **SSRF 防护**（默认开）：拒绝私网（10/8、172.16/12、192.168/16）、环回、链路本地（含云 metadata 169.254.169.254）、组播/受限广播及映射 IPv4 的 IPv6 地址；DNS 解析出的每个 IP 都校验；重定向每跳重查目标主机。**非安全边界**——绕过 DNS rebind…[args omitted]

| 参数 | 类型 | 必需 | 说明 |
|------|------|------|------|
| `command` | string | 是 | shell 命令 |

约束：
- 命令超时 120 秒，超时后整进程组被 `SIGKILL` 清理（防止 `make`/`find` 等派生的孙子进程残留）
- 输出超过 100000 字符截断
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
- **摘要压缩**（config `run.context_window > 0`）：before 钩子每步先用 `applyCompactionBarrier` 屏障掉最新 `kind=summary` 之前的旧历史（仍留 session 文件），再用 `FitHistory` 估算超 window 80% 时把中段摘要为单条 `kind=summary` 消息（既进 context 又 append 落盘）。摘要失败/无中段回落有损 `compactHistory`，仍超则裁到最近轮，再超则报错终止（避免循环烧请求）。
- 文件损坏（非法 JSON 行、未知 role、tool 消息缺 `tool_call_id`、tool_calls/tool 配对断裂、超过 50 MiB 上限）→ stderr 报错 + 退出码 1，不静默丢弃历史。
- **信任假设**：session 文件内容原样进入 LLM 上下文，属于可信输入（与 system prompt 同级）；能写该文件的进程即可注入指令。
- 思考内容（reasoning）：wire 解析响应里的 `reasoning_content` / `reasoning`（双兼容），随 assistant 消息进入上下文并以 `reasoning_content` 回灌；**随 session 落盘**（与 content 同级）。
- 多轮接续：首轮 `-save-session`（从 stdout 首条 `session` 事件的 `id` 字段取生成的 id），后续轮 `-session <id>`；每次调用 stdin 的全部内容作为一个 turn 的完整 prompt。

## 退出码

| 码 | 含义 |
|----|------|
| 0 | 正常结束（含达到 `maxIterations` 上限、最终文本为空的场景） |
| 1 | 参数错误、API key 缺失、缺 workdir 或非绝对路径、stdin 为空、session 加载/追加失败、`-list-models` 失败、主流程 `error` 事件 |
| 130 | SIGINT/SIGTERM 取消的干净退出（128+SIGINT，POSIX 习惯；不 emit `error` 事件，区别于真故障） |

## 运行隔离（工程实践）

miniagent **不做任何 agent 层安全保障**（v5.0.0 删 `-mode`/confineWrap/白名单子命令工具/`.git` 封锁）：`shell` 恒注册、无路径限制，隔离**完全由运行用户的 OS 权限决定**，调用方负责：

- 用**专用低权限用户**运行；workdir 属该用户（或只读挂载），无关路径靠文件系统权限隔离。
- 密钥经 `$MINIAGENT_API_KEY` 环境变量或 config `provider.key` 注入。**注意：无论哪种方式，shell 子进程都可经 `/proc/$PPID/environ` 或读 config 文件拿到 key**（环境变量剥离只挡 `echo $VAR` 这类直读，挡不住 procfs）。因此密钥隔离**依赖运行用户的 OS 权限**：专用低权限用户、config 文件 `0600`、必要时容器/独立 UID。不要再依赖已移除的 `-key-file`。
- **session 保存受信号保护**：收到 `SIGINT`/`SIGTERM` 后，正在进行的 LLM/工具调用会被取消，但 `AppendMessages`/`RewriteMessages` 期间会临时忽略信号，保证 session 文件原子落盘，避免半写截断。
- 需要更强隔离时自行叠加容器 / 独立 UID / `hidepid` / 网络出口白名单等——这些**不在 miniagent 职责内**，由运行环境提供。

> 一句话：miniagent 信任其运行用户的权限边界；越权访问的闸门是 OS 用户与文件权限。此外，工具/输入/输出/请求体均有大小上限（见「内部约束」），`grep` 拒绝复杂正则，以降低误用与注入风险。

## 内部约束（常量）

下列带「config 覆盖键」的项可经 `config` 的 `run.*` 覆盖（策略化，`<=0` 或缺省用内置默认）：

| 常量 | 值 | config 覆盖键 | 含义 |
|------|----|------|------|
| `maxIterations` | 20 | —（CLI `-max-iterations`） | 单轮 LLM 调用上限 |
| `maxParallelTools` | 8 | `run.max_parallel_tools` | 单步内并行工具并发上限 |
| `maxToolResultInHistory` | 4000 | `run.max_tool_result_chars` | tool 结果进入历史消息的默认字符数（shell/grep/glob） |
| `maxFileResultInHistory` | 8000 | `run.max_file_result_chars` | read/edit 结果进入历史消息的字符数（代码内容，截断丢准确性） |
| `contextTrimToolChars` | 2000 | `run.context_trim_tool_chars` | context 超限降级时把 tool 结果压到的字符数 |
| `contextKeepRecent` | 4 | `run.context_keep_recent` | 摘要/有损压缩保留的最近轮数（首轮之外） |
| `summaryMaxChars` | min(5000, `context_window/5`) | `run.summary_max_chars` | 摘要式压缩单条 summary 的字符上限；默认随窗口缩放（方向 A，小窗口自适应），大窗口取 5000 |
| `summaryMaxTokens` | 2500（派生自 `summaryMaxChars/2`） | `run.summary_max_tokens` | 摘要请求输出 token 上限；<=0 由 `summaryMaxChars` 派生（CJK 最密口径，防中文摘要被 token 先截） |
| `maxGrepMatches` / `maxGlobEntries` | 500 / 500 | `run.grep_max_matches`（仅 grep；glob 无独立键） | grep 命中行 / glob 命中条数上限 |
| `maxReadFileBytes` / `maxReadFileChars` | 1 MiB / 262144 | `run.max_read_file_bytes`（chars = bytes/4） | 读文件字节 / 输出字符上限 |
| `maxLineLimit` | 10000 | — | `read` 的 `limit` 上限 |
| `maxWriteFileBytes` / `maxEditFileBytes` | 10 MiB | — | 写 / 编辑文件字节上限 |
| `maxShellOutputChars` | 100000 | `run.max_shell_output_chars` | shell 输出字符上限 |
| `maxSessionBytes` | 50 MiB | `run.max_session_bytes` | session 文件字节上限 |
| `shellTimeout` | 120s | `run.shell_timeout` | shell 命令超时（默认值，可被 config 覆盖） |
| `fileOpTimeout` | 30s | `run.file_op_timeout` | read/edit/grep/glob 文件操作超时（默认值，可被 config 覆盖） |
| `writeOpTimeout` | 30s | `run.write_timeout` | write 原子写入超时（默认值，可被 config 覆盖） |

## 项目专属配置

system prompt 来自 config `defaults.system_prompt`（未配则内置默认 `defaultSystemPrompt`，含工作流约束 + 「停留在工作目录内」的越界禁令）加上可选的 workdir 规则文件（`defaults.rules_file`，见下）；不再从 `.miniagent/persona.md`/`rules.md` 无条件自动加载。`.miniagent/` 目录现仅用于 session 存储（见「会话」节）。末尾无条件注入 CLI `-workdir` 的绝对路径行 + stay-inside 约束（软引导，非边界；真边界靠 OS 权限）。

> **破坏性变更**：项目级 `workdir/.miniagent/persona.md`/`rules.md` 自动加载已移除（继全局 `~/.miniagent/` 层之后的第二次收口，system prompt 来源统一为 config-only）。迁移：原 persona 内容直进 `defaults.system_prompt`（「取代默认」语义与 system_prompt 等价）；原 rules 为「追加」语义，物进 `system_prompt` 文本时需自行保留内置默认的工作流约束（或接受其丢失）。

**可选项目规则文件**（opt-in `defaults.rules_file`）：设为 workdir 下的纯文件名（如 `AGENTS.md`，**不含路径分隔符**）——文件存在则其文本追加到 system prompt（base 之后、subagent guidance 之前），不存在静默跳过；默认空 = 不启用（纯 config-only）。这是 v4.4.0 移除无条件 `.miniagent/rules.md` 后的 opt-in 回归：满足「项目规则 + 保留内置默认工作流约束」，且仅限 workdir 内纯文件名（拒 `..`/绝对路径/子目录，防越界读注入 prompt——default 的 workdir 约束在工具层，管不到此 main 层装配）；>64KiB 截断并 stderr 警告，读失败不致命。

### 提示词模板（config `defaults`）

所有面向 LLM 的提示词均可经 config 配置，占位符统一命名风格（`strings.NewReplacer` 替换，无 `%%` 转义）：

| 字段 | 占位符 | 说明 |
|------|--------|------|
| `system_prompt` | — | 主 system prompt（未配则内置默认） |
| `rules_file` | — | workdir 下纯文件名（如 `AGENTS.md`）；存在则追加到 system prompt（base 后/guidance 前），默认空=不启用 |
| `summary_request` | — | 撞迭代上限时注入的总结请求 |
| `summarizer_prompt` | `{max_chars}` | 摘要 system 全量 override（非空时忽略下列字段） |
| `summary_create_instruction` | `{max_chars}` | 摘要 CREATE 指令 |
| `summary_update_instruction` | `{max_chars}` | 摘要 UPDATE 指令 |
| `summary_template` | — | 摘要固定结构模板 |
| `subagent_guidance` | `{config_path}`/`{mode}` | subagent fork 引导（注入 system prompt 末尾；`config_path` 经 shell quote） |

空字段用内置默认。

> **破坏性变更**：`summarizer_prompt` 占位符由 `%v` 改为 `{max_chars}`（与其他提示词统一命名占位符）。现有用户自定义 `summarizer_prompt` 含 `%v` 须迁移为 `{max_chars}`。

### 配置示例（miniagent.json）

```json
{
  "providers": [
    {
      "name": "openai",
      "chat_url": "https://api.openai.com/v1/chat/completions",
      "models_url": "https://api.openai.com/v1/models",
      "key": "sk-xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx",
      "models": [
        {"name": "gpt-4o"},
        {"name": "gpt-4o-mini"}
      ]
    }
  ],
  "session": {"dir": ".sessions"},
  "defaults": {
    "provider": "openai",
    "model": "gpt-4o",
    "thinking": "off",
    "mode": "default",
    "summary_request": "请按以下格式总结：...",
    "summarizer_prompt": "你是代码审查助手。"
  },
  "run": {
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
    "context_trim_tool_chars": 1000
  },
  "compaction": {
    "provider": "openai",
    "model": "gpt-4o-mini"
  }
}
```

**关键字段说明**：
- `provider.chat_url` / `provider.models_url`：完整端点 URL（OpenAI Chat Completions，按 `kind`；kind 仅支持 `openai`/空）
- `provider.key`：按字面量读取；明文入 config，注意文件权限（建议 `0600`），或改用 `$MINIAGENT_API_KEY`
- `defaults.provider` / `defaults.model`：主会话 provider 名与 model id，**成对必填**（拆分后不再使用 `provider/id` 拼接串）
- `run.*`：覆盖内置常量（`<=0` 用内置默认）；duration 用 `30s`/`5m` 格式
- `compaction.provider` / `compaction.model`：摘要压缩模型。**成对可选**：同设可跨 provider；整段留空则整体回落 defaults 对；只设其一报错

## 完整调用示例

```bash
# 单次问答（默认读取 ~/.miniagent/miniagent.json；key 经 $MINIAGENT_API_KEY 或 config provider.key 注入）
echo "用一句话解释 goroutine" | \
  MINIAGENT_API_KEY=sk-xxx ./bin/miniagent -workdir "$PWD"

# 显式指定配置文件
MINIAGENT_API_KEY=sk-xxx ./bin/miniagent -config /path/to/miniagent.json ...

# 带工具 + 指定工作目录（shell cwd 为该目录；-workdir 须绝对路径）
echo "在当前目录跑测试并总结失败原因" | \
  MINIAGENT_API_KEY=sk-xxx ./bin/miniagent -workdir "$PWD/repo"

# 思考级别 + 摘要压缩（run.context_window 在 config 配置）
echo "重构这段代码" | \
  MINIAGENT_API_KEY=sk-xxx ./bin/miniagent -workdir "$PWD" -thinking high

# 限制整体墙钟 5 分钟（防 ReAct 循环失控烧 token；config run.max_duration）
echo "跑全量测试并总结" | \
  MINIAGENT_API_KEY=sk-xxx ./bin/miniagent -workdir "$PWD"

# subagent fork：把可并行子任务再调一次 miniagent（仅输出结果文本）
echo "<子任务>" | ./bin/miniagent -config <path> -workdir "$PWD" -result-only  # subagent 无状态，不落盘会话

# 查看版本
./bin/miniagent -version
```
