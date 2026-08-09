---
layer: L2
type: incident
tags: [hooks, panic, recover, contract, footgun, shape-result]
created: 2026-08-09
confidence: high
---

# 钩子调用点无核心 recover → Run 顶层兜底 + ShapeToolResult 契约

## 现象
LLM 调用（`callLLMOnce`）与工具调用（`safeCall`）都有 recover 兜底，但 8 个 `LoopHooks` 字段与 `CompactingHook` 的所有调用点（`loop.go` BeforeLLM/AfterLLM/OnLLMError、`loop_extra.go` OnDelta、`loop_tools.go` OnToolUse/OnToolResult/ShapeToolResult、`compaction/assemble.go` `applyCompactingHook`）全是裸 error 检查、无 defer recover——第三方钩子 panic 直接崩进程，本轮未持久化的 NewMessages 丢失。

## 修复（commit `d37e2fc`，`internal/miniagent/loop.go:Run`）
在既有 defer 里加顶层 `if r:=recover(); r!=nil { err=fmt.Errorf("run panicked: %v", r) }`；recover 跑在字段赋值前，使 transcript 仍能存活供 session 持久化。比逐个钩子包裹更保护，也兜核心逻辑 panic。

## 理由（刻意取舍）
统一 `safeInvoke` 兜底会静默吞钩子 bug，与「错误尽早暴露」相悖；核心自带默认钩子经审查 panic-free，故核心装配无实际风险，未加统一兜底。

## 落地约束（写 / 审钩子必查）
1. 自定义 / 第三方钩子须自行 `defer recover()` 或保证恒不 panic。
2. `ShapeToolResult` 只可改 content，禁改 role / tool_call_id（破坏配对致端点 400，配对由核心 `fillPlaceholderTail` 保证）。
3. 钩子经返回值（`StepOutput` 五字段 / `CompactingOutput`）表达意图，禁直接改入参 `msgs` 元素（核心用返回值，直接改不生效）。

## error 语义
- `OnToolUse` 返 `ErrToolDenied` 哨兵 → 仅拒该工具、不终止循环。
- `OnLLMError` `retry=true` → 重试一次不递归。
- `CompactingHook` error → 中止本次压缩、走 lossy fallback。

## 参考
- `internal/miniagent/loop.go`（`Run`）、`loop_api.go`（`StepOutput`）、`loop_tools.go`
- commit `d37e2fc`
