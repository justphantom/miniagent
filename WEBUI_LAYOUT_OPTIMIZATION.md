# WebUI 排版优化实施方案

基线：v6.4.0+（配置管理页已合并）。方案草图已经用户确认（2026-08 会话）。
仅前端静态资源改动，零 Go 变更，零新依赖。

## §0 硬约束

- 不引入框架/构建器；纯原生 HTML/CSS/ES Module。
- CSP 合规：禁内联样式、禁内联事件处理器。
- 不改动任何 API / 事件契约 / SSE 逻辑。
- 展开/折叠状态持久化一律走 localStorage（沿用 `miniagent.web.*` 键前缀）。
- 完成后 verify-gate 全绿（本方案只动 static/，Go 门禁应无感）。

## §1 现状问题（已确认）

| # | 问题 | 位置 |
|---|------|------|
| 1 | header 9 元素拥挤；`#model-badge` 与 composer 模型下拉重复；`#version-badge` 低价值 | index.html:22-33 |
| 2 | composer 常驻两行，低频的 workdir 永久占位 | index.html:44-55 |
| 3 | 桌面侧栏 240px 强制显示，不可收叠 | app.css:283-287 |
| 4 | 对话区 `max-width: 900px` 偏宽 | app.css:55,79 |
| 5 | 侧栏「⚙ 配置」与「＋新会话」并列，视觉权重错乱 | index.html:36-38 |

## §2 目标布局（确认版草图）

```
┌────────────────────────────────────────────────────────────┐
│ ☰ miniagent  sess-a1b2 · 12.3k tok▕spacer▕≡ ◐ ⏻      │
├──────────┬───────────────────────────────────┬─────────────┤
│ ＋ 新会话 │                                   │ 工具轨迹     │
│──────────│      对话区 (max 780px 居中)       │ (≡ 开关,    │
│ 会话列表  │   user → 右对齐                   │  默认隐藏)   │
│  (1fr)   │   assistant/result → 左对齐       │             │
│          │   ◈ tool ▸ 折叠                   │             │
│          │ ┌───────────────────────────────┐ │             │
│          │ │ 输入任务…              [发送] │ │             │
│          │ │ ▸ 📁workdir · 模型▾  (可折叠) │ │             │
│          │ └───────────────────────────────┘ │             │
│ ⚙ 配置    │                                   │             │
└──────────┴───────────────────────────────────┴─────────────┘
```

断点策略不变：≥800 桌面 / 640-799 平板 / <640 窄屏。

## §3 变更规格

### 3.1 header 瘦身

**index.html header 块改为：**

```html
<header>
  <button id="menu-btn" class="ghost" type="button" aria-label="切换会话列表">☰</button>
  <h1>miniagent</h1>
  <span id="session-id" class="session-id muted" title="当前会话 ID"></span>
  <span id="session-tokens" class="session-id muted" title="当前会话累计 token"></span>
  <span class="flex-1"></span>
  <button id="traj-btn" class="ghost" type="button" title="工具轨迹" aria-label="工具轨迹">≡</button>
  <button id="theme-btn" class="ghost" type="button" title="切换到亮色" aria-label="切换主题">◐</button>
  <button id="logout" class="ghost">退出</button>
</header>
```

- 删除 `#version-badge`、`<span id="model-badge">` 两个 DOM 节点。
- `store.js`：
  - `setVersion(v)`：循环 id 列表改为 `["version-login"]`，注释同步（版本只在登录页展示；配置页已有 cfg-status 可后续承载，不在本期）。
  - 删除 `setModelBadge` 函数。
- `app.js`：import 列表移除 `setModelBadge`；删除 app.js:418 `if (restored) setModelBadge(saved);` 与 app.js:423 `setModelBadge(v);`。
- `app.css`：删除 `#model-badge` 规则（app.css:46）、`#version-badge` 规则（app.css:135）；窄屏媒体查询中 `#model-badge, #traj-btn { display: none; }` 改为仅 `#traj-btn`（app.css:302）；平板查询中 `#version-badge { display: none; }` 删除（app.css:277）。

### 3.2 composer 折叠行

**index.html composer 块改为：**

```html
<div id="composer">
  <div class="composer-row main-row">
    <textarea id="prompt" placeholder="输入任务（桌面 Enter 发送 / Shift+Enter 换行；触屏 Enter 换行，点发送）" aria-label="输入"></textarea>
    <span id="wait" class="muted" hidden></span>
    <button id="send" type="button">发送</button>
  </div>
  <details id="composer-adv">
    <summary>工作目录 · 模型</summary>
    <div class="composer-row adv-row">
      <input id="workdir" placeholder="工作目录（绝对路径）" aria-label="工作目录">
      <button id="workdir-browse" type="button" class="ghost" aria-label="浏览目录">📁</button>
      <select id="model" aria-label="模型"><option>默认模型</option></select>
    </div>
  </details>
</div>
```

- 原生 `<details>`，零 JS 开关逻辑；`summary` 样式化。
- **状态记忆**：`store.js` 新增：

```js
const COMPOSER_ADV_KEY = "miniagent.web.composerAdv";
export function saveComposerAdv(open) { localStorage.setItem(COMPOSER_ADV_KEY, open ? "1" : ""); }
export function loadComposerAdv() { return localStorage.getItem(COMPOSER_ADV_KEY) === "1"; }
```

- `app.js` 启动时（showApp 路径）：`$("composer-adv").open = loadComposerAdv()`；监听 `toggle` 事件持久化：

