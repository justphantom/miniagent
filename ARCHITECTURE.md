# miniagent 系统架构

本文按当前代码（`go.mod` → `module github.com/justphantom/miniagent`, go 1.25）描述 miniagent 的运行时架构。定位：一个**极简、无内置策略的 ReAct agent 核心库** + 一个从 stdin 读 prompt、向 stdout 输出 NDJSON 事件的 CLI。

## 1. 设计总纲

核心思想一句话：**循环本身不做任何上下文管理**。所有上下文策略（压缩、记忆、溢出检测、预算、用量记账、工具结果成型、事件输出、步骤观测）都经 `LoopHooks` 外挂实现。不挂钩子即得到一个把 transcript 原样发给 LLM 的极简 agent；挂上 `NewCompaction` 与默认钩子工厂即恢复完整能力。

由此带来两条贯穿全系统的约束：

- **核心循环 (`loop.go:Run`) 无策略**：`LoopConfig` 只含循环本体所需（模型/系统提示/工具/历史/限额/流式/思考/thinking 映射）；预算、tool 输出上限、并行度、tool-output 落盘目录等是**策略配置载体**——核心不读它们，由 `NewDefault*` 钩子工厂消费。
- **策略皆可插拔**：压缩 (`NewCompaction`)、预算 (`OnBudget`)、失败恢复 (`OnLLMError`)、工具结果成型 (`ShapeToolResult`)、步骤观测 (`OnStep`) 都是钩子，核心不绑定特定实现。

## 2. 分层与目录

核心（`miniagent`）已按职责拆成若干子包；`config/`/`provider/`/`text/` 与核心同级。依赖单向无环：**所有子包 → 核心（+ `text` 纯工具）**，无任何跨子包横向引用；`cmd` → 核心/子包。库化后所有包均可被外部 Go 模块直接导入。

