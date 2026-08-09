---
updated: 2026-08-09T19:30:00+08:00
---

# .agent 记忆索引

## L0 永久约束
- [persona.md](L0/persona.md) — 定位与沟通风格
- [constraints.md](L0/constraints.md) — 架构不变量 + 记忆元规则
- [policies.md](L0/policies.md) — 流程策略

## L1 活跃过程
- [active/session.md](L1/active/session.md) — 当前会话

## L2 经验教训
- [decisions/core-zero-policy-loophooks-decoupling.md](L2/decisions/core-zero-policy-loophooks-decoupling.md) — 核心零策略 + LoopHooks 外挂 + 子包化解耦
- [decisions/default-mode-not-security-boundary.md](L2/decisions/default-mode-not-security-boundary.md) — default 模式非安全边界 + 凭证剥离局限 + confirm/sandbox 配置化
- [patterns/compaction-system.md](L2/patterns/compaction-system.md) — 压缩体系：预算自适应 CW + 7 阶裁剪 + reasoning 头尾截断
- [patterns/config-tri-state-resolve.md](L2/patterns/config-tri-state-resolve.md) — config 三态裁决 + 隐性 footgun
- [patterns/memory-freshness-pointer-over-count.md](L2/patterns/memory-freshness-pointer-over-count.md) — 记忆反过期元规则：结构数量引用用指针不硬编码
- [incidents/session-jsonl-persistence.md](L2/incidents/session-jsonl-persistence.md) — session jsonl 持久化可靠性组
- [incidents/streaming-sse-robustness.md](L2/incidents/streaming-sse-robustness.md) — 流式 SSE 健壮性（中断/index 碰撞/幂等重试）
- [incidents/thinking-pindown-downgrade-length.md](L2/incidents/thinking-pindown-downgrade-length.md) — thinking 钉死 + 降级链 + length 空回复
- [incidents/hooks-no-recover-shape-result-contract.md](L2/incidents/hooks-no-recover-shape-result-contract.md) — 钩子无 recover + Run 顶层兜底 + ShapeToolResult 契约
- [incidents/corrupted-summary-prompt-injection.md](L2/incidents/corrupted-summary-prompt-injection.md) — 损坏摘要注入（P0，v4.3.0 已修：isSummaryGarbage 校验 + prose-only 重试 + lossy 回落）
