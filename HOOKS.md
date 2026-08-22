# 钩子集成开发规范

配套 [ARCHITECTURE.md](./ARCHITECTURE.md)。本文规范 `LoopHooks`（`loop_api.go`）及其关联钩子（`CompactingHook`）的**二次开发与集成**：契约、错误语义、不变量、红线、测试要求。所有结论溯源到当前源码 `file:line`。

## 0. 评估结论（TL;DR）

**成熟度**：钩子设计契约清晰、可插拔、单步内执行顺序确定。`BeforeLLM` 经 `StepOutput` 五字段精确表达"改视图 / 提交持久化 / 累加用量 / 标记压缩"四种意图，是整个上下文管理体系的单一缝口，抽象层次恰当。

**可正确集成的三个前提**（开发者必须先理解）：

1. **钩子 panic 有 Run 顶层兜底（不自崩）**：`loop.go:39-59` 的 defer `recover()` 会把 9 个 `LoopHooks` 字段与 `CompactingHook` 的调用点（`loop.go`：OnStep / OnLLMError / AfterLLM / BeforeLLM、`loop_extra.go callLLMOnce 内 OnDelta`、`loop_tools.go`：OnToolUse / OnToolResult / ShapeToolResult、`compaction/compacting.go:applyCompactingHook`）及核心辅助函数中任一 panic 转为 error 返回（`run panicked: ...`），transcript/newMsgs 经同一 defer 保活供 session 落盘——钩子 panic 不再崩进程、不丢本轮消息。注意：panic 仍终止整轮 `Run`，且错误信息只含 panic 值、丢失调用点现场；故复杂钩子仍建议自行 `defer recover()` 以输出可定位错误（见 §6.1 红线 1）。核心取「Run 顶层兜底」而非逐调用点 `safeInvoke`：逐点包装会掩埋钩子内部一半的状态，顶层兜底以最小侵入保住持久化契约（d37e2fc 引入，f25d9db 更新注释）。
2. **钩子不直接改 transcript**：意图经返回值（`StepOutput`/content/`CompactingOutput`）表达，由核心折叠副作用。直接改入参 `msgs` 不会生效（核心用返回值）。
3. **`NewCompaction` 无共享可变状态**：`before` 闭包捕获的 `budget`（`compaction/compacting.go`）初始化后只读；每步 `Force` 推断到局部变量、拷贝传入 `FitHistory`，闭包不写共享状态——多 `Run` 并发复用同一对钩子实例亦无 race。（旧版 `overflowPending` 跨步状态已在 4.2.0 移除：溢出判定改为 `before` 从 `in.Msgs` 的真实 usage 推断，`after=nil`。）

**主要风险点**：钩子实现者若忽略上述 1、3，会在长会话/并发/畸形数据下崩进程或 `-race` 失败。

## 1. 钩子全景与执行时序

单步（step）内调用顺序，核心保证确定性：

```
applyBeforeLLM ── hooks.BeforeLLM(ctx, StepInput) → StepOutput      [loop.go]
  │
callLLMOnce ──── chat.Do / stream.DoStream                           [loop_extra.go]
  │  ├─ (失败) hooks.OnLLMError(ctx, step, msgs, err) → (recovered, retry, err)  [loop.go]
  │  │              └─ retry=true → 用 recovered 收紧历史，重试一次（核心不递归）
  │  └─ (流式) hooks.OnDelta(step, kind, text)                       [loop_extra.go callLLMOnce 内]
  │
hooks.AfterLLM(ctx, step, resp) → error                              [loop.go]
recordStepUsage → 累加真实 usage → hooks.OnBudget(ctx, step, in, *total) [loop.go]
  │
  ├─ 无 tool_calls → 终止（finishStop）
  │
handleToolCalls ──                                                    [loop_tools.go]
  ├─ hooks.OnToolUse(name, input) × N   （全部先顺序通知）           [loop_tools.go]
  ├─ runToolsParallel                   （并行执行，信号量限并发）
  └─ per call:
       hooks.OnToolResult(name, callID, result) → error              [loop_tools.go]
       hooks.ShapeToolResult(name, callID, step, result) → content   [loop_tools.go]
       appendMsg(tool)
```

