---
layer: L2
type: incident
tags: [compaction, budget, summarizer-prompt, override, stale-clause, contract-ripple]
created: 2026-08-11
updated: 2026-08-11
confidence: high
---

# jointTailBudget override 路径误扣 headAdj（SummarizerPrompt 过时子句）

## 现象
配 `defaults.summarizer_prompt`（override 路径）+ 第二次及以上压缩（头部已有旧 KindSummary）→ `jointTailBudget` 误把已提取的旧 summary 头计入 headAdj → tail 预算被扣到 0 → `selectTailByTokens` 退化纯轮次 → 压缩后超 CW×4/5 → FitHistory 报错。**override 路径在小窗模型上压缩失败，default 成功**，反转「override 是 default 严格超集」设计意图。实测 CW=4000/5000：default tail 296/1096 vs override 0/0。

## 做法
`budget.go:269` 删 `|| budget.SummarizerPrompt != ""`，使「head 为单 KindSummary 时 headAdj=0」对两条路径都成立；订正 255-257 过时注释；`compaction_test.go` 补 `mkOverride` + override+KindSummary 头（不扣）/ override+非 summary 头（仍扣）两用例，堵未测盲区。

## 根因
`compactWithSummary`（split.go:177-186）改「default 与 override 两条路径都把单 KindSummary 头提取为 prevSummary 并置 head=nil」，但消费者 `jointTailBudget`（budget.go:269）的 `SummarizerPrompt != ""` 子句仍按「override 路径保留头」扣 headAdj。注释（255-256）先于代码过时（声称 override 保留头，实非）。**生产者契约改了，消费者未同步。**

## 可复用经验
改生产者输出契约（此处=头是否进 out）→ 必 grep 所有预算/消费者调用点同步。该 bug 潜伏至「summarizer_prompt 盲区评审」才暴露——常规 compaction 测试只覆盖 default 路径（mk 不设 SummarizerPrompt），override+KindSummary 头组合是未测盲区。

## 关联
- `compaction-system`（L2/patterns）§B jointTailBudget 公式——此记 gotcha
- `producer-contract-change-ripple`（L2/patterns）——通用化的本类模式

## 参考
- `miniagent/compaction/budget.go:262-279`、`split.go:157-228`
- `miniagent/compaction/compaction_test.go` TestJointTailBudget
