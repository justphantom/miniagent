---
layer: L2
type: incident
tags: [provider, anthropic, openai, copy, asymmetry, review, wire, retry, validation]
created: 2026-08-11
updated: 2026-08-20
confidence: high
status: superseded
---

> **v5.0.0 已废弃**：anthropic provider 已移除（`9bdb60a`），仅保留 openai + httpretry。此 incident 记录的历史经验供未来重加 provider 时参考，不再适用于当前代码库。

# anthropic provider 复制 openai 时漏掉的对称项（评审捕到 5 bug）

## 现象
anthropic provider（`provider/anthropic/`）从 openai 复制 retry/stream/client/wire 后，5 个「openai 路径有、anthropic 路径漏」的不对称项漏改：
1. **role=system 折叠缺失**（wire_blocks.go）：核心 `summarizeAtLimit` 注入 role=system 消息；openai wire 透传，anthropic `case RoleSystem:` 空体丢弃 → 撞迭代上限时收尾指令丢失。修：折叠进 top-level system。
2. **max_tokens model 级绕过**（config.go）：anthropic 只校验 provider 级 max_tokens>0；model 级 0 经 pickMPG 覆盖 → 每次 400。修：model 循环加 kind=anthropic 校验。
3. **HTTP 529 不重试**（retry.go）：`shouldRetryStatus` 逐字抄 openai（429/5xx），漏 Anthropic 专属 529 overloaded。修：加字面量 529。
4. **StreamAllowUnterminated 未接线**（setup_http.go）：buildOpenAILLM 设该 flag，buildAnthropicLLM 漏设 → opt-in 对 anthropic 静默失效。修：补赋值。
5. **thinking.map 值不校验**（config.go）：validateThinking 只校验键；值（JSON 对象串）拼错 → resolveThinking 静默丢 thinking。修：启动期校验值为含 type 的 JSON 对象。

## 做法
逐项修复 + 补测试。对抗式评审 workflow（6 维 finder × verify）独立确认 + 捕全。

## 根因
「复制 openai 实现 + 改 wire 形态」时未逐项核对两 provider 的对称差异。openai 的某些语义（role=system 进 messages 数组、max_tokens 可省、无 529、StreamAllowUnterminated 接线点、map 值为裸串）在 anthropic 不成立或需另处理。

## 可复用经验（跨 provider 复制对称清单）
加新 provider 时，对每条 openai 行为核对新 provider 对应：
- **状态码集**：重试状态码是否一致（Anthropic +529）
- **config opt-in 接线**：每条 ProviderConfig 布尔/指针是否在 build*LLM 两路径都赋值
- **消息角色处理**：role=system 在 wire 层落点（openai 进数组 vs anthropic 顶层）
- **强制字段**：对方可选、本方必填的字段（max_tokens）是否有启动校验
- **config 值校验**：map/结构化值是否启动期而非运行期校验（fail-loud）
- **鉴权**：header 集、是否 Bearer（anthropic 用 x-api-key + anthropic-version + beta）

## 关联
- `multi-provider-kind-dispatch`（L2/decisions）——设计决策（此记实现陷阱）
- `producer-contract-change-ripple`（L2/patterns）——通用模式
- `streaming-sse-robustness`（L2/incidents）——SSE 健壮性（retry/idempotency 部分重叠）

## 参考
- `provider/anthropic/`（wire.go/wire_blocks.go/retry.go）、`cmd/miniagent/setup_http.go` buildAnthropicLLM
- 评审 workflow（finder × verify 两轮，各 20+ agents）