`CompactingHook` 在 `compactWithSummary` 内、调 `Summarize` 前触发（`compaction/compacting.go` 的 `applyCompactingHook`），独立于 `LoopHooks`，仅压缩路径生效。

## 2. 逐钩子契约

### 2.1 `BeforeLLM(ctx, StepInput) (StepOutput, error)`

- **职责**：决定本轮发给 LLM 的消息视图（压缩 / 注入记忆 / RAG / 透传）。上下文管理的**唯一缝口**。
- **入参** `StepInput`：`Step`、只读 `Msgs`（当前 transcript）、`System`、`Tools`。只读意图——不要就地改。
- **返回** `StepOutput`：见 §3。
- **nil**：透传原 transcript（极简模式，核心不做上下文管理）。
- **error**：任意 error → 终止 `Run`，返回该 error。
- **红线**：须**幂等**（每步调用）；不直接改入参 slice。

### 2.2 `AfterLLM(ctx, step, resp) error`

- **职责**：用量记账 / 自定义观察（如事件输出）。`NewCompaction` 自 4.2.0 起 `after=nil`——静默溢出判定已并入 `before`（从历史真实 usage 推断 `Force`），本钩子不再承担置 `Force` 职责。
- **resp**：含真实 `Usage`、`FinishReason`、`ToolCalls`。
- **nil**：不通知。
- **error**：任意 error → 终止 `Run`。
- **注意**：核心在 `AfterLLM` 返回后才累加 `total`（`loop.go recordStepUsage`），故钩子读到的 `total` 不含本步；记账场景用 `resp.Usage`。

### 2.3 `OnLLMError(ctx, step, msgs, err) (recoveredMsgs []Message, retry bool, retErr error)`

- **职责**：单步 LLM 调用失败后的恢复缝口（`BeforeLLM`/`AfterLLM` 都在成功路径，唯有此钩子在失败路径，`loop.go`）。典型：`ErrContextLength` 时收紧历史重试一次。
- **入参**：`msgs` 当前运行 transcript；`err` LLM 调用返回的 error。
- **返回**：`recoveredMsgs` 非 nil → 核心用其替换运行 transcript；`retry=true` → 核心重试一次本次调用（不递归，再失败直接上抛）；`retErr` 非 nil → 上抛终止。`retry=false` 且 `retErr=nil` → 核心 error 直接上抛终止。
- **nil**：核心不做任何恢复，error 直接上抛终止 `Run`。默认实现 `NewDefaultOnLLMError` 承载 `ErrContextLength` 收紧重试（`trimHistoryForContext`），其他 error 透传。
- **幂等**：仅失败时触发，至多重试一次。

### 2.4 `OnBudget(ctx, step, BudgetInput, *Usage) error`

- **职责**：零 usage 本地估算 fallback + 预算熔断。核心已把真实 usage 累加进 `total`；本钩子在 `resp.Usage` 全零时补本地估算（`EstimateTokens`），再按 `MaxTotalTokens` 判定。
- **nil**：核心不估算不熔断（仅累加真实 usage）。默认实现 `NewDefaultOnBudget` 承载估算 fallback + `MaxTotalTokens` 判定。
- **error**：返回 `ErrBudgetExceeded`（可 `errors.Is` 判定）→ 终止 `Run`，走 error 路径（CLI 退出码 1）。
- **组合**：main 用 `NewDefaultOnBudget` 外挂预算判定（`main.go`）；自定义预算/熔断逻辑替换此闭包即可。

### 2.5 `OnToolUse(name, input) error`

- **职责**：工具执行前通知（实时观察 / 危险命令拒绝）。
- **时序**：本步全部 tool_call **先顺序通知**，再并行执行（`loop_tools.go`）。顺序确定。
- **哨兵**：返回 `ErrToolDenied`（`errors.go`）→ 核心**仅拒绝该工具**（回填"用户拒绝执行"、`ExitCode=exitCodeNotSet`）、**不终止循环**，继续通知其余工具。
- **其他 error**：终止 `Run`。
- **nil**：不通知。