```js
$("composer-adv").addEventListener("toggle", (e) => saveComposerAdv(e.target.open));
```

- **默认收起**。首次访问用户看不到 workdir/模型行。
- **app.css** 新增：

```css
#composer-adv summary { cursor: pointer; font-size: 12px; color: var(--muted); user-select: none; list-style: none; }
#composer-adv summary::before { content: "▸ "; font-size: 10px; color: var(--accent); }
#composer-adv[open] summary::before { content: "▾ "; }
#composer-adv summary:hover { color: var(--fg); }
#composer-adv .adv-row { margin-top: var(--space-sm); }
```

- 窄屏规则 `#model { width: 100%; flex: none; }`（app.css:310）保留，选择器仍命中（details 内 DOM 不变）。
- textarea 自动增高逻辑（app.js:128-132）不变。

### 3.3 侧栏：桌面可折叠 + 配置沉底

**index.html nav 块调整顺序**（config 按钮移到列表之后）：

```html
<nav id="nav">
  <button id="new-chat" type="button">＋ 新会话</button>
  <div id="session-list"></div>
  <button id="config-btn" type="button" class="ghost nav-config">⚙ 配置</button>
</nav>
```

- **桌面折叠**：`#menu-btn` 全断点显示（删除 app.css:286 `#menu-btn { display: none; }`），桌面点击切换 `body.nav-collapsed`；窄屏维持现有 `body.nav-open` 抽屉语义，两者互斥：

```js
// app.js 替换 app.js:105 的监听器
$("menu-btn").addEventListener("click", () => {
  if (matchMedia("(min-width: 800px)").matches) {
    const c = document.body.classList.toggle("nav-collapsed");
    saveNavCollapsed(c);
  } else {
    document.body.classList.toggle("nav-open");
  }
});
```

- `store.js` 新增：

```js
const NAV_KEY = "miniagent.web.navCollapsed";
export function saveNavCollapsed(c) { localStorage.setItem(NAV_KEY, c ? "1" : ""); }
export function loadNavCollapsed() { return localStorage.getItem(NAV_KEY) === "1"; }
```

- 启动恢复：showApp 时 `document.body.classList.toggle("nav-collapsed", loadNavCollapsed())`。
- **app.css**：

```css
/* 桌面折叠：grid 列归零，nav 隐藏 */
@media (min-width: 800px) {
  body.nav-collapsed #layout { grid-template-columns: 0 1fr; }
  body.nav-collapsed #layout.trajectory-open { grid-template-columns: 0 1fr var(--trajectory-width); }
  body.nav-collapsed #nav { display: none; }
}
```

- **配置沉底**：`#nav` grid 行模板由 `auto 1fr` 改为 `auto 1fr auto`（桌面 app.css:284 与窄屏 app.css:274 两处同步）；`.nav-config` 增加 `border-top: 1px solid var(--border); padding-top: var(--space-md); margin-top: var(--space-sm);`。

### 3.4 对话区收窄

- app.css:55 `#events > .view { max-width: 900px }` → `780px`。
- app.css:79 `.ev { max-width: 900px }` → `780px`。
- `.ev.user { max-width: 720px }` → `620px`（保持约 80% 比例）。

### 3.5 轨迹面板

维持现状（`traj-btn` 开关、默认隐藏、桌面第三栏 / 平板窄屏 fixed overlay）。不改动。

## §4 文件改动清单

| 文件 | 改动 |
|------|------|
| `cmd/miniagent/webstatic/static/index.html` | header 删 2 节点；composer 重排为 details；nav 内 config 沉底 |
| `cmd/miniagent/webstatic/static/app.css` | 删 `#model-badge`/`#version-badge` 规则；新增 composer-adv/nav-collapsed/nav-config 规则；3 处 max-width；媒体查询清理 |
| `cmd/miniagent/webstatic/static/store.js` | `setVersion` 精简；删 `setModelBadge`；新增 4 个持久化函数（2 键） |
| `cmd/miniagent/webstatic/static/app.js` | import 精简；删 2 处 `setModelBadge` 调用；menu-btn 分断点逻辑；composer-adv 状态恢复/监听 |

无 Go 文件变更；`assets.go` embed 通配若已覆盖 `.css/.html` 则无需改（实施时确认 embed 模式）。

## §5 验收 DoD

1. 桌面：header 仅 7 元素；无 version/model 徽标残留（DOM 与 CSS 均删净，`grep -r "model-badge\|version-badge" cmd/` 仅命中本文件以外的 0 处——实际应为 0 处）。
2. composer 默认单行；点 summary 展开可见 workdir/📁/模型下拉；刷新后展开态保持；目录选择器、模型下拉功能不回退。
3. 桌面 ☰ 折叠/展开侧栏，刷新后状态保持；<800px ☰ 仍是抽屉 + 遮罩，互不干扰。
4. 对话区 780px 居中；user 消息右对齐 620px。
5. ⚙ 配置按钮位于侧栏底部，点击正常进入配置页。
6. 三断点（1280 / 768 / 375）人工走查无布局破碎；to-bottom、轨迹面板、模态行为不回退。
7. verify-gate 全绿。

## §6 提交计划（一次一事，3 commits）

1. `web: slim header, drop version/model badges` —— §3.1
2. `web: collapse composer advanced row into details` —— §3.2
3. `web: collapsible desktop sidebar, pin config to bottom, narrow chat column` —— §3.3 + §3.4

每个 commit 前给用户 diff 摘要待审阅（L0 约束 #2）。
