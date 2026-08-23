# WebUI 排版 v3：IDE 风格布局全面重新设计

基线：`1216a10`（P0-P2 修复已落地，三行骨架已恢复）。
前置文档：`WEBUI_LAYOUT_V2.md`（三行骨架规格）、`WEBUI_LAYOUT_ROOTCAUSE.md`（根因排查）。
参考：第三方 AI 编程助手 UI（截图所示——左图标栏 + 顶部标签页 + 卡片式步骤流 + 富状态栏 + 项目树面板）。

---

## §0 核心改动一览

| # | 改动 | 级别 | 文件 |
|---|------|------|------|
| 0 | 页面骨架改为"左图标栏 + 右主区" | 结构 | app.css, index.html |
| 1 | 240px 侧栏 → 56px 垂直图标栏（对话/新建/历史/搜索） | 导航 | app.css, index.html, app.js |
| 2 | 头部下方加标签栏（对话/轨迹），切换替代右侧 ≡ 按钮 | 标签 | app.css, index.html, app.js |
| 3 | 会话列表 → 图标栏点击弹出侧滑面板 | 面板 | app.css, app.js, views.js |
| 4 | 消息卡片加步骤图标（Think/Bash/Result 等类型标记） | 消息 | app.css, events.js |
| 5 | 状态栏丰富：轮次/步骤/LLM耗时/工具耗时/token/缓存率 | 状态栏 | app.css, store.js, usage.js, app.js |
| 6 | 输入区整洁：移除冗余元素，加权限选择器 | 输入区 | app.css, index.html |
| 7 | 项目树面板：会话列表改为文件树式分组 | 面板 | app.css, app.js, 新建 panel.js |

---

## §1 目标布局（参考截图）

```
┌──────┬──────────────────────────────────────────────────────┐
│      │ header: ☰ miniagent  会话·token              ◐ 退出 │
│ icon ├──────────────────────────────────────────────────────┤
│ bar  │ tab bar:  [对话]  [轨迹]              [确认] [日志↓] │
│ 56px ├──────────────────────────────────────────────────────┤
│      │                                                      │
│ 对话  │  main content (card-based step timeline)            │
│ 新建  │                                                     │
│ 历史  │  [icon] Think  …                                    │
│ 搜索  │  [icon] Bash  > Stage and check commit 1             │
│      │  [icon] Think  …                                    │
│      │  [icon] Bash  > Commit all 6 fixes                    │
│      │  [icon] Result  …                                    │
│      │                                                     │
│      ├──────────────────────────────────────────────────────┤
│      │  status bar: 11轮·103步  LLM 53m13s  工具1m14s      │
│      │              首tok 7.4s  20tok/s  缓存 87%  输入6.7M │
│      ├──────────────────────────────────────────────────────┤
│      │  input: [📎] 给智能体发消息…  [Full access ▾]  [发送] │
└──────┴──────────────────────────────────────────────────────┘
```

### 断点响应策略

| 断点 | 宽度 | 行为 |
|------|------|------|
| 桌面 | ≥960px | 图标栏常显，标签栏可见，会话面板可滑出 |
| 平板 | 640-959px | 图标栏压缩为 44px，标签栏文本隐藏只留图标，会话面板全屏遮罩 |
| 窄屏 | <640px | 图标栏浮动收起（☰），底部输入区固定，标签栏折叠进汉堡菜单 |

---

## §2 组件规格（逐项）

### 2.1 页面骨架（§0-0）

**当前**（1216a10 后）：
```
#app.on { display: grid; grid-template-rows: var(--header-top) minmax(0, 1fr) auto; height: 100dvh; }
#layout { display: grid; grid-template-columns: var(--sidebar-width) 1fr; grid-template-areas: "nav main"; }
footer { /* composer + status-bar */ }
```

**目标**：改为左图标栏 + 右通栏主区，移除 `#layout` 的 grid 两列，改为 flex 行。