### 2.6 `OnToolResult(name, callID, ToolResult) error`

- **职责**：工具执行后通知结果（含 `ExitCode`/`IsError`）。
- **error**：终止 `Run`，且核心为**剩余 calls（含当前 i）补占位 tool 消息**保配对完整（`loop_tools.go`）。典型场景：下游 stdout 管道关闭。
- **nil**：不通知。

### 2.7 `ShapeToolResult(name, callID, step, ToolResult) (string, error)`

- **职责**：覆盖 tool 消息入历史的 `content`（截断 / 落盘 / RAG 摘要）。
- **返回 content**：空串 → 核心用内置默认成型（`defaultShapeResult`：`trimForHistory` + 可选落盘）；非空 → 用该 content。
- **error**：终止 `Run` + 剩余 calls 补占位（`loop_tools.go`）。
- **🔴 红线**：**只可改 content，不可改 role / tool_call_id**——配对不变量由核心保证。钩子返回的是 `string`，物理上无法改其他字段，但实现者不得绕过此缝口自行构造 tool 消息。
- **nil**：内置默认成型。

### 2.8 `OnDelta(step, kind, text) error`

- **职责**：流式增量（`DeltaText` / `DeltaReasoning`）。仅流式模式触发（`callLLMOnce` 内，`loop_extra.go`）。
- **error**：立即中止流、沿 `DoStream` 返回该 error。
- **nil**：非流式不触发；流式但 nil 时核心丢弃增量（`return nil`）。

### 2.9 `OnStep(ctx, StepSnapshot) error`

- **职责**：每步**观察**缝口——输出该步的 transcript 长度、输入/输出 token、是否压缩、累计 LLM 请求数、本步新增消息数。observe-only（无 error 返回），典型消费者是 `metrics.NewStepEmitter`（NDJSON 到 stderr）。
- **触发时序**：每步顶部，`BeforeLLM` 之前（`loop.go:128`），step 编号从 1 起、随每一步推进（与 `StepSnapshot.Step` 的 1-based 语义一致）。
- **入参** `StepSnapshot`：只读快照，不含可写副作用。
- **error 契约**：`OnStep` 无 error 返回（`func(ctx, snap)`），实现者不得抛出——panic 会经 Run 顶层兜底终止整轮（见 §0 点 1），观察类逻辑请自行 recover 以免误杀整轮。
- **nil**：不通知（核心零开销短路）。
- **红线**：不得依赖快照字段顺序做精确匹配；`Compacted` 为**截至本步**是否压缩过（含历史步）。

### 2.10 `CompactingHook(ctx, CompactingInput) (CompactingOutput, error)`（关联钩子）

- **职责**：摘要前注入 context（领域知识 / 文件清单）或一次性替换 `summarizerPrompt`。
- **入参** `CompactingInput`：`SessionID`、只读 `Middle`、`Model`。
- **返回** `CompactingOutput`：`Context []string`（以一条 user 消息 append 到 middle 末尾进摘要输入）、`Prompt string`（替换本次 prompt）。
- **error**：实现 A 契约——返回 error **中止本次压缩**（`compaction/compacting.go`），`FitHistory` 回落有损压缩。不可 cancel。
- **nil**：零开销短路。
- **配对安全**：仅追加无 tool_calls 的 user 消息，不破坏配对（触发前已过 `ValidateToolPairing`）。

## 3. `StepOutput` 契约详解（BeforeLLM 回参）

| 字段 | 语义 | 核心动作 |
|---|---|---|
| `View []Message` | 本轮发给 LLM 的消息（必填） | nil 时回落入参 `*msgs` |
| `Commit bool` | 是否替换运行 transcript | true → `*msgs = View`（压缩场景）；false → 仅本轮发 View，保留原 transcript（记忆/RAG 注入） |
| `Persist []Message` | 持久化增量（如 summary） | `mergePersisted`：带 `Kind` 的条目替换 newMsgs 中同 Kind 旧条目，再前插到首部 |
| `ExtraUsage *Usage` | 累加用量（如摘要调用 token） | 非nil → 累加进本轮 `total`（`MaxTotalTokens` 预算含此） |
| `Compacted bool` | 标记本轮压缩 | true → 置 `result.Compacted`（交互层据此 rewrite session） |