```
cmd/miniagent/        CLI 入口层（库的装配消费者）
  main.go             组装主线（config→Resolve→key→workdir/session→system prompt→Run→save）
  setup_run.go        loopCfg / compactionOptions / warnNoBudgetFuse（Run 组装辅助）
  setup.go            config 查找、key 解析、hooks 构建、退出码
  setup_http.go       buildLLM / buildDoer（HTTP client 注入，provider Kind 分派）
  setup_providers.go  buildRuntimeClients / providerKind 分派
  setup_transport.go  newHTTPClient / newHTTPTransport
  models.go           modelSource / listAllModels（模型清单聚合，Kind 分派）
  tools.go            buildTools：注册内置工具（单模式，shell 恒注册）
  session.go          -save-session/-session 解析与互斥
  prompts.go          默认 system prompt、subagent 引导、workdir 绝对路径注入
  replay.go           -replay：离线重放会话（不调 LLM、不用 key、不落盘）
  stdin.go            读 prompt（空 stdin 交互引导）

miniagent/   核心库（零外部依赖，纯标准库）
  loop.go             Run：ReAct 主循环（defer 统一写命名返回，captureDowngrade 固化降级）
  loop_extra.go       callLLMWithDowngrade / callLLMOnce（含降级与 panic 兜底）
  loop_tools.go       runToolsParallel / safeCall（handleToolCalls 已拆至 tool_handler.go）
  tool_handler.go     handleToolCalls / fillPlaceholderTail（工具执行业务流程）
  loop_api.go         domain 类型：LoopHooks/LoopConfig/Tool/ToolResult/StepInput/StepOutput/BudgetInput/Result/ThinkingMapping
  message.go          Message / ToolCall / SessionMeta / ValidateToolPairing
  token_estimate.go   EstimateTokens / EstimateResponseTokens / TrimForHistory / SystemOverheadTokens（纯估算与裁剪，核心持有以消除子包横向依赖）
  errors.go           哨兵 error：ErrBudgetExceeded/ErrContextLength/ErrThinkingUnsupported/ErrToolDenied
  limits.go           Limits 结构：运行时覆盖内置默认
  provider_api.go     LLM / Doer / Provider 接口
  overflow.go         context 超限识别（正则+排除，IsContextLengthError）
  platform*.go        平台原语：flock / O_NOFOLLOW / 进程组 kill（windows 分文件）

miniagent/compaction/  压缩引擎子包（「压缩作为外挂」的默认实现）
  compacting.go       NewCompaction（封装为 before/after 钩子，after 自 4.2.0 起=nil）+ CompactingHook/CompactingInput/CompactingOutput/applyCompactingHook
  assemble.go         摘要 prompt/模板：buildSummarizerSystem / summarizeMiddle / deriveSummaryMaxChars/MaxTokens
  budget.go           FitHistory 流水线、ContextBudget、applyContextStrips（P1/P4/P6/P7/P8'/P9b/P11）、tail 预算
  budget_tail.go      jointTailBudget / preserveRecentTokens / estimateRoundTokens
  budget_const.go     压缩常量（summary/tail/reasoning/args 阈值）
  split.go            applyCompactionBarrier、compactWithSummary、selectTailByTokens
  compaction_split.go lastApplicableUsageIndex、isUsageOverflow（静默溢出判定）
  history_*.go        主动裁剪各阶段：reasoning(args)/dedup(shell)/dedup_read(read)/reasoning(P1/P7)

miniagent/policy/      默认策略工厂
  loop_hooks_default.go NewDefaultOnBudget / NewDefaultOnLLMError / NewDefaultShapeToolResult
  trim.go             TrimForHistory（薄转发到核心 miniagent.TrimForHistory）
  history_util.go     trimHistoryForContext（ErrContextLength 收紧；token 估算已下沉核心）
  tool_output_store.go 工具输出落盘 store（超 limit 全文写盘 + 过期清理）
  confirm_on_tool_use.go 破坏性工具确认门禁（opt-in）

miniagent/session/     会话持久化子包
  session.go          LoadSession / AppendMessages（flock 跨进程锁 + append-only + 预序列化拒绝超限；SessionMeta 别名自核心）
  session_rewrite.go  RewriteMessages（全量原子改写：CLI 主路径每轮落盘 + compaction 后真正丢弃旧历史）
  validate.go         ValidateSessionID / validateSessionMessage（ValidateToolPairing 已上移核心）
  lock_*.go           平台锁原语

miniagent/tools/       内置工具实现
  tool_read/write/edit/grep/glob/ast.go  文件与符号搜索六工具（ast 为 Go 符号声明搜索）
  tool_shell.go         命令执行（shell 恒注册，进程组隔离 + 超时 + 输出滑窗）
  tool_web.go         网页抓取（opt-in，SSRF 防护 + HTML 标签剥离）
  tool_helpers.go     路径解析、schema 构造、decodeStrict
  output_accum.go     shell 输出字节滑窗累积器（保尾部 + 落盘 spill）
  run_limited.go      drainPipe / waitOutputTimeout / UTF-8 边界缝合
  web_charset.go / web_text.go  网页字符集解码与 HTML 文本提取

miniagent/event/       NDJSON 事件编码子包（session/tool_use/tool_result/result/error/delta/model/llm_request）
miniagent/metrics/     OnStep 默认消费者：NewStepEmitter（per-step NDJSON 到 writer，best-effort 不终止循环）

config/      配置解析子包（库化后独立顶层）
  config.go           Config / Defaults / Run / Provider / CompactionProvider 类型（含 v5.0.0 废弃键 Mode/Cache/Confine*）
  config_load.go      LoadConfig / decodeConfigStrict（DisallowUnknownFields + 废弃键容忍）
  validate.go         validateConfig / validateThinking / thinkingFieldBlacklist
  resolve.go          Resolve：cli>config>builtin 三态裁决产出 Resolved
  resolve_helpers.go  三态裁决辅助：pickInt / findModelConfig / ParseDuration

text/                  纯文本工具（NowMs / Truncate / TruncateHeadTail / ValidateURL）
provider/openai/       OpenAI 兼容 provider（Chat Completions）
  wire.go             buildChatBody / parseChatResponse 序列化层
  client.go           ChatClient：非流式 + models 列表，重试/降级
  stream.go / stream_parse.go   流式 SSE
  models.go           ListModels
  retry.go            重试退避
provider/httpretry/    共享重试原语（429/5xx 退避，厂商无关）
```

入口 `main.go` 自上而下：**flag → config → Resolve → key → workdir/session → assembleSystemPrompt（config-only + subagent guidance + workdir 绝对路径）→ 注入 `Limits` → buildLLM/buildDoer → buildTools → loopCfg → NewCompaction → assembleHooks → Run → 落盘**。`-replay` 在 Resolve 之后、validateConversation 之前短路（读会话文件离线重放）。`-metrics-step` 通过 `OnStep = metrics.NewStepEmitter(w).Emit` 挂步骤观测。