```css
:root {
  --iconbar-width: 56px;
  --tab-height: 40px;
  --status-height: 28px;
  --input-height: auto; /* 自适应 */
}
#app.on {
  display: grid;
  grid-template-rows: var(--header-top) var(--tab-height) minmax(0, 1fr) var(--status-height) auto;
  grid-template-columns: var(--iconbar-width) 1fr;
  grid-template-areas:
    "icon header"
    "icon tabs"
    "icon main"
    "icon status"
    "icon input";
  height: 100dvh;
}
#iconbar { grid-area: icon; }
header { grid-area: header; }
#tabs { grid-area: tabs; }
main { grid-area: main; }
#status-bar { grid-area: status; }
#composer { grid-area: input; }
```

**删除**：`#layout` 栅格、`#nav` 侧栏（移入图标栏面板）。

### 2.2 图标导航栏（§0-1）

**HTML**（新 `<nav id="iconbar">`，替代当前 `#nav`）：

```html
<nav id="iconbar">
  <button class="icon-btn active" data-panel="sessions" aria-label="对话">💬</button>
  <button class="icon-btn" data-panel="new" aria-label="新建">＋</button>
  <button class="icon-btn" data-panel="history" aria-label="历史">🕐</button>
  <button class="icon-btn" data-panel="search" aria-label="搜索">🔍</button>
  <span class="flex-1"></span>
  <button class="icon-btn" data-panel="settings" aria-label="设置">⚙</button>
</nav>
```

**CSS**：

```css
#iconbar {
  display: flex;
  flex-direction: column;
  align-items: center;
  gap: 4px;
  padding: 8px 0;
  background: var(--bg-elev-1);
  border-right: 1px solid var(--border);
  z-index: 30;
}
.icon-btn {
  width: 40px; height: 40px;
  display: grid; place-items: center;
  border: none; border-radius: var(--radius-sm);
  background: transparent; color: var(--muted);
  font-size: 18px; cursor: pointer;
  transition: background var(--transition-fast), color var(--transition-fast);
}
.icon-btn:hover { background: var(--bg-hover); color: var(--fg); }
.icon-btn.active { background: var(--accent); color: #fff; }
```

**JS**：点击图标切换面板，点击同一图标关闭面板。

### 2.3 侧滑会话面板（§0-3）

点击 💬（对话）图标弹出侧滑面板，宽度 280px，显示会话列表 + 项目树。

**HTML**（新 `<div id="session-panel">`）：

```html
<div id="session-panel" class="slide-panel" hidden>
  <div class="panel-head">
    <span class="panel-title">对话</span>
    <button class="panel-close ghost" type="button" aria-label="关闭">✕</button>
  </div>
  <div class="panel-body">
    <div class="panel-search"><input placeholder="搜索会话…"></div>
    <div id="session-tree"></div>
  </div>
</div>
```

**CSS**：

```css
.slide-panel {
  position: fixed; left: var(--iconbar-width); top: calc(var(--header-top) + var(--tab-height));
  bottom: calc(var(--status-height) + var(--input-height));
  width: 280px; z-index: 25;
  background: var(--bg); border-right: 1px solid var(--border);
  display: flex; flex-direction: column;
  transition: transform var(--transition-med);
}
.slide-panel[hidden] { display: none; }
/* 窄屏全屏遮罩 */
@media (max-width: 639px) {
  .slide-panel { left: 0; width: 100vw; bottom: 0; z-index: 35; }
}
```

**会话列表渲染**：从 `app.js` 的 `loadSessions()` 改写入 `#session-tree`，保留现有 `sess-item` 结构，新增项目分组能力。

### 2.4 标签栏（§0-2）

**HTML**：

```html
<div id="tabs">
  <button class="tab active" data-tab="chat">对话</button>
  <button class="tab" data-tab="trajectory">轨迹</button>
  <span class="flex-1"></span>
  <span id="tab-meta" class="muted tab-meta"></span>
</div>
```

**CSS**：

