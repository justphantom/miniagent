---
layer: L2
type: pattern
tags: [compaction, budget, summary, context-window, strip, reasoning, cjk, dedup]
created: 2026-08-09
confidence: high
---

# 压缩体系：预算自适应 CW + 7 阶主动裁剪 + reasoning 头尾截断

## 背景
固定 summary 预算在中 CW / CJK 下致压缩后仍超窗、循环不终止；或中文摘要被 `MaxTokens` 先于字符上限截断（原隐性截到设计值约 30%）。同时每步即便未超窗都应无损压瘦上下文。

## 做法

### A. summary 体积自适应 CW
- `deriveSummaryMaxChars`（`internal/miniagent/compaction/assemble.go`）默认 `min(5000, CW/5)`；CW<=0 回落 5000。summary token 占 CW ~10%，防 summary 自身 > `CW×4/5`。
- `deriveSummaryMaxTokens` 从 `maxChars` 派生 `maxChars/2`（与 `EstimateTokens` 的 CJK≈1token/2chars 同口径），替代固定 1024 偏紧值（commit `7a051d4`）。显式 `summary_max_chars` / `summary_max_tokens` 仍可 override；同源派生保证「只配 chars 时 token 自动跟随」。

### B. jointTailBudget（`compaction/budget.go`）
`tail = CW×4/5 − overhead − head − summaryEstimate`，再 `min(..., preserveRecentTokens[CW/4, 2000–8000])`。不可压缩的 summary 优先占位、tail 主动让出；`selectTailByTokens`（`split.go`）消费该 budget，最近一轮强制入 tail（即便单轮超 budget 也不进 middle 被摘要）。

### C. anti-staleness
`compactWithSummary`（`split.go`）构造 summaryMsg 显式打 `Ts`，使其成 `lastApplicableUsageIndex` 最大 `latestSummaryTs`，失效压缩前 assistant 真实 usage，强制下轮回落本地估算重算 post-compaction 小历史，避免陈旧大 usage 驱动立即二次压缩。`compactionReserve` 改 flat 20000 buffer 不依赖 `maxTokens`（commit `d37e2fc`）。

### D. 7 阶主动裁剪（`applyContextStrips`，`budget.go`）
每步无损压瘦：P1 `stripStaleReasoning`（清非最近 `keepN=contextKeepReasoning=1` 条 reasoning）/ P7 `truncateKeptReasoning`（窗内超 ~4000 rune 单条做头 1/4 + 尾 3/4 截断）/ P4 `stripStaleToolArgs` / P6 `dedupReadResults`（按 path,offset 分组，不同 offset 是文件不同段不可互覆）/ P11 `foldStaleReadResults` / P8' `foldStaleWriteEditArgs`（`IsError=false` 闸，失败 write 不算 supersede）/ P9b `dedupShellCommands`。

`windowStartOf`（`history_dedup.go`）返回倒数第 keepN 个 assistant 的 index（不是计数），`keepN<=0 → len(msgs)` 全留——4.2.0 修复原 `keepN<=0` 返 0 被 dedup/fold 误读为「全窗口内=全保留」的反转 bug（应为「全压」）。

## 关键约束
1. `applyContextStrips` 只改 context 拷贝，不动 transcript / session（非压缩步 `committed=false`，持久化无损上下文有损）；`ValidateToolPairing` 仍成立。
2. 压缩后 `FitHistory` 额外对 tail 子切片跑 P1+P7（`budget.go`），防 post-compaction 逐字重放 tail reasoning。
3. 硬下限 `CW<~1536` 为物理极限不可由压缩解；`tokenBudget<=0` 回落纯轮数兼容无窗 / 旧 session。

## 参考
- `internal/miniagent/compaction/assemble.go`、`budget.go`、`split.go`、`history_dedup.go`、`history_reasoning.go`
- commits `7a051d4`、`d37e2fc`、`956e3f8`
