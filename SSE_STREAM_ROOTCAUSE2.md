# 「连接中断：流意外结束」根因分析（第二轮）

基线：v6.4.0-38-g0548ae7。方法：miniagent journal × llm-proxy 归档日志毫秒级对齐 + llm-proxy 源码审读 + curl 实证复现。结论：**四层叠加**，上游掐流是第一因，miniagent web 层两个真 bug 放大为"频繁"。

## §0 结论摘要

| # | 根因 | 层 | 性质 |
|---|------|----|------|
| R1 | zhipu 网关（glm-5.2→实际服务 glm-5.3，open.bigmodel.cn coding 端点）随机 mid-stream 掐流/发出截断 JSON 帧 | 上游 | 外部故障，低概率高频使用必遇 |
| R2 | `web_turn.go:95 defer cancel()`——handler 返回即 cancel turnCtx，**违反 D1 注释**（客户端断开不得杀 turn）：刷新页面/断网即杀后台 turn | miniagent web | **架构 bug（P0）** |
| R3 | `run_turn.go:169-173` context.Canceled 静默返回、**不 emit 终态事件**——前端 `sawTerminal` 永假 → 落「连接中断」兜底文案 | miniagent engine | **契约 bug（P0）** |
| R4 | `stream.go:213` mid-data 截断降级为 `finish_reason=length` 成功——答案静默半截，用户无截断感知 | miniagent provider | 体验缺陷（P1，v6.4.0 有意保内容的副作用） |

用户"频繁"的完整故事：R1 半截答案（体验主体）→ 用户刷新/重发 → R2 杀正在跑的 turn → R3 无终态 → 前端「连接中断：流意外结束」→ 用户以为服务坏了 → 再刷新 → 循环放大。

## §1 证据链（毫秒级对齐）

miniagent journal × llm-proxy 日志（归档 `llm-proxy.log.20260823-233603` + 当前）：

| 时刻 | miniagent | llm-proxy | 间隔 |
|------|-----------|-----------|------|
| 23:20:26.099 | `mid-data truncated: parse sse chunk: unexpected end of JSON input`（step7）| `upstream response committed; aborting mid-stream: context canceled` | 0ms |
| 23:21:40.230 | 同上（step1）| 同上 | 1ms |
| 23:22:10.932 | `llm call failed: read sse: context canceled`（step1）| 同上 | 0ms |
| 23:55:39.059 | 同 23:20:26（step1，新进程）| 同上 | 0ms |

支撑事实：
1. **23:55:28.841 UP 记录**（llm-proxy 向 zhipu 发起）→ 23:55:39.059 掐 = 10.2s；step7 同型 ≈10.8s；无固定周期，其余 6 次同会话请求（4-8s 完成）全部成功 → 随机性。
2. **curl 实证**（85s 直连 llm-proxy→zhipu glm-5.2 长思考流式）：流持续正常（848KB），断开残留为**无换行半行** → miniagent 的 `unexpected end of JSON input`（**带换行的坏帧**才能触发，`stream_parse.go:107`）只能来自 zhipu 发出的截断帧；字节透传经 `util.CopyResponseBodyWithUsage` 审读确认 llm-proxy 不重写帧。
3. llm-proxy `ErrResponseCommitted` 路径（`retry_execute.go:117`）：copy 读上游 body 报 `context canceled` = zhipu h2 流被 reset/GOAWAY 时 Go http2 的典型映射 → llm-proxy force-close 下游。**llm-proxy 是受害者不是凶手**。
4. 23:22:10 案例：turnCtx 被 cancel（R2 的 `defer cancel()`：用户在 23:21:40 半截答案后刷新页面）→ miniagent 主动断 → llm-proxy 同样落 canceled abort。两类故障在 llm-proxy 侧同貌，miniagent 侧可由错误文本区分（`parse sse chunk:` vs `read sse:`）。
5. miniagent 流式 client 无总超时（`streamClient()` 明确零化，stream_client_test.go 钉死）→ 排除客户端自掐；`upstream_streaming_seconds:120` ≠ 10s → 排除 llm-proxy 流超时。

## §2 根因详解

### R2：`defer cancel()` 杀 turn（D1 违反）

```go
// web_turn.go:94
turnCtx, cancel := context.WithTimeout(s.baseCtx, defaultWebMaxDuration)
defer cancel()          // ← handler 作用域！
...
go func() { s.engine.runTurn(turnCtx, spec, entry) ... }()  // turn 用 turnCtx 跑在 handler 外
...
case <-r.Context().Done():
    return              // ← 客户端断开/刷新 → handler 返回 → defer cancel() → turn 被杀
```

turn 的生命周期长于 handler，`defer cancel()` 却绑定 handler。文件头注释与 D1 设计（"client disconnect does NOT cancel the agent"）被此行推翻。`entry.cancel`（stop API 用）与它是同一 cancel——stop 能杀 turn 说明 cancel 有效直达 runTurn。

### R3：Canceled 无终态事件

