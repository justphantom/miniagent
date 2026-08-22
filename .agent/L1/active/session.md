---
layer: L1
type: session
updated: 2026-08-23
---

# 当前会话

## 状态
- WEBUI_SYNC_PLAN.md 全部落地（P0-P4），verify-gate 全绿（gofmt 空/build/vet/test -race 14 包/golangci-lint 0 issues/JS node --check 全过/非测试 .go ≤300 行），6 个提交（7f39383→143371a），工作树干净。
- 决策 D1-D4 全按推荐实施并记录在 WEBUI_SYNC_PLAN.md §11 与 L2 webui-architecture §7-§10。

## 落地清单（commit → 内容）
- 7f39383 docs: 方案文档+决策确认（WEBUI_SYNC_PLAN.md）。
- 6f51cbb P0: thinking 流式 markdown——300ms 节流 mdRender、首块立即绘制、>64KB 停重绘、隐藏视图跳过、复制/折叠仍 finish 绑定。
- aed9e02 P1: turnRegistry 事件总线（web_bus.go）——turn ctx 派生 baseCtx（断连/切视图/关标签不杀轮次，nolint:contextcheck 有注释）、预生成 id 删 `__new__` 全局锁、stop API、`web.max_concurrent_turns`（429）、Write 恒 nil error、beginDelete 承载删除互斥。测试：web_bus_test.go 7 例 + web_turn_test.go 5 新例（断连不杀/stop/并发新建/上限/shutdown）。
- 42169ed P2: web_live.go——GET /api/events（hello/turn_started/turn_finished/session_deleted/15s ping）、GET /api/sessions/{id}/live（事件 0 重放+live_end）、列表 running 字段；live.js（fetch NDJSON、2s 重连、attachLive 旁观）。
- 0ab7467 P3: 前端多视图——views.js（#events 视口+每会话 .view、rekey、≤8 逐出、running 不逐出）、events.js 视图化（per-view curText/toolNodes/tokens/gen）、app.js 重写（切换不 abort、发送变停止走 stop API、409 升级旁观、lifecycle 驱动列表/旁观）、store.js 瘦身（会话态入 view）。
- 143371a P4: CHANGELOG（Unreleased 含 Breaking 断连语义）、README WebUI 节重写、ARCHITECTURE cmd 树、L2 决策 §7-§10、config.example.json。

## 关键实现事实（供后续轮次追溯）
- 引擎 out 写 turnEntry；HTTP handler 只是订阅者：首事件前等 `<-ch`，!ok 时按 err 映射 404/500/204（writeTurnError），保持旧 JSON 错误契约。
- 订阅握手在 entry.mu 内快照+注册原子化（无重无漏）；慢订阅 chan(64) 满即关闭淘汰；finish 关闭全部订阅 chan（缓冲可排空）。
- live 重放含 `live_truncated {dropped:N}` 首行（缓冲 20000 行超限丢最旧）。
- 测试技巧：blockLLM{entered,release}；流式断言用 mutex 化 syncRecorder（httptest.Recorder 非并发安全）。
- Go 原始字符串 `` `...\n` `` 内 \n 是字面量——曾致总线测试少一行（已修，测试统一用 "...\n" 解释串）。

## 待办
- 无。P3 观察项 O7/O8 中 O8（并发上限）已顺带落地为 opt-in；O7 维持待决策。
- 发布时 CHANGELOG Unreleased 段并入版本号。

## 备注
- 冒烟实测（真实服务）：/api/events hello、/api/turn 契约不变、列表 running 字段、live_end 均符合预期。
- 提交已按用户 2026-08-23 明确指令执行（覆盖 CLAUDE.md 禁提交的默认约束）。
