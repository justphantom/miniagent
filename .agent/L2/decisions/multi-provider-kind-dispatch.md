---
layer: L2
type: decision
tags: [architecture, provider, anthropic, wire, thinking, dispatch]
created: 2026-08-11
updated: 2026-08-20
confidence: high
status: superseded
---

> **v5.0.0 已废弃**：anthropic provider 已移除（`9bdb60a`），当前仅 openai 单一 provider。Kind 分派架构保留（`cmd/miniagent/setup_providers.go`），但 `"anthropic"` 分支不复存在。此决策保留供参考。

# 多 provider Kind 分派 + wire 边界有损投影

## 背景
原仅 openai 一种 provider，cmd 硬编码 `&openai.Provider{}`（main.go:193），无选择机制。接 Anthropic Messages API 需第二 provider 共存，且 openai/兼容端点零回归。

## 决策（方案 A：核心零改动 + wire 边界有损投影）
1. **cmd 按 `ProviderConfig.Kind` 字符串 `switch` 分派**（`setup_http.go` buildLLM/buildDoer）：`""`/`"openai"` 默认，`"anthropic"` 走新包。**不引入 factory/registry**（重复 <3 不抽，对齐 AGENTS.md 节制抽象）。`buildRuntimeClients`/`compactionOptions` 签名由具体 `*openai.ChatClient` 改 `miniagent.LLM`/`miniagent.Doer` 接口。
2. **核心领域类型零改动**：经现成 `miniagent.LLM`/`Doer` 接口缝（provider_api.go:39-50）替换 provider；`loop.go:Run`/`handleToolCalls`/compaction（已用 Doer）都不动。新 provider 在 wire 边界把扁平核心类型投影成厂商协议——**不为此扩 Message/Response/Delta/Usage**。
3. **thinking 钉死契约不变，扩 Map 值语义**：核心 `ThinkingMapping{Field, Map map[string]string}` 不动；anthropic 把结构化对象（`{type,budget_tokens}` / `{type:adaptive,effort}`）编码为 Map 值的 **JSON 对象串**，provider 自解（effort 拆 `output_config`）。覆盖两种模型族，不动核心类型。
4. **retry/backoff + HTTP transport 跨 provider 拷贝复用**：retry.go 非导出函数（shouldRetryStatus/parseRetryAfter/capRetryDelay/sleepCtx + 常量）HTTP 级无关，拷贝 ~95 行；不抽 internal/httpx（会逼 openai 包改 import + 52 调用点，违核心零改动；looptest 已有拷贝先例）。

## anthropic provider 关键约束（未来维护必知）
1. **thinking 模型族错配 400**：Claude 4.7+/5（sonnet-5/opus-4-8/fable-5）拒 `enabled+budget`、要 `adaptive+effort`；4.5-（sonnet-4-5/haiku-4-5）反之。config 按模型族配 Map 值；`isThinkingError`（双向措辞）兜底 → `ErrThinkingUnsupported` → callLLMWithDowngrade drop thinking 重试。见 thinking-pindown-downgrade-length。
2. **thinking signature 多轮**：服务端要求 signature 后续轮原样回放；核心 `Reasoning` 单 string 无落点 → wire **DROP 历史 thinking block**（当轮仍流式给 consumer）。这是方案 A 的"有损"点。
3. **content 是 block 数组**：assistant 把 Content+ToolCalls 重排为有序 `[text?,tool_use...]`；`tool_use.input` 必须是对象（json.RawMessage），发字符串 → 400；连续 `role=tool` 合并进**同一 user** 的多 tool_result block（拆条训练模型停并行）。
4. **SSE 无 [DONE]**：终止是 `message_stop` 事件；parseSSE 跳过 `event:` 行只解 `data:` JSON.type。中断检测/幂等重试同 openai（`errStreamUnterminated` + `StreamAllowUnterminated` opt-in），见 streaming-sse-robustness。
5. **usage 跨事件**：input(+cache_creation+cache_read) 在 `message_start`、output 在 `message_delta`。**未到 message_delta 归零 usage**（防 compaction 零用量回退拿半成品双计）；cache token 折叠进 `InputTokens`（核心 Usage 无 cache 字段，溢出检测需总量）。
6. **鉴权固定**：`x-api-key` + `anthropic-version:2023-06-01` + `interleaved-thinking` beta 头（不支持处静默忽略，无条件发安全）；非 Bearer，不能套 openai StreamClient。

## 理由
- 接口缝本就为此设计（openai 包注释明示"换 provider 零核心改动"）；方案 A 顺应。
- 不动核心领域类型 = 零回归（openai/兼容端点 + 所有现有测试不受影响）。
- Kind 字符串分派是最小抽象；factory 在 provider 数 <3 时是过度设计。

## 关联
- `core-zero-policy-loophooks-decoupling`（L2/decisions）—— 核心零策略，provider 是子包钩子之一。
- `thinking-pindown-downgrade-length`（L2/incidents）—— thinking 钉死 + 降级链，本次复用且 Map 值扩为 JSON 对象串。
- `streaming-sse-robustness`（L2/incidents）—— SSE 健壮性组，anthropic parseSSE 复用同款中断检测/幂等重试。

## 参考
- `cmd/miniagent/setup_http.go`（Kind 分派）、`main.go`（接口签名）、`miniagent/config/config.go`（Kind 字段）
- 评估 workflow（finder × verify 两轮，三方案 A/B/C 对照，差异矩阵）