```css
#tabs {
  display: flex; align-items: center;
  padding: 0 var(--space-md); gap: 0;
  background: var(--bg);
  border-bottom: 1px solid var(--border);
}
.tab {
  padding: 6px 16px; font-size: 13px;
  border: none; border-bottom: 2px solid transparent;
  background: transparent; color: var(--muted); cursor: pointer;
  transition: color var(--transition-fast), border-color var(--transition-fast);
}
.tab:hover { color: var(--fg); }
.tab.active { color: var(--accent); border-bottom-color: var(--accent); }
.tab-meta { font-size: 11px; }
```

**JS**：点击标签切换视图。`chat` 标签显示对话区（当前 `#events`）；`trajectory` 标签显示轨迹面板（不再作为右侧浮动面板）。

### 2.5 消息卡片步骤图标（§0-4）

**当前**：`.ev.tool summary::before { content: "◈"; }`，所有工具步骤统一菱形图标。

**目标**：按事件类型使用不同图标，形成类似截图的视觉时间线。

在 `events.js` 的 `appendToolUse` 和 `appendUserPrompt` 等函数中，按 `ev.type` 或 `ev.name` 选择图标：

```js
const STEP_ICONS = {
  think: "💭",
  bash: ">_",
  read: "📄",
  write: "✏️",
  edit: "📝",
  search: "🔍",
  web: "🌐",
  result: "✅",
  error: "❌",
  user: "👤",
  default: "◈",
};
function getIcon(name) {
  const key = (name || "").toLowerCase().split(/[_\s]/)[0];
  return STEP_ICONS[key] || STEP_ICONS.default;
}
```

**CSS**：

```css
.ev { display: flex; gap: var(--space-sm); }
.ev-step-icon {
  flex: 0 0 28px; height: 28px;
  display: grid; place-items: center;
  border-radius: 50%; background: var(--bg-elev-1);
  font-size: 13px; color: var(--muted);
  margin-top: 2px;
}
.ev-body { flex: 1; min-width: 0; }
/* 时间线竖线 */
.ev::before {
  content: ""; position: absolute;
  left: 13px; top: 32px; bottom: -10px;
  width: 2px; background: var(--border);
}
.ev:last-child::before { display: none; }
```

**事件 DOM 结构**（每个 `.ev` 改为）：

```html
<div class="ev tool">
  <div class="ev-step-icon">>_</div>
  <div class="ev-body">
    <!-- 现有 tag/pre 内容 -->
  </div>
</div>
```

### 2.6 富状态栏（§0-5）

**当前**：`#status-bar` 显示版本 + 模型（`justify-content: space-between`）。

**目标**：显示会话统计指标，仅在活跃会话时显示完整数据，空闲时显示版本+模型。

**HTML**：

```html
<div id="status-bar" class="muted">
  <span id="status-metrics"></span>
  <span id="status-version" title="服务端版本"></span>
  <span id="status-model" title="当前模型"></span>
</div>
```

**数据格式**（`#status-metrics` 动态填充）：

```
11轮 · 103步  |  LLM 53m13s  ·  工具 1m14s  |  首 tok 7.4s  ·  20 tok/s  |  缓存 87%  ·  输入 6.7M tok
```

**CSS**：

```css
#status-bar {
  display: flex; align-items: center; gap: var(--space-sm);
  padding: 0 var(--space-md);
  font-size: 11px; border-top: 1px solid var(--border);
  overflow-x: auto; white-space: nowrap;
}
#status-bar span:empty { display: none; }
#status-metrics { flex: 1; }
#status-metrics:empty { display: none; }
.metric-divider { color: var(--border); margin: 0 4px; }
```

**数据来源**：
- 轮次/步骤：从 `result` 事件累计（`ev.steps`）
- LLM 耗时：`turnStartTs` 到 `result` 的 `ts` 差值
- 工具耗时：从 `tool_use` 到 `tool_result` 的累计
- Token 速率：`(input_tokens + output_tokens) / elapsed`
- 缓存：从 `result` 的 `compacted` 标志统计
- 输入量：`tokens.in` 累计

**store.js 新增**：

