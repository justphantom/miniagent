# miniagent 系统架构

本文按当前代码（`go.mod` → `module github.com/justphantom/miniagent`, go 1.25）描述 miniagent 的运行时架构。定位：一个**极简、无内置策略的 ReAct agent 核心库** + 一个从 stdin 读 prompt、向 stdout 输出 NDJSON 事件的 CLI。

## 1. 设计总纲

核心思想一句话：**循环本身不做任何上下文管理**。所有上下文策略（压缩、记忆、溢出检测、预算、用量记账、工具结果成型、事件输出）都经 `LoopHooks` 外挂实现。不挂钩子即得到一个把 transcript 原样发给 LLM 的极简 agent；挂上 `NewCompaction` 与 `OnBudget` 即恢复完整能力。

由此带来两条贯穿全系统的约束：

- **核心循环 (`loop.go:Run`) 无策略**：`cfg.LoopConfig` 只含循环本体所需（模型/系统提示/工具/历史/限额/流式/思考），一切压缩/保留参数在 `CompactionOptions`。
- **策略皆可插拔**：压缩 (`NewCompaction`)、预算 (`OnBudget`)、事件输出 (`buildHooks`)、工具结果成型 (`ShapeToolResult`) 都是钩子，核心不绑定特定实现。

## 2. 分层与目录

```
cmd/miniagent/        CLI 入口层：flag 解析 → 组装 → 调 Run → 落盘会话
  main.go             组装主线（config→Resolve→tools→hooks→Run→save）
  setup.go            config 查找、key 解析、hooks 构建、退出码
  setup_http.go       buildLLM / buildChatClient（HTTP client 注入）
  tools.go            buildTools：注册内置工具 + 脚本工具，按 mode 调约束
  session.go          session 解析（-save-session/-session 互斥、load/write）
  project.go          .miniagent/ 项目规则（persona/rules/scripts）发现与合并
  sandbox.go          confineWrap：default 模式写工具路径越界拒绝
  prompts.go          默认 system prompt、subagent 引导注入
  stdin.go            读 prompt（空 stdin 交互引导）

internal/miniagent/   核心库（零外部依赖，纯标准库）
  loop_api.go         domain 类型：LoopHooks/LoopConfig/Tool/StepInput/StepOutput/BudgetInput（原 types.go）
  loop.go             Run：ReAct 主循环
  loop_extra.go       callLLMWithDowngrade / callLLMOnce（含 panic 兜底）
  loop_tools.go       handleToolCalls / runToolsParallel / safeCall
  loop_hooks_default.go NewDefaultOnBudget / NewDefaultOnLLMError / NewDefaultShapeToolResult（默认外挂工厂）
  message.go          Message / Usage domain 类型
  errors.go           哨兵 error：ErrBudgetExceeded/ErrContextLength/ErrThinkingUnsupported/ErrToolDenied
  limits.go           Limits 结构：运行时覆盖内置默认（替代旧 Set* setter）
  provider_api.go     LLM / Doer / Provider 接口（provider 实现可替换）
  resolve.go          Resolve：cli>config>builtin 裁决产出 Resolved
  config.go           Config 结构、LoadConfig、validateConfig
  session.go          jsonl 会话持久化（load/append/rewrite、写前截断崩溃半行 ensureTrailingNewline）
  session_validate.go 会话校验：ValidateSessionID / ValidateToolPairing / validateSessionMessage
  overflow.go         context 超限识别（24 正则+排除，IsContextLengthError）
  output_accum.go     shell 输出字节滑窗累积器（保尾部、可选头部落盘）
  platform*.go        平台原语：flock / O_NOFOLLOW / 进程组 kill（windows 分文件）
  tools.go            路径解析、截断工具（truncate/truncateHeadTail）、schema 构造
  tool_output_store.go 工具输出落盘 store（超 limit 全文写盘 + 过期清理）
  tool_*.go           内置工具实现（read/write/edit/grep/glob/codemap/shell/script）
  url.go / history_util.go 小型工具

internal/miniagent/compaction/  压缩引擎子包（「压缩作为外挂」的默认实现）
  assemble.go         NewCompaction（封装为 before/after 钩子，after 自 4.2.0 起=nil）、summarizeMiddle、applyCompactionHook
  budget.go           FitHistory 流水线、ContextBudget、CompactingInput
  split.go            applyCompactionBarrier、compactWithSummary、selectTailByTokens
  compaction_split.go lastApplicableUsageIndex、isUsageOverflow（静默溢出判定）
  history_*.go        主动裁剪各阶段（reasoning/toolArgs/dedup/fold）

internal/provider/openai/  OpenAI 兼容 provider 实现（核心经 LLM 接口调用，可替换）
  wire.go             Chat Completions 序列化层（buildChatBody/parseChatResponse）
  client.go           ChatClient：非流式 + models 列表，重试/降级
  stream.go           StreamClient：流式 SSE
  stream_parse.go     parseSSE：SSE 帧解析与增量聚合
  models.go           ListAllModels：models 列表（动态 GET + 静态回落）
  retry.go            重试退避策略

internal/miniagent/event/  NDJSON 事件编码子包（session/tool_use/tool_result/result/error/delta/model）
internal/text/        纯文本工具（NowMs / Truncate / TruncateTail）
```