## 3. 核心循环 `Run`

`loop.go:Run` 是 ReAct 循环（`maxIterations` 默认 20）：

```
复制 History → 追加 user prompt
for step in 1..iterLimit:
    ctx 取消检查
    toSend, ... = applyBeforeLLM(hooks)              # 开放缝①：压缩/注入/透传
    resp, downgraded, reqs, err = callLLMWithDowngrade  # 调 LLM（thinking 降级）
    if downgraded: captureDowngrade（清 thinking 字段，固化跨步）
    if err != nil && hooks.OnLLMError != nil:
        recovered, retry = hooks.OnLLMError(msgs, err)  # 默认 OnLLMError 收紧历史
        if retry: 重试一次本步（核心不递归）；否则 error 上抛
    recordStepUsage → 累加真实 usage → hooks.OnBudget(...)    # 开放缝③：预算熔断
    hooks.OnStep(snap)                                   # 开放缝④：步骤观测（metrics step）
    hooks.AfterLLM(step, resp)                          # 开放缝②
    if 无 tool_calls:
        appendMsg(最终文本+真实usage); return finishStop
    msgs = handleToolCalls(...)                         # 执行工具并回灌（并行 + 配对补全）
    if step == iterLimit: 注入 roleSystem summaryRequest; return
return finishMaxIterations
```

关键设计：

- **`msgs` vs `newMsgs` 分离**：`msgs` 是 LLM 上下文（裁剪只动它），`newMsgs` 只记本轮新增（main 在 Run 末尾经 `RewriteMessages` 将 `result.Messages` 全量落盘——首行 `meta.LLMRequests` 跨轮累积需更新，append-only 无法改首行）。两者经 `appendMsg` 同步追加，保证上下文与持久化一致。
- **`Result` 命名返回 + defer 统一写**：Usage/Messages/NewMessages/thinkingDowngraded/compacted 全部由 defer 写入，各 return 只设差异字段（Steps/Text/Finish），杜绝遗漏字段致会话持久化丢消息。
- **thinking 降级固化**：`captureDowngrade` 在降级后清空 `cfg` 的 thinking 字段 + 置 `thinkingDowngraded=true`，跨步保持（主路径/OnLLMError 重试/总结闭包共用同一处理点）。
- **ErrContextLength 失败恢复（外挂）**：经 `OnLLMError` 钩子承载，核心自身不做错误恢复。默认 `NewDefaultOnLLMError` 对 `ErrContextLength` 调 `trimHistoryForContext`（清 reasoning + 压 tool content）收紧后 `retry=true`，核心重试一次本次调用（不递归）。
- **撞上限总结**：迭代上限前一步若刚执行工具，注入内部 `summaryRequest`（`roleSystem`，不持久化）引导 LLM 输出最终文本。

## 4. 开放缝：`LoopHooks`

`loop_api.go:LoopHooks` 是核心与外部能力的唯一缝口，共 **9** 个字段，皆可 nil：

| 钩子 | 时机 | 职责 | nil 行为 |
|---|---|---|---|
| `BeforeLLM` | 每步调 LLM 前 | 改写发给 LLM 的消息视图、收缩 transcript、注入记忆/RAG、提交持久化摘要、累加用量；`NewCompaction` 在此做静默溢出判定（从历史真实 usage 推断 Force）+ 压缩 | 透传原 transcript |
| `AfterLLM` | 每步响应后 | 用量记账、自定义观察（`NewCompaction` 自 4.2.0 起 after=nil，溢出判定已并入 before） | 不通知 |
| `OnBudget` | 累加 usage 后 | 零 usage 本地估算 fallback + 预算熔断（`ErrBudgetExceeded`） | 不估算不熔断（仅累加真实 usage） |
| `OnLLMError` | LLM 调用失败时 | 失败恢复（典型：`ErrContextLength` 收紧历史重试一次） | error 直接上抛终止循环 |
| `OnToolUse` | 工具执行前 | 事件输出/拒绝（`ErrToolDenied` 仅拒该工具，可加确认门禁） | 不通知 |
| `OnToolResult` | 工具执行后 | 结果事件输出（含 ExitCode/IsError） | 不通知 |
| `ShapeToolResult` | 结果入历史前 | 覆盖 tool 消息 content（截断/落盘/RAG 摘要） | 内置默认成型 |
| `OnDelta` | 流式增量 | 推 text/reasoning 增量事件 | 非流式不触发 |
| `OnStep` | 每步结束 | 步骤级观测（transcript 增长、token 斜率、压缩次数、LLM 请求数） | 不通知 |