```js
export function updateMetrics(stats) {
  const el = document.getElementById("status-metrics");
  if (!el) return;
  const parts = [];
  if (stats.rounds) parts.push(`${stats.rounds}轮`);
  if (stats.steps) parts.push(`${stats.steps}步`);
  if (stats.llmTime) parts.push(`LLM ${fmtDuration(stats.llmTime)}`);
  if (stats.toolTime) parts.push(`工具 ${fmtDuration(stats.toolTime)}`);
  if (stats.ttft) parts.push(`首tok ${stats.ttft.toFixed(1)}s`);
  if (stats.tps) parts.push(`${stats.tps.toFixed(0)} tok/s`);
  if (stats.cacheRate !== undefined) parts.push(`缓存 ${(stats.cacheRate * 100).toFixed(0)}%`);
  if (stats.inputTotal) parts.push(`输入 ${fmtTokens(stats.inputTotal)}`);
  el.textContent = parts.join("  ·  ");
}
```

### 2.7 输入区美化（§0-6）

**当前**：textarea + 发送按钮 + 折叠的"工作目录·模型"详情。

**目标**：单行输入框（可扩展）+ 左侧附件按钮 + 右侧权限选择器 + 发送按钮。

**HTML**：

```html
<div id="input-area">
  <div class="input-row">
    <button id="attach-btn" class="ghost" type="button" aria-label="附件">📎</button>
    <textarea id="prompt" placeholder="给智能体发消息" aria-label="输入"></textarea>
    <select id="perm-select" aria-label="权限">
      <option value="full">Full access</option>
      <option value="read">Read only</option>
      <option value="chat">Chat only</option>
    </select>
    <button id="send" type="button">发送</button>
  </div>
  <details id="composer-adv" class="input-adv">
    <summary>工作目录 · 模型</summary>
    <div class="adv-row">
      <input id="workdir" placeholder="工作目录（绝对路径）">
      <button id="workdir-browse" type="button" class="ghost" aria-label="浏览目录">📁</button>
      <select id="model" aria-label="模型"><option>默认模型</option></select>
    </div>
  </details>
</div>
```

**CSS**：

```css
#input-area {
  padding: var(--space-sm) var(--space-md);
  padding-bottom: calc(var(--space-sm) + env(safe-area-inset-bottom, 0px));
  border-top: 1px solid var(--border);
  background: var(--bg);
}
.input-row {
  display: flex; gap: var(--space-sm); align-items: flex-end;
}
#prompt { flex: 1; resize: none; min-height: 40px; max-height: 120px;
  border-radius: var(--radius-md); padding: 8px 12px; font-size: 14px; }
#perm-select {
  font-size: 12px; padding: 4px 8px; border-radius: var(--radius-sm);
  max-width: 120px;
}
#send {
  padding: 8px 20px; background: var(--accent); color: #fff;
  border: none; border-radius: var(--radius-md); font-weight: 500;
}
#send:hover { filter: brightness(1.1); }
#send.danger { background: var(--err); }
```

### 2.8 项目树面板（§0-7）

**HTML**（新 `<div id="project-tree">` 内嵌于会话面板）：

```html
<div id="project-tree">
  <div class="tree-group">
    <div class="tree-group-title">当前项目</div>
    <div class="tree-item active" data-project="miniagent">
      <span class="tree-icon">📁</span>
      <span class="tree-name">miniagent</span>
    </div>
  </div>
  <div class="tree-group">
    <div class="tree-group-title">其他项目</div>
    <div class="tree-item" data-project="tmp">📁 tmp</div>
    <div class="tree-item" data-project="lark-bridge">📁 lark-bridge</div>
  </div>
</div>
```

**CSS**：

```css
.tree-group { margin-bottom: var(--space-sm); }
.tree-group-title { font-size: 11px; color: var(--muted); padding: 4px 8px; text-transform: uppercase; letter-spacing: .5px; }
.tree-item {
  display: flex; align-items: center; gap: 6px;
  padding: 6px 8px; border-radius: var(--radius-sm);
  cursor: pointer; font-size: 13px;
  transition: background var(--transition-fast);
}
.tree-item:hover { background: var(--bg-hover); }
.tree-item.active { background: var(--bg-elev-1); color: var(--accent); }
.tree-icon { flex: none; }
```