入口 `cmd/miniagent/main.go` 自上而下：**flag → config → Resolve → key → workdir/session → 合并项目规则 → 注入 `Limits` 运行时覆盖 → buildLLM → buildTools → loopCfg → NewCompaction → buildHooks → Run → 落盘**。

## 3. 核心循环 `Run`

`loop.go:Run` 是 ReAct 循环（`maxIterations` 默认 20）：

```
复制 History → 追加 user prompt
for step in 1..iterLimit:
    ctx 取消检查
    toSend = applyBeforeLLM(hooks)          # 开放缝①：压缩/注入/透传
    resp, downgraded = callLLMWithDowngrade  # 调 LLM（thinking 降级经 captureDowngrade 跨步固化）
    if err != nil && hooks.OnLLMError != nil:   # 开放缝：失败恢复（典型 ErrContextLength）
        recovered, retry = hooks.OnLLMError(msgs, err)  # 默认 NewDefaultOnLLMError 收紧历史
        if retry: 重试一次本步（核心不递归）；否则 error 上抛
    hooks.AfterLLM(step, resp)               # 开放缝②：用量记账/静默溢出判定
    recordStepUsage → 累加真实 usage（零 usage 估算 fallback 由 NewDefaultOnBudget 承载）
                    → hooks.OnBudget(...)    # 开放缝③：预算熔断
    if 无 tool_calls:
        appendMsg(最终文本+真实usage); return finishStop
    msgs = handleToolCalls(...)              # 执行工具并回灌（并行 + 配对补全）
    if step == iterLimit:                    # 撞上限：summarizeAtLimit 注入总结请求再调一次
        注入 roleSystem summaryRequest; return
return finishMaxIterations
```

关键设计：

- **`msgs` vs `newMsgs` 分离**：`msgs` 是 LLM 上下文（裁剪只动它），`newMsgs` 只记本轮新增（main 据此 append-only 落盘）。两者经 `appendMsg` 同步追加，保证上下文与持久化一致。
- **`Result` 全路径带回 `Messages`**：正常/出错/撞上限都返回全量 transcript，供会话持久化。
- **ErrContextLength 失败恢复（外挂）**：经 `OnLLMError` 钩子承载，核心自身不做任何错误恢复。默认实现 `NewDefaultOnLLMError` 对 `ErrContextLength` 调 `trimHistoryForContext`（清 reasoning + 压 tool content）收紧后 `retry=true`，核心据此重试一次本次调用（不递归）；其他 error 透传上抛。防长会话崩溃，且恢复策略可替换。
- **撞上限总结**：迭代上限前一步若刚执行工具，注入内部 `summaryRequest`（`roleSystem`，不持久化）引导 LLM 输出最终文本。

## 4. 开放缝：`LoopHooks`

`loop_api.go:LoopHooks` 是核心与外部能力的唯一缝口，共 8 个字段，皆可 nil：