`StepOutput`（BeforeLLM 回参）的语义是策略外挂的关键契约：

- `View`：本轮实际发给 LLM 的消息（必填）。
- `Commit=true`：核心把运行 transcript 替换为 View（压缩场景）。
- `Persist`：额外持久化增量（如 summary），带 `Kind` 的条目替换 newMsgs 中同 Kind 旧条目（多次压缩只留最新 summary）。
- `ExtraUsage`/`Compacted`：累加用量、标记压缩（观测用；cmd 落盘每轮恒 rewrite，不据此决定）。

## 5. 上下文管理（压缩引擎）

`compaction/compacting.go:NewCompaction` 把整套压缩封装为 `(before, after)` 钩子对（`after` 自 4.2.0 起=nil）。`before` 每步从历史最新真实 usage 推断 `Force`（静默溢出判定），再做 `applyCompactionBarrier` + `FitHistory`。溢出判定不依赖跨步闭包状态——before/after 无共享可变状态，单 Run 内串行调用，可被多 Run 安全复用。

**压缩屏障**：`applyCompactionBarrier` 定位最新一条 `Kind=="summary"` 消息，只把它及之后的消息进 context，之前的旧历史仍留 session 文件（机会性 rewrite 才真正丢弃）。

**FitHistory 流水线**（`compaction/budget.go:FitHistory`）：

1. **4/5 阈值门控**：未超 `ContextWindow*4/5` → 仅跑主动裁剪，返回。`Force=true`（上一步真实 usage 命中溢出）跳过门控直接进摘要分支。
2. **摘要中段** `compactWithSummary`：保留最早 1 轮 + 按 token 预算选 tail（`selectTailByTokens`，边界轮可 split/shrink 贴合预算），中段经 `applyCompactingHook`（可注入上下文/一次性替换 summarizerPrompt）后调 `Summarize` 压成单条 `KindSummary` 消息。
3. **有损 fallback** `compactHistory`：摘要失败/无中段时回落。
4. **仍超 → `trimRecentRounds`**：裁到最近轮。
5. **再超 → 报错终止**：避免死循环。

**active 主动裁剪**（`applyContextStrips`，7 个阶段，均只改 context-side copy，不动 newMsgs/持久化，保留 keepToolArgs 窗口内最近 N 条 assistant 消息不动）：

| 阶段 | 阶段名 | 说明 |
|---|---|---|
| P1 | `stripStaleReasoning` | 清非最近 N 条 assistant 的 Reasoning |
| P7 | `truncateKeptReasoning` | 窗口内超长 Reasoning 头尾截断（默认 4000 rune） |
| P4 | `stripStaleToolArgs` | 非窗口 write/edit args 压缩为前缀占位 |
| P6 | `dedupReadResults` | 同 (path,offset) 重复 read 结果折叠（保留**最后一次**、折叠早次：旧内容已被新读覆盖，keep-last 是 P6b 语义要求） |
| P11 | `foldStaleReadResults` | 同 path 后有新成功写入/编辑时，读结果折叠为占位 |
| P8' | `foldStaleWriteEditArgs` | 同 path 后有后成功写入/编辑时，前写/edit args 折叠 |
| P9b | `dedupShellCommands` | 归一化命令签名重复 shell 折叠（仅保最新一次） |

**silent overflow 检测**（`compaction/compaction_split.go:isUsageOverflow`）：检测上一步真实 usage 是否已溢出窗口（`lastApplicableUsageIndex` 定位最近一条可用 usage，考虑 summary 消息的边界），命中即下一步设 `Force=true` 直接摘要。

> **能力边界**：silent-overflow 检测依赖端点返回真实 `usage`。不回 usage 的流式端点上所有 assistant.Usage 为零 → 无锚 → 门控自动回退本地估算（`policy.EstimateTokens`），但 Force 检测依旧无锚，该端点三层防御只剩两层（本地估算门控 + 被动 ErrContextLength 重试）。配置键 `run.compaction.auto` 的静默检测在此类端点失效属已知边界，非回归。

## 6. 工具系统

`loop_api.go:Tool` 定义工具契约（name/description/parameters schema + `Call func` 执行函数）；`loop_api.go:ToolResult` 为执行结果（Content/ExitCode/IsError）。内置工具实现均在 `miniagent/tools/`：

