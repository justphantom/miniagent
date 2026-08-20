---
layer: L2
type: incident
tags: [streaming, sse, retry, idempotency, tool-call, oom, credentials]
created: 2026-08-09
confidence: high
---

# 流式 SSE 健壮性组：中断检测 / index 碰撞 / 幂等重试 / OOM 与凭证封顶

## 现象
reverse proxy / LB 中途断流、兼容端点省略 index、恶意 provider 无限流式或在错误里回显凭证。

## 做法（commit `00038cf`，`provider/openai/stream_parse.go` + `stream.go`）
1. **中断检测**：`parseSSE` 跟踪 `doneSeen`；若 `!doneSeen && FinishReason=="" && 有 text/reasoning/callOrder` 则返 `(partial Response, errStreamUnterminated)` 硬错，区分「clean EOF 但无终止符」与正常终止（OR 语义有意：Azure/LiteLLM 只发 `finish_reason` 不发 `[DONE]` 仍合法）。provider 级 `StreamAllowUnterminated` opt-in 放宽（vLLM / Ollama）。
2. **重试幂等边界**：`StreamClient` 不带总 Timeout（会砍长生成，总时长交 ctx）；`DoStream` 用 `wrappedOnDelta` 计 `deltaSent`，仅当 `deltaSent==0` 且瞬态错误且 `attempt<maxRetries` 透明重试，`deltaSent>0` 不重试（live UX 已收部分内容，重放会重复）。
3. **index 碰撞防护**：`streamAccum.apply` 同 index 已有非空 `acc.name` 且新 name 不同 → 「index collision」显式报错（spec 合规流 name 仅首片段出现，零误报），防兼容端点每调用都 `index=0` 静默覆盖前一条 name 并拼接 args 的历史 bug。
4. **封顶**：`maxStreamResponseBytes=4MiB` `guardAdd` 防无限流式 OOM；`maxErrorChunkChars=256` 截断 provider error.message（防恶意代理在错误里回显凭证 / 超大文本）；剥离行首 UTF-8 BOM；bufio buffer 放宽 4MiB 防超大单行 `ErrTooLong`；`sawChoice` 假且无 finish/text/tool_calls 报错防空流伪装成功。

## 根因
原解析只看 `[DONE]` / finish，缺对「有内容无终止符」识别。

## 可复用经验
**凡流式 / 增量输出回调的重试必须有显式幂等边界——零 delta 可重试，有 delta 不可重试。** 否则断流重放会重复内容或伪装部分成功。

## 参考
- `provider/openai/stream.go`（`DoStream`）、`stream_parse.go`（`parseSse` / `apply` / `errStreamUnterminated`）
- commit `00038cf`
