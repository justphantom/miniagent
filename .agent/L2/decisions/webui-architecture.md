---
layer: L2
type: decision
tags: [webui, frontend, architecture, es-modules, sync, streaming, markdown, vanilla-js]
created: 2026-08-11
updated: 2026-08-24
confidence: high
---

# WebUI 架构决策

## 状态
- superseeds: none
- superseded_by: none
- updated: 2026-08-24

## 背景
v5.0.0 起 `miniagent -serve` 提供 WebUI，v6.1.0 全面优化体验。需记录前端架构选型理由。v7（2026-08-23，WEBUI_SYNC_PLAN.md 落地）补多会话并发/跨浏览器同步/流式 markdown 三项决策（§7-§10）。

## 决策

### 1. 前端零构建链（纯 vanilla + ES modules）
- 原因：项目无前端构建流程（no webpack/vite/rollup），`go:embed` 直接嵌入 `.js` 文件。引入构建链需增加 `package.json` / `node_modules` / CI 编译步骤，与项目「纯 stdlib」哲学冲突。
- 约束：仅用 ES modules（`import/export`）、无 transpile、无 polyfill。目标浏览器：现代 Chrome/Firefox/Safari（Chromium 系嵌入式终端）。

### 2. Markdown 自研子集（非 marked.js/DOMPurify）
- 原因：避免引入第三方依赖（CDN 离线不可用、增加 XSS 攻击面），且 markdown 子集需求明确（标题/粗体/斜体/代码/列表/引用/表格/删除线），无需完整 GFM。
- 安全：严格先 `esc()`（HTML 实体转义 `&<>"`）再 markdown 替换，杜绝 XSS。`innerHTML` 仅注入转义后内容。用户 prompt 保持 `textContent` 不渲染。

### 3. 流式累积→一次性渲染
- 原因：`text_delta` 可每秒发数十次，每次 `innerHTML` 重解析整个 DOM 导致 O(n²)（n=delta 数）。累积到 `d._md` 字符串，`finishText()`（遇 result/error/工具打断）时一次性渲染，避免闪烁和性能问题。

### 4. 前端拆 4 个 ES module
- `md.js`：markdown 渲染（esc/mdInline/mdRender）
- `store.js`：全局状态 + localStorage 持久化（key/token 累计/version/theme/workdir）
- `events.js`：NDJSON 事件→DOM 渲染（evDiv/appendDelta/appendToolUse/appendToolResult/finishText/renderEvent/折叠/复制按钮）
- `app.js`：引导/鉴权/发送/中断/会话/滚动/主题/快捷键
- 原因：`app.js` 在 v6.0.0 已达 400 行，未来功能继续加会超过可维护阈值。无构建链下的 module 拆分通过 `import` 语句完成，浏览器 HTTP/2 并发加载 4 个小文件开销可忽略。

### 5. 后端 API 设计
- `GET /api/whoami`：公开（无鉴权），返回 `auth_required`/`version`，供前端探测。
- `POST /api/turn`：NDJSON 流式，契约与 CLI stdout 逐字节一致。`session=""` 新建、非空续跑。同 session 轮次经 `sync.Mutex` 串行化（防 `RewriteMessages` 首行竞态）。
- `GET /api/sessions`：列表，`LoadSessionMeta` 只读首行（O(元数据行) 非 O(全文件)）。v6.1.0 加 `preview` 字段（尾扫 ≤64KB 取最后 assistant 文本）。
- `GET /api/sessions/{id}`：重放最近 200 条消息为 NDJSON 事件流。
- `DELETE /api/sessions/{id}`：v6.1.0，TryLock 持有中轮次→409。
- `GET /api/models`：provider/model 下拉数据源。
- 鉴权：`x-api-key` 头 + `subtle.ConstantTimeCompare`。key 空时仅允许 loopback listen。

