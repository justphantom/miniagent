---
layer: L1
type: session
updated: 2026-08-23
---

# 当前会话

## 状态
- 用户要求：深入评估并输出实现方案文档——①WebUI 多会话同时进行 ②多浏览器消息/状态同步 ③thinking 文本渲染 markdown。
- 已产出 `WEBUI_SYNC_PLAN.md`（评估+方案，分 P0-P4 五阶段），**未写实现代码**，待用户确认决策点 D1-D4。

## 关键评估结论（证据已核）
- ①服务端不同 session 已可并发（per-session TryLock；共享状态核查全线程安全）；blocker：`__new__` 全局锁串行新建会话（web_turn.go:69）+ 前端切视图 abort fetch → r.Context() 取消 → 杀在途轮次（app.js:88/399）。
- ②无推送通道；NDJSON 只发发起者；进行中轮次磁盘无落盘（saveSession 在 Run 后）→ 跨浏览器观战必须服务端缓冲。
- ③node DOM shim 实测：finishText 已对 reasoning mdRender（与 CHANGELOG 自述一致）；差距在**流式阶段**纯文本直出原始 md 语法——thinking 恰是流式最长的可见内容。
- 方案核心：turnRegistry 事件总线（引擎写总线、扇出订阅者、Write 恒 nil——OnToolUse emit 错误会终止轮次 tool_handler.go:35）+ turn ctx 脱离 r.Context() + 预生成 id 去 `__new__` + stop/live/events 三端点 + 前端多视图 detached DOM + thinking 300ms 节流重绘。
- 传输选 fetch NDJSON 不选 EventSource（无法带 x-api-key 头）。

## 待办
- 用户确认 D1 断连语义（推荐继续执行+stop 接管）/ D2 并发上限默认（推荐 0）/ D3 live 重连（推荐重建视图）/ D4 前端不引入测试框架。
- 确认后按 P0(thinking)→P1(registry+解耦+stop)→P2(live/events)→P3(多视图)→P4(文档) 实施。

## 备注
- 工作树：新增 WEBUI_SYNC_PLAN.md 未提交（CLAUDE.md 禁止提交）；HEAD 0699c2e。
