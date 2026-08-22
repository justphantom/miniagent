---
layer: L1
type: session
updated: 2026-08-22
---

# 当前会话

## 状态
- 2026-08-22 按 `WEBUI-REVIEW.md` 清单全面落地修复，未提交待审阅。
- 部署反馈修复：`0.0.0.0`/`[::]` 通配监听经 LAN IP / hostname 访问被 Host 门 403（"host not allowed"）。`hostVariants` 通配时并入 loopback 拼写 + `os.Hostname()` + 全部网卡地址（跳过 link-local）；IPv6 Host 去 `[]` 后比较；新增 `web.allowed_hosts` 配置（反代域名/外部 IP 手动豁免）。README WebUI 段同步。
- 服务端：新增 `web_guard.go`（guard 中间件：Host 白名单防 DNS rebinding + Sec-Fetch-Site/Origin 拒绝跨站 + CSP/XFO/nosniff 头）；`/api/turn` 强制 `Content-Type: application/json`（CSRF 简单请求阻断，415）；resume 不存在 → 404（session.go 加 `errSessionNotFound` sentinel）；sessions 列表目录不可读 → 500；NDJSON 响应加 `X-Accel-Buffering: no`；`IdleTimeout=2m`。
- 前端：折叠修复（`.ev.collapsed .md`）；`.out/.err` 样式（错误红边）；model-badge 从下拉/result 回填；overlay 点关；sending 时切会话/新建先 abort 再切换 + `resetTransient()` 清 callID 映射；tool_result 按 `call_id` 精确挂载；输入 ≥16px；safe-area-inset；宽表格 `overflow-x:auto`；等待指示（非流式配置）；模型选择持久化；alert→行内提示；登录页初始隐藏防闪现；aria-label。
- 测试：新增 TestWebGuard/TestHostVariants/TestWebTurnContentTypeEnforced/TestWebTurn_ResumeMissingSession404/TestWebSessionsList_UnreadableDir；存量测试适配（mux 返回 http.Handler、SameSessionSerialized 补 Content-Type）。
- 未做（报告低项，需用户决定）：whoami 版本探测收紧、鉴权爆破限速、`__new__` 全局串行、BroadcastChannel 多标签同步、优雅停机加长 grace。

## 待办
- 用户审阅 diff 后决定提交。

## 备注
- verify-gate 全绿：gofmt 空/build/vet/test -race/golangci-lint 0 issues/≤300 行。
