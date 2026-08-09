---
layer: L2
type: incident
tags: [thinking, downgrade, retry, reasoning-models, finish-reason, length]
created: 2026-08-09
confidence: high
---

# thinking 钉死 + 降级链 + length 截断空回复

## 现象
thinking 启用易撞 provider 400（`ErrThinkingUnsupported`）；长 reasoning 模型（kimi/k3 等）reasoning_content 与正文共享 output 预算（服务端封顶 4096），长 reasoning 吞光预算致正文一字未生成就被 `finish_reason=length` 截断——核心原仅 Warn、空 Text 被当最终回复，致循环提前终止返回残缺文本。

## 做法
1. **钉死**（commit `5c3ae4a`，breaking）：`internal/provider/openai/wire.go:buildChatBody` 必经 `provider.ThinkingMapping` 的 `field` + `map[level]`；`req.Thinking==nil` 不发该字段；`thinking.field` 黑名单防 clobber 标准 payload key（钉死后 `reasoning_effort` 合法已移出黑名单）；启动期 `validateThinking` 枚举前置校验 `level∉map` 报错而非端点 400。
2. **降级链**：`internal/miniagent/loop_extra.go:callLLMWithDowngrade` 单步 `ErrThinkingUnsupported` 时去 thinking 字段重试一次；`downgraded` 标记回传 `Run` 经 defer 跨步固化 `cfg.ThinkingLevel=''`，防下一轮重传再撞 400。
3. **length 截断**（commit `be1857c`）：主路径当 `err==nil && FinishReason=="length" && Text=="" && Reasoning!=""` 且未降级且 thinking 开启时，去 thinking 重试一次释放输出预算（`downgraded=true` 防循环）；压缩路径 `summarizeMiddle`（`compaction/assemble.go`）同条件返错，让 `compactWithSummary` 上抛、`FitHistory` 回落 lossy 防垃圾摘要持久化。

## 根因
原对 `length` 仅 Warn，未识别「reasoning 吞光预算致正文空」子例。

## 可复用经验
1. **降级 / 回退链必须有显式终止条件**（`downgraded` flag / 单次不递归），窄判定只救确定子情形（length+空文本+有 reasoning），半句截断等不确定情形不自动续。
2. `callLLMOnce` 携带 `defer recover` 兜底畸形 payload panic 不崩进程（与防 tool panic 的 `safeCall` 对称）。

## 参考
- `internal/miniagent/loop_extra.go`（`callLLMWithDowngrade` / `callLLMOnce`）、`internal/provider/openai/wire.go`
- `internal/miniagent/compaction/assemble.go`（`summarizeMiddle`）
- commits `5c3ae4a`、`be1857c`、`d37e2fc`
