---
layer: L2
type: pattern
tags: [review, workflow, adversarial, verification, blind-spot, quality]
created: 2026-08-11
updated: 2026-08-11
confidence: high
---

# 对抗式 workflow 评审：finder × verify 两阶段

## 模式
重大变更（新 provider 接入、机制移除）评审用 workflow：N 维 finder 并行各管一面 → 每条 finding 进对抗 verify（默认 refute，读码确认）。比单视角评审多捕盲区问题。

## 做法（两轮实测）
- **finder**：按维度切分（wire 正确性 / SSE 健壮性 / 核心契约 / config-cmd / 并发边角 / 测试覆盖 / 文档一致性），每维独立读码报 finding（schema：file/line/summary/severity/failure_scenario）。
- **verify**：对每条 finding 派对抗验证 agent，默认 refute，读 cited 代码 + 交叉文件，返回 verdict（confirmed/refuted/uncertain）+ corrected_severity。pipeline 结构：每维 finder 完成即对其 findings fan-out verify，无 barrier。
- **embed 权威事实**：把易误判的事实（如 Anthropic thinking 正确 wire 形态、API 错误码）写进 finder/verify prompt 当 ground truth，防误报。

## 实测收益
- 首轮（anthropic 接入评审，22 agents）：16 发现/15 确认，独立确认人工 3 个 + 捕人工漏的 2 个（529、StreamAllowUnterminated）。
- 复审（提示词注入，18 agents）：12 发现/11 确认，盲区维度（summary_* 提示词族）捕到 budget.go:269 真 bug（人工前置预检未覆盖，因其非提示词注入类）。
- 两轮共 refute 2 条（L0#10 误读、README 历史迁移注）——对抗 verify 有效挡误报。

## 可复用经验
- **盲区维度**是收益源：审「主机制」之外的相关面（审提示词注入时带 summary_* 提示词族、审 provider 时带 config-cmd 接线）。
- **finder 切维度**而非切文件：同一文件多视角比单大 finder 深。
- **embed 权威事实**防 finder/verify 漂移（尤其 API 形态、版本真相）。
- **pipeline 优于 parallel+barrier**：维度间无依赖，finder 完成 verify 即启。
- 不适用：单点小改、纯对话、机械编辑——这些 solo 即可。

## 关联
- `anthropic-provider-copy-asymmetry`（L2/incidents）——首轮产物
- `compaction-headadj-override-stale-clause`（L2/incidents）——复审产物
- `producer-contract-change-ripple`（L2/patterns）——评审本质在跑的 checklist

## 参考
- 对抗式 workflow 评审（finder × verify 两轮）