```go
// run_turn.go:169
if errors.Is(err, context.Canceled) {
    ...
    return err // caller maps to 130 / closed stream; no error event
}
```

CLI 语义（exit 130）原样带进 web：turn 结束、bus 关闭、订阅者 drain 后 EOF，**无 `result` 也无 `error`**。前端 `app.js:233` 只认这两类为 terminal → 「连接中断：流意外结束」。用户点停止按钮也落此文案（应为"已停止"）。

### R4：mid-data 截断静默降级

`stream.go:213-218`：`deltaSent>0 && transient && 有文本` → `FinishReason="length"` 伪成功。v6.4.0 为保住已流出内容的正确选择，但：答案半截无任何标记；`input_tokens=0 output_tokens=0`（usage 帧没到）→ 计量失真；`loop_extra.go:42` 的 length 救援只覆盖空文本子案，有文本的截断不续跑。23:20:26/23:21:40/23:55:39 三次全是此路径。

## §3 修复方案

### P0-F1：turn 生命周期与 handler 解耦（R2）

`web_turn.go`：删 handler 的 `defer cancel()`；turn goroutine 内 `defer cancel()`：

```go
turnCtx, cancel := context.WithTimeout(s.baseCtx, defaultWebMaxDuration)
// busy 路径与早退路径需各自 cancel（防泄漏）：
//   register busy → cancel()（现逻辑已有）
//   degenerate fast path（subscribe 失败/首事件前结束）→ 已由 goroutine finish 覆盖
go func() {
    defer cancel()
    err := s.engine.runTurn(turnCtx, spec, entry)
    ...
}()
```

handler 其余 return 路径不再触及 cancel；stop API 的 `entry.cancel` 不变。

### P0-F2：Canceled 补终态事件（R3）

NDJSON 契约新增 `stop` 事件（README §事件契约 同步）：`{"type":"stop","reason":"canceled"}`。落点：`run_turn.go` Canceled 分支 emit（out 已是 bus writer，web 路径自然 fan-out；CLI 路径 out=os.Stdout 也会多一行——**契约变更**，CLI 消费方需知）。前端 `app.js:233` `sawTerminal` 认 `stop`，渲染"已停止（会话已保存已执行部分）"。

> 备选（契约不变）：web_turn.go goroutine 里 `errors.Is(err, context.Canceled)` 时向 entry 补写 error 事件行。选此则零契约变更，但 stop 与 error 语义混同，前端只能靠文案区分。**推荐前者**（语义干净）。

### P1-F3：mid-data 截断透明化（R4）

`miniagent.Response` 加 `TruncatedStream bool`（领域层标记，L0 #8：不进 wire）；`stream.go:217` 降级时置位；`emitRunResult` 的 result 事件加 `"truncated":true`；前端 result 渲染截断徽标+「重试续跑」快捷按钮（复用现有 retry）。

### P1-F4：llm-proxy 上游断流可观测（运维侧，非本仓库）

llm-proxy 的 `ErrResponseCommitted` 日志当前无法区分「上游掐」vs「下游断」：给 copy 错误分类（upstream read err / client write err 分列），或对 zhipu 启用 circuit_breaker。zhipu 侧治本（升级 glm-5.3 稳定性/换端点）超出本仓库范围。

## §4 文件改动清单（P0 两项）

| 文件 | 改动 |
|---|---|
| `cmd/miniagent/web_turn.go` | F1：`defer cancel()` 迁入 goroutine；busy/早退路径补 cancel |
| `cmd/miniagent/run_turn.go` | F2：Canceled 分支 emit stop 事件（event 包加 EmitStop） |
| `miniagent/event/` | EmitStop |
| `cmd/miniagent/webstatic/static/app.js` | sawTerminal 认 stop；渲染"已停止"卡片 |
| `README.md` | 事件契约补 `stop` |

测试：web_live_test.go 补「stop 后流含 stop 事件」断言；run_turn 单测 Canceled 路径 emit 断言。

## §5 验收 DoD

1. turn 运行中刷新页面 → journal 无 `read sse: context canceled`，turn 继续跑完（live 重连可见 result）。
2. 点停止 → 前端显示"已停止"，不再落「连接中断」文案。
3. zhipu 掐流复现时（不可控）：答案半截 + result 事件仍达（现状保持），P1 落地后带 truncated 标记。
4. verify-gate 全绿；`go test -race ./cmd/miniagent/ -run Web` 双窗口用例过。

## §6 明确不做

- 不在 miniagent 侧做 mid-data 自动续跑（重放半流会重复输出，P2-4 irrevocable 结论维持）。
- 不改 llm-proxy（另一仓库，另立任务）。
- 不给 zhipu 换端点/模型（用户决策，不在代码层擅动）。
- 不新增日志（现有 WARN+对齐法已足够定位；F4 落在 llm-proxy 侧）。

## §7 残留疑问（记录，不阻塞）

- zhipu 坏帧的精确形态（带换行截断 JSON）未抓到原始样本——概率低，加日志抓样本收益低于 P0 修复；若 P0 后仍频发再补 debug 级原始帧日志。