**会话分组**：`loadSessions()` 按 `workdir` 前缀分组，每组一个 tree-group。当前活跃会话高亮。点击 tree-item 切换到该会话。

---

## §3 改动文件清单

| 文件 | 改动类型 | 内容 |
|------|----------|------|
| `static/index.html` | 重写 | 删 `#layout`/`#nav`/`#overlay`；增 `#iconbar`/`#tabs`/`#session-panel`/`#project-tree`/`#input-area`；重写 `#composer` 结构 |
| `static/app.css` | 重写 §§ | 骨架改 grid 5行 × 2列；增 iconbar/tabs/slide-panel/tree/step-icon/status-metrics 规则；删 `#layout`/`#nav` 旧规则 |
| `static/app.js` | 重写导航 | 删 menu-btn/nav-open/nav-collapsed 逻辑；增 iconbar 点击切换面板；tab 切换对话/轨迹；session 列表写入 `#session-tree` |
| `static/events.js` | 重构渲染 | `.ev` 增加图标列 + 时间线竖线；`renderEvent` 按类型选图标 |
| `static/store.js` | 新增 | `updateMetrics`、`fmtDuration`、`fmtTokens`；指标累计状态 |
| `static/views.js` | 不变 | 多视图模型不变 |
| `static/config.js` | 不变 | 配置模态不变 |
| `static/usage.js` | 不变 | 用量条逻辑不变（可复用） |
| `static/trajectory.js` | 适配 | 轨迹面板改为 tab 内嵌（不再独立浮动面板） |
| 新文件 `static/panel.js` | 新增 | 侧滑面板管理：打开/关闭、动画、点击外部关闭 |
| `static/live.js` | 不变 | 生命周期流不变 |

---

## §4 实施顺序（4 次提交）

### Commit 1: 骨架重构 + 图标栏 + 标签栏

**范围**：§2.1–§2.4（骨架、图标栏、侧滑面板、标签栏）

**改动**：
- `index.html`：新骨架 DOM，删 `#nav`/`#layout`，增 `#iconbar`/`#tabs`/`#session-panel`
- `app.css`：grid 5行 × 2列替换 `#layout` 栅格；iconbar/tabs/slide-panel 样式
- `app.js`：导航逻辑重写（iconbar 点击切换面板、tab 切换视图）
- 新 `panel.js`：面板管理

**验收**：图标栏 4 图标可见，点击 💬 弹出会话面板，点击 对话/轨迹 标签切换视图，窄屏图标栏折叠。

### Commit 2: 消息卡片 + 步骤图标 + 时间线

**范围**：§2.5

**改动**：
- `events.js`：`renderEvent`、`appendToolUse`、`appendUserPrompt` 增加图标列 + 时间线竖线
- `app.css`：`.ev` 改为 flex 行 + 图标圈 + 竖线

**验收**：消息卡片左侧有对应步骤图标（💭/ >_ / ◈），工具步骤间有竖线连接，最后一条无竖线。

### Commit 3: 富状态栏 + 指标系统

**范围**：§2.6

**改动**：
- `store.js`：`updateMetrics`、`fmtDuration`、`fmtTokens`；`metricsCache` 状态
- `app.js`：在 `renderEvent` 结果回调中更新指标
- `app.css`：状态栏 metrics 样式

**验收**：运行会话后状态栏显示轮次/步骤/LLM耗时/工具耗时/token 速率/缓存率/输入总量；无会话时回落显示版本+模型。

### Commit 4: 输入区美化 + 项目树面板

**范围**：§2.7–§2.8

**改动**：
- `index.html`：`#input-area` 新结构（附件按钮、权限选择器）
- `app.css`：输入区样式、项目树样式
- `app.js`：`loadSessions` 改写入 `#session-tree`，支持项目分组

**验收**：输入区单行简洁，附件按钮、权限选择器、发送按钮排列整齐；项目树显示会话按工作目录分组，点击切换会话。

---

## §5 验收 DoD（整体）

