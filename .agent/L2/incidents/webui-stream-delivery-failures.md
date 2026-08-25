---
layer: L2
type: incident
tags: [webui, streaming, ndjson, turn-lifecycle, cancel, event-contract, lag-close]
created: 2026-08-25
confidence: high
---

# WebUI 流交付链四层故障叠加（上游掐流 / cancel 杀 turn / 无终态 / lag-close）

## 现象
线上「连接中断：流意外结束」频繁：会话已落盘、后端无错误日志，前端却报流断。三轮根因（`docs/SSE_STREAM_ROOTCAUSE{2,3}.md`，历史可查）定位为四层叠加，前两轮真 bug 已修。

## 根因（四层，按发现序）
1. **上游 mid-stream 掐流**（R1，外部）：zhipu 随机中途断流/发截断 JSON 帧，`parse sse chunk: unexpected end of JSON input`。llm-proxy 是受害者非凶手。不可控，`deltaSent>0` 时不重试（防重放半段流，`streaming-sse-robustness` 决策），文本半截降级 `finish_reason=length`（R4）。
2. **`defer cancel()` 绑定 handler**（R2，P0 架构 bug）：`web_turn.go` 曾 `defer cancel()` 于 handler 作用域，turn goroutine 生命周期长于 handler——客户端刷新/断网即调用 cancel 杀正在跑的 turn，违反 D1「客户端断开不得杀 turn」。fix：cancel 迁入 turn goroutine 内 `defer`，busy/早退路径各自 cancel。
3. **context.Canceled 无终态事件**（R3，P0 契约 bug）：`run_turn.go` Canceled 分支静默返回、不 emit 终态；前端 `sawTerminal` 永假 → 落「连接中断」兜底文案。fix：契约新增 `stop` 事件（reason=canceled），前端认其为终态渲染「已停止」。
4. **lag-close 掐线 + 前端无自愈**（P0）：事件总线缓冲 chan 仅 64，订阅者消费慢（WiFi/渲染阻塞/标签节流）→ 写满即 `close(ch)` 淘汰；handler 无法区分「turn 结束关闭」与「lag 掐线关闭」，一律按结束处理 → 干净 EOF 无终态 → 前端报错。弱点：D3 设计注释假定客户端会重连重放，但**发起端 sendTurn 从未实现重连**。fix：lag-close 写显式非终态 `stream_cut` 事件（非伪装正常结束）；前端 `healAfterCut`（rebuild 视图 + attachSpectator/loadReplay 重建）；`turnEntry.id` + lag-close WARN 可观测；缓冲 64→256 降 churn。

## 做法（修复后现状，勿回退）
- turn ctx 派生自 `s.baseCtx`（非 `r.Context()`），断连/关标签/切视图不杀轮次；停止走 `POST /api/sessions/{id}/stop`（已是 R2 修复验收点）。
- NDJSON 契约含终态 `result`/`error` 与新增 `stop`；服务端掐线用非终态 `stream_cut`，轮次真结束时仍 `result`/`live_end`——**「非终态信息事件」与「终态事件」语义是第一原则**。
- 前端：EOF 无终态且无 `stream_cut` 也触发 healAfterCut（双保险）；`runningKnown` 真则 attachSpectator（/live 全量重放），假则 loadReplay。服务重启丢 turn 时诚实走「连接中断」兜底，不假装自愈。

## 可复用经验
1. **审计工具链调优生成类代码（如 turn goroutine）时，凡是 goroutine 生命周期 > handler/调用者作用域的，任何绑定调用方 scope 的 cancel/defer 都是杀 goroutine 的隐患**——cancel 必须从内部 owner 管理，外部只允许显式 stop 指令。
2. **任何协议流都要保证「全到达终端状态」**：取消/失败路径必须 emit 显式终态事件，否则下游无法区分真结束与通道掐断，只能靠猜测兜底文案。
3. **总线淘汰慢订阅者时，淘汰行为必须对客户端可见**（显式事件），不得伪装成正常结束；且「设计假定客户端会重连」的注释必须配套实现重连，否则假定为空。
4. 故障定位方法可复用：「服务端日志全绿 + 客户端断流」→ 隔离在浏览器↔服务器交付层；「毫秒级对齐上游/网关日志 + curl 实证字节透传」可区分上游掐流 vs 本地断连。

## 参考
- `cmd/miniagent/web_turn.go`（turn ctx 生命周期 / stream_cut 分支）
- `cmd/miniagent/web_bus.go`（turnEntry / lag-close WARN / 缓冲容量）
- `cmd/miniagent/run_turn.go`（Canceled→stop 终态）
- `cmd/miniagent/webstatic/static/app.js`（healAfterCut / rebuildView）
- `cmd/miniagent/webstatic/static/events.js`（sawTerminal 认 stop/stream_cut）
- commits：cancel 解耦 + stop 事件（v6.4.0 序列）、lag-cut 自愈 3c7025d
- 相关：`.agent/L2/incidents/streaming-sse-robustness.md`（上游半段流重试边界）