| 工具 | 行为 | 关键机制 |
|---|---|---|
| `read` | 读文件 | offset/limit 行号输出、拒绝二进制、拒绝非 regular 文件 |
| `write` | 写文件 | 覆盖写、按 write_timeout |
| `edit` | 精确替换 | old→new 唯一匹配、多段事务 |
| `grep` | 递归正则搜索 | 输出 path:lineno:line，可 glob 过滤 |
| `glob` | 路径匹配 | filepath.Match 模式；含 `/` 按相对路径逐段匹配，支持 `**` |
| `ast` | Go 符号声明搜索 | go/parser 解析，输出 path:lineno:kind Name，kind 过滤 |
| `shell` | 命令执行 | 默认超时 **120s**，进程组隔离、按 shell_timeout |
| `web` | 网页抓取 | SSRF 防护（拒私网/环回/链路本地）、重定向每跳重查、HTML 标签剥离、body/输出双截断 |

v5.0.0 起单模式，8 工具（上表全部）恒注册；`shell` 无任何 agent 层约束。原 `git`/`go`/`npm`/`golangci-lint` 白名单子命令工具与 `rename`/`delete` 工具已删（外部命令一律走 `shell`）。安全模型见 §10。

工具执行经 `handleToolCalls` + `runToolsParallel`（并行度受 `MaxParallelTools` 约束，默认 5）+ `safeCall`（panic 兜底）。

## 7. 客户端层

通过 `Provider` 配置分派到 `provider/` 下对应实现，核心经 `LLM` / `Doer` 接口调用，不感知底层协议：

- **OpenAI 兼容**（`openai/`）：Chat Completions，`wire.go` 序列化层（含 thinking / tools），`client.go`（非流式+重试/降级）+ `stream.go`/`stream_parse.go`（SSE）+ `models.go`（动态 GET + 静态回落，单客户端 `ListModels`）。
- **共享重试**（`httpretry/`）：厂商无关的 429/5xx 指数退避 + `retry_after` 解析，openai 使用。

配置侧 `Provider.Kind` 仅接受 `""`/`"openai"`（v5.0.0 删 anthropic/responses 后单 provider）。模型清单聚合（`cmd/miniagent/models.go`）在 cmd 层做 Kind 分派：`openai`/`responses`/`openai` 兼容全走 OpenAI 客户端；provider 包保持协议专属、互不依赖。`FetchModelLimits`（cmd 层）同样按 Kind 分派（当前仅 openai 分支）。

## 8. 会话持久化

`miniagent/session/` 以 jsonl 持久化：首行 `SessionMeta`（含 provider/model/workdir/created/LLMRequests；类型定义在核心 `miniagent/message.go`，session 包以别名复用），后续每行一条 message。

- **写（主路径恒 RewriteMessages）**：CLI 每轮 `saveSession` 全量重写——首行 `meta.LLMRequests` 跨轮累积，append-only 无法改首行；compaction 亦借此真正丢弃旧历史。临时文件同目录 + `os.Rename` 原子交换 + 父目录 fsync，写入/rename 失败均清理临时文件；写盘期忽略 SIGINT/SIGTERM。
- **写（AppendMessages，库 API）**：append-only 追加保留给库化调用方——flock 跨进程锁防行交错非法 JSON；预序列化拒绝超限（size+pending > limit 即拒，避免成功写入后被 LoadSession 拒）；`ensureTrailingNewline` 写前截断崩溃半行残留；原子性靠 `f.Sync()`。CLI 主路径不再调用。
- **读（LoadSession）**：`OpenNoFollow` + `LimitReader` 单次读取 + 单行 64KB 扫描缓冲；容忍末尾半行（崩溃残留），非末尾则报"mid-file corruption"。配对校验经核心 `ValidateToolPairing`。
- **改写（RewriteMessages）**：CLI 主路径每轮全量落盘（非仅 compaction 触发）。临时文件同目录 + `os.Rename` 原子交换 + 父目录 fsync，写入/rename 失败均清理临时文件。

## 9. 配置与解析

`config/` 三态裁决（cli flag > config 文件 > 内置默认）：