1. **骨架**：`getComputedStyle($("app")).gridTemplateRows === "52px 40px 1fr 28px auto"`（近似）；`gridTemplateColumns === "56px 1fr"`。
2. **图标栏**：4 主图标 + 底部设置图标，活动态高亮；点击切换面板，再次点击关闭。
3. **标签**：对话/轨迹两标签，点击切换；对话标签显示 `#events`，轨迹标签显示轨迹面板（非浮动右栏）。
4. **会话面板**：280px 侧滑，会话列表完整（含新建/删除/运行态圆点），搜索框可过滤。
5. **消息卡片**：每张卡片左侧有步骤图标（`💭` Think / `>_` Bash / `📄` Read / `✅` Result 等），步骤间有竖线连接。
6. **状态栏**：会话中显示完整指标（轮次·步骤 | LLM 耗时 · 工具耗时 | 首 tok · 速率 | 缓存 · 输入）；无会话时显示版本 + 模型。
7. **输入区**：单行 textarea 自动扩展，📎 按钮、权限选择器、发送按钮同级排列；折叠区"工作目录·模型"可展开。
8. **项目树**：会话按工作目录分组，当前项目+活跃会话高亮，点击切换。
9. **三断点**（1280/768/375）：桌面全功能；平板图标栏 44px、面板全屏遮罩；窄屏图标栏浮动、输入区固定、标签栏折叠。
10. **验证**：`node --check` 过所有 JS；`verify-gate` 全绿；`make build` 重编部署后可 curl 核对静态资源。

---

## §6 不变约束（不回归范围）

- 零 Go 变更（纯前端重写，`assets.go` embed 通配已覆盖新增文件——若新增 `panel.js` 需确认 `assets.go:12` 的 embed 指令支持 `*.js` 通配，否则追加文件名）。
- 所有 API/事件契约/SSE/NDJSON 格式不变。
- 多视图模型（views.js）不变，切换会话不中断流式渲染。
- 配置模态（config.js）不变，仍为 `#config-modal`。
- 目录选择器（dirpicker.js）不变，通过 `#workdir-browse` 触发。
- 生命周期同步（live.js）不受影响。
- 认证/登录流程不变。
- 持久化键名（`miniagent.web.*`）不变，用户已有状态不丢失。

---

## §7 评估：为何选此方案

| 方案 | 优点 | 缺点 |
|------|------|------|
| **A（本方案）**：5行×2列 grid + 图标栏 + 标签栏 | 与参考截图一致；导航效率高（鼠标短距移动）；垂直空间利用率高；步骤时间线直观 | 改动量大（4 commits）；需要重建侧栏/导航逻辑 |
| **B**：保持 240px 侧栏，仅加标签栏和步骤图标 | 改动小（~2 commits）；兼容现有侧栏折叠 | 侧栏 240px 浪费水平空间；与参考截图差距大；窄屏体验差 |
| **C**：保留当前三行骨架，仅优化视觉细节 | 零骨架改动；风险最低 | 与参考截图差距最大；导航布局现代感不足 |

**推荐 A**。虽然改动量最大，但参考截图的布局优势明显：56px 图标栏 + 280px 侧滑面板 = 仅 336px 导航开销，比当前 240px 侧栏多 96px 但换来图标栏常显（无需点击 ☰ 即可切换功能）+ 面板按需展开（任务密度高时自动隐藏）。步骤时间线更是直观提升用户体验。

---

## §8 文件树（最终状态）

```
cmd/miniagent/webstatic/static/
├── index.html        # 新骨架
├── app.css           # 重写布局/导航/消息样式
├── app.js            # 重写导航逻辑
├── store.js          # 新增 metrics
├── events.js         # 重构步骤图标+时间线
├── views.js          # 不变
├── config.js         # 不变
├── trajectory.js     # 适配 tab 内嵌
├── panel.js          # 新增：侧滑面板管理
├── dirpicker.js      # 不变
├── live.js           # 不变
├── md.js             # 不变
├── usage.js          # 不变
└── assets.go         # 确认 embed 覆盖
```