| 钩子 | 时机 | 职责 | nil 行为 |
|---|---|---|---|
| `BeforeLLM` | 每步调 LLM 前 | 改写发给 LLM 的消息视图、收缩 transcript、注入记忆/RAG、提交持久化摘要、累加用量；`NewCompaction` 在此做静默溢出判定（从历史真实 usage 推断 Force）+ 压缩 | 透传原 transcript |
| `AfterLLM` | 每步响应后 | 用量记账、自定义观察（`NewCompaction` 自 4.2.0 起 after=nil，溢出判定已并入 before） | 不通知 |
| `OnBudget` | 累加 usage 后 | 零 usage 本地估算 fallback + 预算熔断（`ErrBudgetExceeded`） | 不估算不熔断（仅累加真实 usage） |
| `OnLLMError` | LLM 调用失败时 | 失败恢复（典型：`ErrContextLength` 收紧历史重试一次） | error 直接上抛终止循环 |
| `OnToolUse` | 工具执行前 | 事件输出/拒绝（`ErrToolDenied` 仅拒该工具） | 不通知 |
| `OnToolResult` | 工具执行后 | 结果事件输出（含 ExitCode/IsError） | 不通知 |
| `ShapeToolResult` | 结果入历史前 | 覆盖 tool 消息 content（截断/落盘/RAG 摘要） | 内置默认成型 |
| `OnDelta` | 流式增量 | 推 text/reasoning 增量事件 | 非流式不触发 |

`StepOutput`（BeforeLLM 回参）的语义是策略外挂的关键契约：

- `View`：本轮实际发给 LLM 的消息（必填）。
- `Commit=true`：核心把运行 transcript 替换为 View（压缩场景）。
- `Persist`：额外持久化增量（如 summary），带 `Kind` 的条目替换 newMsgs 中同 Kind 旧条目（多次压缩只留最新 summary）。
- `ExtraUsage`/`Compacted`：累加用量、标记压缩（交互层据此 rewrite session）。

## 5. 上下文管理（压缩引擎）

`compaction/assemble.go:NewCompaction` 把整套压缩封装为 `(before, after)` 钩子对（`after` 自 4.2.0 起=nil）。`before` 每步从历史最新真实 usage 推断 `Force`（静默溢出判定），再做 `applyCompactionBarrier` + `FitHistory`。溢出判定不再依赖跨步闭包状态——before/after 无共享可变状态，单 Run 内串行调用，可被多 Run 安全复用。

**压缩屏障**：`applyCompactionBarrier` 定位最新一条 `Kind=="summary"` 消息，只把它及之后的消息进 context，之前的旧历史仍留 session 文件（机会性 rewrite 才真正丢弃）。

**FitHistory 流水线**（`compaction/budget.go:74`）：

1. **4/5 阈值门控**：未超 `ContextWindow*4/5` → 仅跑主动裁剪，返回。`Force=true`（上一步真实 usage 命中溢出）跳过门控直接进摘要分支。
2. **摘要中段** `compactWithSummary`：保留最早 1 轮 + 按 token 预算选 tail（`selectTailByTokens`，边界轮可 split/shrink 贴合预算），中段调 `Summarize` 压成单条 `KindSummary` 消息。
3. **有损 fallback** `compactHistory`：摘要失败/无中段时回落。
4. **仍超 → `trimRecentRounds`**：裁到最近轮。
5. **再超 → 报错终止**：避免循环烧请求。

**主动裁剪**（`applyContextStrips`，两分支复用，仅改 context 侧拷贝）：`P1` 清旧 reasoning、`P7` 截长 reasoning、`P4` 压旧 tool_call args、`P6` 去重 read 结果、`P11` 折叠旧 read、`P8'` 折叠旧 write/edit args、`P9b` 去重 shell 命令。debug 级日志记录各阶段节省 token。

**静默溢出检测**（`compaction/compaction_split.go:isUsageOverflow`）：provider 200 成功但实际已撞窗时——判据 `usageFootprint(input+output) >= usableTokens(ContextWindow - reserve)`，reserve 默认 `min(20000, max_tokens)` 且 clamp 到 CW/5（防小窗时阈值低于 FitHistory 的 CW*4/5 门控、Force 过早主导）；仅 `ContextWindow>0 && auto` 启用。`before` 钩子每步从已入史最新 assistant.Usage 据此置 `Force`，下一步跳过估算门控直接压缩，撞 provider 400 前先压。

**context 超限识别**：`isContextLengthError` 用 24 条正则 + 4 条排除（防 throttling/rate-limit 误命中），状态门从仅 400 放宽到 `400||413`。

## 6. 工具系统

**内置工具**（7 个，`buildTools` 注册）：`read` / `write` / `edit` / `grep` / `glob` / `codemap` / `shell`。

