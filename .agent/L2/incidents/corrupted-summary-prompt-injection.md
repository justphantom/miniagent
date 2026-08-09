---
layer: L2
type: incident
tags: [compaction, summary, prompt-injection, quality, P0, open-issue]
created: 2026-08-09
confidence: high
---

# 损坏摘要被当 KindSummary 落盘并主动注入（P0 未决）

> 状态：**未实施根因修复**。记录现状与根治方向，供未来会话不要重蹈、也不要假设摘要内容可信。

## 现象
实跑会话（kimi/k3 + compaction 用 agnes-2.5-flash、`summary_max_tokens=512`）压缩产出的 1502 字符 `<tool_call>` python 改写脚本（无任何模板段）被原样落盘为 idx0 `KindSummary`；agent 复活后第一个动作（idx1）原样执行该 stale draft 脚本（与实际落盘代码 divergent），重走压缩前已被抛弃的「指纹路径」——压缩成 prompt 注入向量，净边际信息为负。

## 根因
`summarizeMiddle`（`internal/miniagent/compaction/assemble.go`）唯一守卫是 `FinishReason=="length" && TrimSpace(Text)=="" && Reasoning!=""`——只挡空摘要。agnes-2.5-flash + 通用 prompt + 紧预算回吐的非空 tool_call 输出，`FinishReason` 大概率 `stop` + 非空 → 直通落盘。当前对 `length+空+非空 reasoning` 子例已回落 lossy（见 [thinking-pindown-downgrade-length.md](thinking-pindown-downgrade-length.md)），但**非空含 tool_call 标记的输出未拦**。

## 根治方向（未实施）
拒绝含 `tool_call` 标记或缺 ≥2 模板段的摘要输出 → 加 strict "OUTPUT PROSE ONLY" 重试一次 → 再失败 surface error → `FitHistory` 回落 lossy。辅以：pin 已知好 summarizer；`summary_max_tokens` 勿过紧。

## 关键约束
1. 压缩路径缺 prose 校验是结构性缺口。
2. **消费侧（`applyCompactionBarrier` / memory 渲染）纯 Kind 判别，绝不嗅探 Content 前缀**——改摘要 Content 形态对读侧零破坏，但绝不可硬编码单一前缀做 `TrimPrefix`。

## 参考
- `internal/miniagent/compaction/assemble.go`（`summarizeMiddle`、`compactWithSummary`）
