---
layer: L2
type: decision
tags: [compaction, summary-garbage, code-fence, estimate-constants, usage, review]
created: 2026-08-22
confidence: high
---

# 压缩审查结论：代码围栏判定收紧 + 估算常量单源守护

## 背景
对 `miniagent/compaction/` 专项审查（COMPACTION-REVIEW.md，已删除）确认整体质量高：架构解耦彻底、不变量守护严密、无高危缺陷。落地 6 项小改，其中 2 项沉淀为本决策。

## 决策

### 1. 摘要垃圾判定的代码围栏收紧（P2-1）
- **原做法**：`assemble.go` 的 `summaryGarbageMarkers` 含 `"```"`，任何含代码围栏的输出判垃圾 → prose-only 重试 → 仍失败则**无故回落有损压缩**，中段信息全丢。
- **根因**：内置模板要求「命令、错误字符串逐字保留」，中文技术会话摘要保真引用时天然用代码块；事故文档只论证了 false-negative 面（垃圾直通），未记 false-positive 面。
- **新规则**：围栏不再一刀切。仅当**首非空行即是围栏**（输出主体即代码块，无 prose 骨架）才判垃圾；正文内围栏（命令/错误引用）合法通过。tool_call 标记仍独立在 markers 中。
- **测试**：`summary_garbage_test.go` 三例钉死（首行拒 / 带空行首行拒 / 正文通过）。

### 2. 估算常量一致性守护（P3-2）
- **问题**：`budget_tail.go` 的 `systemOverheadTokens=400 / envelopePerMsg=4 / envelopePerToolCall=20` 镜像 `policy.EstimateTokens` 契约，但 policy 侧调参时 compaction 静默漂移 → tail 预算系统性偏差。
- **做法**：policy 侧常量经 `SystemOverheadTokens / EnvelopePerMsgTokens / EnvelopePerToolCallTokens` 导出别名；`compaction/policy_const_consistency_test.go` 断言两侧相等。调参即测试失败，不再静默漂移。
- **取舍**：保留局部镜像而非直接 import 常量——既有注释契约「replacement callback sharing this contract stays compatible」不变，测试负责钉死一致性。

## 附带修正（记录备查）
- **P2-2** 能力边界：`run.compaction.auto` 静默检测依赖端点返回真实 `usage`；流式零 usage 端点无锚 → 三层防御降两层（本地估算门控 + 被动 ErrContextLength 重试），文档已明示（`ARCHITECTURE.md` §5）。
- **P3-3** 直接构造 `ContextBudget` 的库用户须先经 `deriveSummaryMaxChars`，字段注释已补指引（`budget.go`）。
- **P3-5** `budget_tail.go` 权限 600→664 对齐包内（git 不跟踪非执行位）。

## 参考
- `miniagent/compaction/assemble.go`、`budget_tail.go`、`budget.go`、`policy_const_consistency_test.go`
- `miniagent/policy/estimate.go`、`ARCHITECTURE.md` §5