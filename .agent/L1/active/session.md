---
layer: L1
type: session
updated: 2026-08-23
---

# 当前会话

## 状态
- 已发布 v6.4.0（WebUI 多会话同步 + 上游 SSE 截断重试加固），tag 已 push，工作树干净，verify-gate 全绿。

## 本会话产出
- **事故根因**（用户报告"连接中断"）：上游 `autoapi` 429 TPM 限流失效前掐断 SSE 数据行 → miniagent 解析到截断 chunk `unexpected end of JSON input`（`stream_parse.go:108`）→ `OnLLMError` 只对 `ErrContextLength` 重试，此错误透传 → 整轮中止；同时客户端请求 ctx 断开使 handler 提前返回（`web_turn.go` `<-r.Context().Done()`），缓冲中的 error 行未 flush，前端只见干净 EOF → 落入"连接中断"兜底。
- **修复**：`isTransientStreamError`（`provider/openai/stream.go`）纳入 `unexpected end of JSON input`，预-delta（`deltaSent==0`）透明重试一次；新增 `TestDoStream_TruncatedChunkPreDeltaRetried`。
- **发版**：CHANGELOG `[Unreleased]` → `[6.4.0]`（Added 合并 + Fixed 补本次修复），commit `7c2d895`，tag `v6.4.0` push 后 `make build` 二进制报 `v6.4.0`。`cmd/miniagent/web_live_test.go` 双窗口 result 完整性测试一并提交。
- 事故关联日志：`/opt/llm-proxy/logs/llm-proxy.log`（429/abort）、miniagent journal（`llm call failed step=6`）、会话 `20260823-104331-467079dcd1e88fd9.jsonl`（尾部停在 step5 tool 输出，无最终 assistant/result）。

## 待办
- 无。