- **config 文件查找**：`-config` 显式路径，否则 `$MINIAGENT_CONFIG`，否则 `~/.miniagent/miniagent.json`（找不到即报错，无静默回落）。
- **严格解析**：`decodeConfigStrict` 拒绝未知字段（防拼写错误静默失效），但容忍 v5.0.0 已删键（`defaults.mode`/`provider.cache`/`run.confine_*`，作为废弃字段保留——旧 config 无需改即可加载）。
- **Defaults 叠加**：`defaults.system_prompt`（未配则内置 defaultSystemPrompt）+ opt-in `defaults.rules_file`（工作目录内 basename 规则文件，追加到 system prompt 中段；防越界/注入）。
- **Provider**：`kind`（仅 `""`/`openai`）、`name`/`chat_url`/`key`、`thinking`（level + provider 映射）、model 列表。
- **Run/Compaction**：`max_iterations`/`max_total_tokens`/`max_duration`/`stream`/`confirm_destructive`/`tool_output_dir`/`context_*`/`summary_*`/`preserve_recent_tokens` 等。

## 10. 安全模型

- **零安全保障**（v5.0.0）：`shell` 恒注册且无约束，文件工具无路径限制，`.git` 不封锁，白名单子命令工具全部删除。隔离**完全靠运行用户的 OS 权限**（容器/低权 UID/文件权限），调用方负责。
- **workdir**（必填、绝对路径）：仅作为相对路径解析基准 + shell cwd，**不是安全边界**。
- **rules_file 越界**：basename-only（仍在，防越界读注入）。
- **凭证剥离**：shell 子进程剥离所有 `MINIAGENT_*` 前缀环境变量（含 key/URL），避免密钥泄漏给 LLM 派生命令；其他环境变量按原样继承。
- **破坏性工具确认**（opt-in `confirm_destructive`）：write/edit + 危险 shell 前提示，非交互/子 agent 模式拒绝（可 `$MINIAGENT_AUTO_APPROVE=1` 放行）。

## 11. 事件协议（NDJSON，`miniagent/event/`）

stdout 一行一个 NDJSON 事件，类型：`session`（新建会话首条，含 id）/ `llm_request`（每次 LLM 调用）/ `tool_use` / `tool_result`（含 ExitCode/IsError；exit_code 仅在结果携带可信退出码时出现——命令语义性非零退出（IsError=false）或错误但 ExitCode>0，校验拒绝/超时等未真正执行命令的路径省略该字段，避免 is_error:true 与 exit_code:0 并存的矛盾）/ `delta`（流式增量，含 kind=text/reasoning）/ `result`（最终，含 `compacted`/`thinking_downgraded` —— 消费方可感知本轮压缩/降级信号而无需解析 transcript）/ `error`（含 code）。

辅助：`-replay` 离线重放会话（读 jsonl、不呼 LLM、不落盘、不读 stdin）；`-metrics-step` 通过 `OnStep` 把 per-step 观测写 stderr NDJSON（transcript 增长、token 斜率、压缩次数、LLM 请求数）。

## 12. 关键不变量速查

1. 核心循环 `Run` 零策略；一切策略经 `LoopHooks` 外挂。
2. `LoopHooks` 9 个字段（含 `OnStep`）皆可 nil → 退化为极简 agent。
3. `OnCompacting` 是 `ContextBudget` 字段（压缩内部钩子），**不在** `LoopHooks`。
4. 依赖单向无环：所有子包 → 核心（+ `text` 纯工具），无任何跨子包横向引用。`cmd` / `config` / `provider` 与核心同级。
5. 领域类型居核心 `miniagent/`：`Message/Usage/Request/Response/Tool/ToolResult/StepInput/StepOutput/BudgetInput`（`loop_api.go`）、`SessionMeta/ValidateToolPairing`（`message.go`）、`EstimateTokens/EstimateResponseTokens/TrimForHistory`（`token_estimate.go`）。
6. `msgs`（上下文）与 `newMsgs`（本轮新增）分离，经 `appendMsg` 同步。
7. 压缩只改 context-side copy，不动 newMsgs/持久化；真正丢弃旧历史靠 `RewriteMessages` 原子改写。
8. think 降级经 `captureDowngrade` 跨步固化，主路径/重试/总结闭包共用同一处理点。
9. 单 provider（openai），`Provider.Kind` 仅 `""`/`"openai"`。LLM 接口不感知底层协议；provider 包仅 import 核心类型 + httpretry。
10. 会话文件 jsonl：主路径恒全量 rewrite（同目录临时文件 + os.Rename 原子交换 + 父目录 fsync），flock + 预序列化拒绝超限 + 末尾半行容忍仍完整；AppendMessages 为库 API，CLI 主路径不调用。