### 6. 主题系统
- CSS 变量双主题（`:root` 暗色 / `[data-theme="light"]` 亮色），localStorage 记忆，页面加载时 `document.documentElement.dataset.theme` 恢复。无 JS 切换→CSS 变量继承，O(1) 切换开销。

### 7. turn 事件总线与断连解耦（D1）
- turnRegistry（web_bus.go）是同会话互斥、事件缓冲与扇出的唯一载体；引擎的 NDJSON 写进 turnEntry（io.Writer），HTTP 请求方只是订阅者之一。
- **turn ctx 派生自服务器 baseCtx 而非 r.Context()**：断连/关标签/切视图不杀轮次；停止走 `POST /api/sessions/{id}/stop`（entry.cancel → 取消路径保存已执行部分）。breaking：曾依赖「关页面=停」的用法需迁移。
- **总线 Write 恒返回 nil error**：OnToolUse 的 emit 错误会终止轮次（tool_handler.go 直接上抛），死订阅者不得反杀引擎；慢订阅者（chan 64 满）被关闭淘汰，客户端按 D3 重建。
- 新会话 id 预生成（turnSpec.sessionID→resolveSession presetID）：注册键恒为真实 id，两个并发新会话真并行（原 `__new__` 全局锁已删）。
- `web.max_concurrent_turns`（默认 0 不限）：信号量非阻塞获取，溢出 429。

### 8. 传输选 fetch NDJSON 而非 EventSource
- EventSource 无法带 `x-api-key` 头（标准限制），query 传 key 会泄漏进日志；前端已有成熟 fetch reader 循环可复用；CSP `connect-src 'self'` 已放行。
- 生命周期流 15s `ping` 防代理空闲断连；断开后 2s 退避重连（状态经列表重同步）。

### 9. live 流「事件 0 全量重放」而非游标增量
- 进行中轮次只存在于总线缓冲（session jsonl 轮末才落盘），磁盘 replay 与 live 天然无重叠，客户端零去重。
- 缓冲上限 20000 行，超限丢最旧并在重放首行置 `live_truncated {dropped:N}` 诚实截断。
- 断开重连（D3）按「重建视图」处理：replay（已落盘轮）+ live（当前轮缓冲）重新拼时间线，不做轮次代去重（YAGNI）。

### 10. 前端多视图 + 流式 markdown
- #events 是视口，每会话一个 .view（views.js）：切换换 display 不 abort；M8 generation 降为 per-view gen；idle 视图 >8 逐出（running 永不逐出）。
- 流式渲染从「纯文本累积、finish 一次性 mdRender」改为「300ms 节流 mdRender 增量重绘」：thinking 流式可达分钟级，原始 md 语法直出不可接受；节流把重解析成本从 O(delta) 降到 ~3次/s，>64KB 停止重绘等 finish，隐藏视图跳过重绘，复制按钮/折叠仍在 finish 一次性绑定。

### 11. 工具卡片统一「预览 + 展开按钮」（弃 details）
- 交互形态（910a4c7）：工具卡（聊天流 events.js + 轨迹面板 trajectory.js）不用 `<details>` 折叠，恒显裁剪预览（前 2 行且 ≤90 字符，超出省略号），被裁剪时出「展开完整内容」按钮切换全文/预览。根因：原 details 展开后看到的仍是裁剪文本（title 提示完整内容），两套折叠语义割裂；统一后预览即所得，展开即全文。阈值是用户拍板的 UI 偏好，改值须同步两文件常量（TOOL_PREVIEW_CHARS/LINES）。
- 镜像常量防环：clip 逻辑在 trajectory.js 复制而非共享——events.js 已 import trajectory.js（refreshPanel），反向抽公共模块成环。与 compaction↔policy 常量镜像同构，但无 JS 测试基建，一致性靠注释指向，改动时须两处同步。
- 验证门槛：静态 JS 无测试框架（不引入，与零构建链同哲学），门槛 = `node --check` 语法 + Go verify-gate（embed 编译含静态资源）。