**并行执行**（`loop_tools.go:runToolsParallel`）：同一步内 LLM 一次发起的多个 tool_call 相互独立，并行执行（信号量限并发，默认 `maxParallelTools=8`），结果按原 index 回填保证与 `assistant.tool_calls` 一一对应（OpenAI 要求顺序匹配）。信号量获取联动 ctx，取消后排队调用立即放弃。每个工具 panic 由 `safeCall` 兜底，未知/被拒工具短路回填错误结果。

**工具结果成型**（`defaultShapeResult`）：`trimForHistory` 截断 + 可选落盘。
- `SplitTruncate`：shell/grep/script/codemap 走**头 1/4 + 尾 3/4**分段截断（错误结论常在尾部）；read/edit 等**带行号代码类**走 head-only（前截断符合分段读大文件语义）。
- **工具输出落盘**（`cfg.ToolOutputDir`，默认按 session 目录派生 `<id>.tool-output/`）：超 limit 的全文写盘，历史 Content 改为 preview + 绝对路径提示；启动时机会性清理过期文件（默认保留 7d）。

**配对不变量**：assistant.tool_calls 与 tool 消息一一对应是核心保证的不变量。下游管道关闭时，核心为剩余 calls 补占位 tool 消息保配对完整，防续跑被端点 400。

## 7. 客户端层

**ChatClient**（非流式，`internal/provider/openai/client.go`）：`sync.Once` 保护端点懒解析；重试仅对瞬时故障（429/5xx + 网络错误）生效 `maxRetries=2` 次，退避 `retryBaseDelay*2^attempt`，`Retry-After` 头优先且受 `retryMaxDelay` 封顶。响应体上限 `maxChatBodyBytes=4MiB`。

**StreamClient**（流式 SSE，`internal/provider/openai/stream.go`）：流式 client **不带总 Timeout**（会砍断长生成），总时长交 ctx 控制；注入的 client 若有 Timeout 则借用其 Transport 另造无 Timeout client。重试仅在 **pre-delta 阶段**（client.Do 失败或非 200）有效；进入 `parseSSE`（已 200、已流 delta）即不可撤回不重试。

**降级重试**（`loop_extra.go:callLLMWithDowngrade`）：单步 thinking 降级——`ErrThinkingUnsupported` 时去 thinking 字段重试一次，`downgraded` 标记回传 Run **跨步固化** `cfg.ThinkingLevel=""`（避免下一轮重传再撞 400）。`callLLMOnce` 携带 panic 兜底（畸形 payload 致解析 panic 不崩进程），与防 tool panic 的 `safeCall` 对称。

**wire 层**（`internal/provider/openai/wire.go`）：`chatMessage`/`chatToolCall` 与 domain `Message`/`ToolCall` 字段刻意重复，上层 domain 不绑死厂商 JSON 形状。`buildChatBody` 写入思考级别（默认字段 `reasoning_effort`，`ThinkingMapping` 可覆盖字段名与级别映射，跨供应商兼容），含请求体大小预估拦截防 OOM。`Message.Kind`/`Usage`/`IsError` 是 session 层标记，**绝不泄漏给 LLM**（`buildChatBody` 独立构造）。

## 8. 会话持久化（`session.go`）

格式：jsonl，首行 `type=session` metadata（id/model/workdir/provider/created），余 `type=message` 行嵌入 `Message`。

- **append-only**（`AppendMessages`）：正常轮追加 `result.NewMessages`。`flock` 跨进程锁防行边界交织非法 JSON；`info.Size()>maxSessionBytes` 前置拒绝 + 预序列化按 `size+待写` 超限拒绝，避免「写成功延后失败」；`ensureTrailingNewline` 写前截断崩溃半写残留的尾行（H3-1）——否则 O_APPEND 盲写把新消息拼到无换行结尾的残行上、下次 Load 反噬为中段损坏（永久丢会话）。
- **rewrite**（`RewriteMessages`）：仅 `result.Compacted` 时全量重写（临时文件 → `os.Rename` 原子替换）。append-only 落盘的 newMsgs 含被屏障的旧 summary 与被压中段，长会话需 rewrite 真正丢弃。
- **LoadSession 容错**：尾行半写（append-only 崩溃残行）容忍丢弃，中间损坏严格报错；`ValidateToolPairing` 守配对完整。