**组合语义**：
- **压缩**：`View=收缩后 / Commit=true / Persist=[summary] / Compacted=true`。
- **注入不持久化**：`View=注入后 / Commit=false`（注入不进 transcript，下轮消失）。
- **透传**：直接返回 `nil` 钩子，或 `View=nil`。

`mergePersisted` 的 Kind 去重保证"多次压缩只留最新 summary"——钩子给 summary 带 `Kind=KindSummary` 即可。

## 4. 错误契约矩阵

| 钩子 | 返回 error 的效果 | 哨兵 / 惯例 | 配对补全 |
|---|---|---|---|
| BeforeLLM | 终止 Run | — | — |
| AfterLLM | 终止 Run | — | — |
| OnBudget | 终止 Run（熔断） | `ErrBudgetExceeded` | — |
| OnLLMError | retry=true 重试一次；否则终止 | — | — |
| OnToolUse | `ErrToolDenied`=拒该工具继续；其他=终止 | `ErrToolDenied` | — |
| OnToolResult | 终止 Run | — | ✅ 剩余 calls 补占位 |
| ShapeToolResult | 终止 Run | — | ✅ 剩余 calls 补占位 |
| OnDelta | 中止流、返回该 error | — | — |
| OnStep | observe-only，无 error 返回（panic 才异常） | — | — |
| CompactingHook | 中止本次压缩→有损 fallback | — | — |

补占位消息固定为 `{Role:tool, ToolCallID, Content:"tool result not submitted: upstream pipeline error", IsError:true}`（`fillPlaceholderTail`，`loop_tools.go`），保证 `Messages` 配对完整、续跑不被端点 400。

## 5. 并发与生命周期

- **单 Run 内顺序**：钩子按 §1 时序顺序调用，**非并发**。同一步内 `OnToolUse` 全部先通知、`runToolsParallel` 后执行；`OnToolResult`/`ShapeToolResult` 在结果回填循环内顺序处理。
- **闭包状态**：`NewCompaction` 的 `before` 闭包捕获 `ContextBudget`（`compaction/compacting.go`），但初始化后只读——`Force` 每步推断到局部变量、拷贝传入 `FitHistory`，无共享可变状态，多 `Run` 并发复用安全。旧版 `overflowPending` 跨步状态已移除（4.2.0）。
- **ctx 联动**：`OnDelta`/工具执行遵守 `ctx`（`runToolsParallel` 信号量获取联动 `ctx.Done`）；钩子内长操作应尊重传入 `ctx`。
- **资源生命周期**：工具输出 store（`toolOutputStore`）由核心在 `Run` 内创建/清理（`loop_hooks_default.go`）；钩子不应持有需跨 `Run` 释放的资源，除非自管。

## 6. 不变量与红线（实现钩子必须遵守）

