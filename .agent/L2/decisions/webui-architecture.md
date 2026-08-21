# WebUI 架构决策

## 状态
- superseeds: none
- superseded_by: none
- updated: 2026-08-21

## 背景
v5.0.0 起 `miniagent -serve` 提供 WebUI，v6.1.0 全面优化体验。需记录前端架构选型理由。

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