**平台硬化**（`platform.go`）：`O_NOFOLLOW` 拒最终分量 symlink；`flock` 非阻塞 + 5s 轮询（持锁进程挂死不永久阻塞）；目录 `0o700`、文件 `0o600`；`setPGID`+`killProcessGroup` 让 shell 超时能杀整个进程树。

## 9. 配置与解析

**配置来源**：config 文件（`-config` 显式 > 默认 `~/.miniagent/miniagent.json`，不存在报错——S1 删裸模式后 config 必须存在）。结构 `Config`：`providers[]` / `defaults` / `run` / `session` / `compaction`。

**裁决优先级**（`resolve.go:Resolve`）：`cli > config > builtin`。CLI 用 `flag.Visit` 区分"显式传入"与"默认值"。核心 CLI 参数（provider/model/thinking/mode/system/workdir/max-tokens/max-iterations/stream）可被 CLI 覆盖；策略参数（summary/duration/window/keep\*/context 阈值等）只在 config。

**成对规则**：`provider/model` 须成对（`-provider`/`-model` 同传覆盖 defaults 对，同缺以 defaults 为准）；`compaction.provider/model` 可跨 provider，同空回落 defaults。

**key 解析**：`provider.Key（config）> $MINIAGENT_API_KEY`，机密建议用环境变量。`thinking.field` 黑名单防 clobber 标准 payload key。

**`Limits` 运行时覆盖**（`limits.go`）：main 据 resolved 构造 `Limits`（`MaxReadFileBytes`/`MaxShellOutputChars`/`ShellStreamWindowBytes`/`MaxGrepMatches`/`MaxSessionBytes`/`ContextTrimToolChars`），经工具构建函数 / session 函数 / 钩子工厂显式注入。替代旧 `Set*` 包级 atomic override——消除包级可变状态，支持多实例（subagent fork 用不同 limits）、无 race（不需 atomic）、测试隔离（传参而非 Set 全局）。零值字段在各注入点回落模块内置默认。

## 10. 安全模型

**两种权限模式**（`loop_api.go`）：

- **`default`**：薄软约束——写工具（write/edit）经 `confineWrap` 限定 workdir 子树、shell/script 拒 sudo/su；workdir 必填。
- **`auto`**：无任何约束。

**重要**：default **不构成安全边界**——shell 可 cd/绝对路径越界，写工具可符号链接逃逸。隔离由调用方（沙箱/容器）保证。

其他硬化：请求/响应体大小上限防 OOM/烧钱；HTTP 重定向依赖标准库默认 `CheckRedirect`（跨域剥离 Authorization）；insecure URL 警告；session 文件 flock/NOFOLLOW/权限。

## 11. 事件协议（NDJSON，`internal/miniagent/event/event.go`）

stdout 输出 NDJSON 流（`-result-only` 时为 subagent fork 的纯文本模式，不发事件）：

- `session`：`-save-session` 新建会话时 Run 前首条（与 jsonl 首行同构），供消费方程序化捕获接续 id。
- `tool_use`：工具执行前（name + input）。
- `tool_result`：工具执行后（output 截断 2000 字符、truncated、is_error、exit_code 仅 shell）。
- `delta` / `text_delta` / `reasoning_delta`：流式增量（带 step）。
- `result`：终态（text/model/input_tokens/output_tokens/steps/finish，键稳定可 parse）。
- `error`：终态错误。
- `model`：`-list-models` 每行一个（provider/model 分离字段）。

**退出码**：正常 0；error 1；SIGINT/SIGTERM 130（干净退出，不 emit error）。session 保存期间忽略信号防截断文件。

## 12. 关键不变量速查

- **工具配对**：assistant.tool_calls ↔ tool 消息一一对应（核心补占位保证）。
- **持久化一致性**：`msgs`/`newMsgs` 经 `appendMsg` 同步，main 据 `newMsgs` append-only 落盘。
- **Kind 不泄漏**：`Kind`/`Usage`/`IsError` 是 session 层标记，`buildChatBody` 独立构造不进 wire。
- **压缩可逆性**：summary 带 `Kind`，经 `applyCompactionBarrier` 屏障识别，不靠内容前缀嗅探。
- **跨步固化**：thinking 降级、Force 压缩、compacted 标记跨循环步累积，统一经 `defer` 写入 `Result`。
- **文件 <300 行**：单文件行数约束（AGENTS.md），大逻辑按阶段拆分（context/history/tool 各自独立文件）。