1. **panic 自保（建议非强制）**：核心已有 Run 顶层兜底（`loop.go` defer recover，见 §0 点 1），钩子 panic 不再崩进程、transcript 保活；但 panic 仍终止整轮且丢失调用点现场，复杂钩子仍建议自行 `defer recover()` 或保证输入无关的恒不 panic（参考 `safeCall`/`callLLMOnce` 的兜底范式），以输出可定位的错误。
2. **🔴 配对不可破坏**：`ShapeToolResult` 只改 content；任何钩子不得新增/删除 assistant.tool_calls 或改 tool 消息的 `tool_call_id`。
3. **🔴 不直接改 transcript**：BeforeLLM 经 `StepOutput` 表达，核心按 `Commit` 决定是否替换。直接改入参 `msgs` 元素不生效。
4. **Kind/Usage/IsError 不进 wire**：钩子构造的消息若带 `Kind`（如 `KindSummary`）仅持久化/屏障识别用，`buildChatBody` 独立构造绝不泄漏给 LLM。
5. **幂等**：`BeforeLLM`/`AfterLLM` 每步调用，须可重复执行无累积副作用（`NewCompaction.before` 每步重跑 `FitHistory`）。
6. **Ts 打戳**：`appendMsg` 对 `Ts==0` 自动打 Unix 毫秒戳，显式设 Ts（如压缩 `summaryMsg`）不覆盖（`loop.go`）。钩子产出持久化消息依赖此"真实 usage 防陈旧"判定，勿手动清零 Ts。
7. **顺序确定**：`OnToolUse` 依赖"全部先通知"的顺序语义（消费方尽早看到完整工具计划），钩子内部不得乱序或延迟通知。

## 7. 可插拔组合模式

核心提供的默认外挂，可叠加自定义钩子：

- **`NewCompaction(opts) → (before, after)`**（`compaction/compacting.go`）：返回一对钩子挂 `BeforeLLM`/`AfterLLM`，恢复完整压缩能力。`opts.Chat` 必须非 nil（摘要 LLM 调用需 client）。
- **`OnBudget` 外挂**：main 用闭包把 `MaxTotalTokens` 判定从核心搬出（`main.go`）。自定义预算/熔断逻辑替换此闭包即可。
- **`OnStep` 外挂**：cmd 用 `metrics.NewStepEmitter(os.Stderr).Emit` 挂 `OnStep`（`run_turn.go`，`spec.metricsStep` 时），每步输出 NDJSON。自定义观测（指标、日志）替换此闭包即可。
- **`buildHooks(resultOnly)`**（`setup.go`）：组装事件输出钩子（`OnToolUse`/`OnToolResult`/`OnDelta`）。`resultOnly=true` 返回空 hooks（subagent fork 纯文本模式）。

叠加自定义钩子时：压缩与预算必须各自独占 `BeforeLLM`/`OnBudget`（核心单字段）；事件类（`OnToolUse`/`OnToolResult`/`ShapeToolResult`/`OnDelta`/`OnStep`）如需叠加，在外层闭包内串联调用（如先调默认事件钩子再调自定义），不得互相覆盖。

## 8. 测试要求（AGENTS.md：用例即需求文档）

- **必须有标准库测试**，覆盖：error 契约（终止/哨兵/补占位）、`StepOutput` 各字段组合、幂等性。
- **注入测试替身**：`Summarize` 回调注入假摘要（`ContextBudget.Summarize`）测压缩分支，无需真实 LLM；`BeforeLLM`/`ShapeToolResult` 用假 `StepInput`/`ToolResult` 单测。
- **`-race` 必跑**：闭包捕获的状态（如 `toolOutputStore`）经 `go test -race` 验证单 Run 内顺序安全；`NewCompaction` 已无共享可变状态（`Force` 局部化），多 Run 并发复用安全。
- 钩子 panic 自保逻辑应有专门用例（注入会 panic 的输入验证钩子不崩进程）。

## 9. 新增钩子集成 checklist

1. 确定触发阶段（每步 LLM 前/后、每工具前/后、每结果成型、流式增量、摘要前）。
2. 确定返回类型：纯通知（`error`）/ 产出内容（`string`）/ 意图包（`StepOutput`/`CompactingOutput`）。
3. 确定 error 语义：终止 Run / 哨兵拒单个 / 中止压缩 / 中止流（对照 §4 矩阵）。
4. 是否需要配对补全？（仅 `OnToolResult`/`ShapeToolResult` 错误路径核心自动补）。
5. 幂等性确认（每步/每工具调用点）。
6. panic 自保（`defer recover` 或恒不 panic）。
7. 跨步状态是否闭包累积？若是 → 文档标注"每 Run 新建"。
8. 不变量自检：不改 role/tool_call_id、不直接改 transcript、Kind 不进 wire、Ts 不清零。
9. 标准库测试 + `-race`。
