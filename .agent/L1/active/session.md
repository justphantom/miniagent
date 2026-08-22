---
layer: L1
type: session
updated: 2026-08-22
---

# 当前会话

## 状态
- 2026-08-22 ASSESSMENT.md 实施审查完成：P0 全部、P1 大半、P2 部分、P3 三项（M3/M4/M5/L15）已落地（未提交，最高约束）。审查中发现 4 个缺陷并当场修复：web_turn.go 缺 import+调用不存在方法致 build 红（改直接 WithTimeout）、app.js `currentSession` 未定义致 M8 防线失效（落实 generation 比较）、M3 SSRF 修复引入私网放行回归（`IsGlobalUnicast` 对 RFC1918 为 true，恢复原谓词叠加）、emit.go 收窄 DangerWords 越权行为变更（还原默认）、M9 placeholder 文案写反（改正）、md.js 链接补协议白名单。
- verify-gate 复跑全绿（gofmt/build/vet/test -race/golangci-lint 0 issues/≤300 行）。
- 遗留质量收尾完成：M2 全清（README:43 示例改 `SummaryMaxChars: 5000`、钩子表补 OnStep 行）；H4 补「不属于漏洞」第三条（文件工具零路径约束）；HOOKS.md OnStep 溯源 main.go:179→run_turn.go；confirmInline 参数化 okText；M8 replay 读流覆盖（openSession 快照 gen，视图切换即停绘，含 title/focus 守卫）。
- 未实施：L11/L12/L14/L16/L17/L18/L19/L20、M6 thinking 下拉。
- 2026-08-22 早先：按 `WEBUI-REVIEW.md` 清单全面落地修复，已提交（2abf666 等，见 git log）。
- 部署反馈修复：`0.0.0.0`/`[::]` 通配监听经 LAN IP / hostname 访问被 Host 门 403（"host not allowed"）。`hostVariants` 通配时并入 loopback 拼写 + `os.Hostname()` + 全部网卡地址（跳过 link-local）；IPv6 Host 去 `[]` 后比较；新增 `web.allowed_hosts` 配置（反代域名/外部 IP 手动豁免）。README WebUI 段同步。
- 服务端：新增 `web_guard.go`（guard 中间件：Host 白名单防 DNS rebinding + Sec-Fetch-Site/Origin 拒绝跨站 + CSP/XFO/nosniff 头）；`/api/turn` 强制 `Content-Type: application/json`（CSRF 简单请求阻断，415）；resume 不存在 → 404（session.go 加 `errSessionNotFound` sentinel）；sessions 列表目录不可读 → 500；NDJSON 响应加 `X-Accel-Buffering: no`；`IdleTimeout=2m`。
- 前端：折叠修复（`.ev.collapsed .md`）；`.out/.err` 样式（错误红边）；model-badge 从下拉/result 回填；overlay 点关；sending 时切会话/新建先 abort 再切换 + `resetTransient()` 清 callID 映射；tool_result 按 `call_id` 精确挂载；输入 ≥16px；safe-area-inset；宽表格 `overflow-x:auto`；等待指示（非流式配置）；模型选择持久化；alert→行内提示；登录页初始隐藏防闪现；aria-label。
- 测试：新增 TestWebGuard/TestHostVariants/TestWebTurnContentTypeEnforced/TestWebTurn_ResumeMissingSession404/TestWebSessionsList_UnreadableDir；存量测试适配（mux 返回 http.Handler、SameSessionSerialized 补 Content-Type）。
- 未做（报告低项，需用户决定）：whoami 版本探测收紧、鉴权爆破限速、`__new__` 全局串行、BroadcastChannel 多标签同步、优雅停机加长 grace。

## 待办
- 第二轮评估落盘 ASSESSMENT.md（覆盖上轮报告）：上轮 45 项整改逐条核验全部真实落地；verify-gate 复跑全绿（557 测试）。新发现 15 项：中危 3（N1 CSP 阻断 index.html:10,20 内联 style 属性致每次刷新双面板 FOUC；N2 confirmInline Enter 在 btnCancel 聚焦时=确认删除，方向危险；N3 serve 每 turn 新建 Transport 无连接复用且无 IdleConnTimeout，fd 慢性累积）+ 低危 12（有序列表缺失/会话空态/README 两处微漂移/assessment.md 违 L1 约定/并发 turn 无 409/主题无态指示/静态名单双维护/网卡快照/无限速/回放 elapsed 失真/workdir 回填竞态等）。修复建议按 P0(N1/N2)→P1(N3/N4/N7)→P2→P3 排序，是否实施待用户定。

## 备注
- verify-gate 全绿：gofmt 空/build/vet/test -race/golangci-lint 0 issues/≤300 行。
