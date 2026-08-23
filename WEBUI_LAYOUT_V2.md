# WebUI 排版优化 v2：三行骨架 + 配置模态化

基线：`84a303a`（三列布局 + header 瘦身 + composer 折叠 + 侧栏折叠已落地）。
前置决策（用户已确认）：
- 底部 = composer 输入区 + 状态栏两行，**整页通栏**
- 三列（侧栏/主区/轨迹）夹在 header 与 footer 之间，仅中间行内容滚动
- 配置改**大尺寸居中模态**（90vw×85vh，内部滚动），窄屏全屏

## §0 硬约束

沿用 v1：纯原生、零依赖、CSP 合规、不动 API/事件契约、状态持久化走 localStorage。零 Go 变更。

## §1 现状问题

| # | 问题 | 位置 |
|---|------|------|
| 1 | 页面只有两行：header + 内容区；composer 嵌在 main 列内，开轨迹面板时输入区被挤窄 | index.html:39-58 |
| 2 | header 是 `position: sticky` 而非骨架行，依赖毛玻璃遮掩滚动内容 | app.css:38-41 |
| 3 | `#to-bottom` 用 `position: fixed` + 媒体查询补偿轨迹宽度，脆弱 | app.css:102-105, 317 |
| 4 | 配置是整页渲染，清空 `#events` 对话区（`renderConfigPage` 直接 `vp.innerHTML=""`），退出配置后对话视图需重建 | config.js:445-458 |

## §2 目标骨架

```
┌──────────────────────────────────────────────────────────────┐
│ header（固定，行1）☰ miniagent sess-id tok        ≡ ◐ ⏻ │
├──────────┬────────────────────────────────────┬──────────────┤
│ 侧栏      │ #events（唯一滚动区）              │ 轨迹面板     │
│ (自滚动)  │                                    │ (自滚动)     │
│          │                                    │              │
├──────────┴────────────────────────────────────┴──────────────┤
│ composer：输入… [发送]  / ▸ 工作目录·模型        （固定，行3）│
│ 状态栏：v6.4.0 · provider/model                              │
└──────────────────────────────────────────────────────────────┘
```

分工：**header = 会话上下文**（会话/token）；**footer 状态栏 = 全局信息**（版本/当前模型）。

## §3 变更规格

### 3.1 三行骨架

**index.html**：`#composer` 从 `<main>` 移出，新增 `<footer>` 包裹 composer + 状态栏；状态栏置于最底：

```html
<main>
  <div id="events" aria-live="polite" aria-label="对话内容"></div>
  <button id="to-bottom" type="button" title="回到底部" aria-label="回到底部">↓</button>
</main>
</div><!-- /#layout -->
<footer>
  <div id="composer">…现有结构不变…</div>
  <div id="status-bar" class="muted">
    <span id="status-version" title="服务端版本"></span>
    <span id="status-model" title="当前模型"></span>
  </div>
</footer>
```

**app.css**：

```css
#app { display: grid; grid-template-rows: var(--header-top) minmax(0, 1fr) auto; height: 100dvh; }
header { position: static; /* 删 sticky/backdrop-filter，保留 border-bottom */ }
#layout { min-height: 0; /* 行2 内容自滚动，骨架不滚 */ }
footer { border-top: 1px solid var(--border); background: var(--bg); z-index: 40; }
#status-bar { display: flex; gap: var(--space-lg); padding: 2px var(--space-lg);
  padding-bottom: calc(2px + env(safe-area-inset-bottom, 0px));
  font-size: 11px; border-top: 1px solid var(--border); }
#status-bar span:empty { display: none; }
#composer { border-top: none; /* border 移到 footer */ padding-bottom: var(--space-sm); }
```

- `#composer` 原有 `padding-bottom: safe-area` 移至 `#status-bar`（状态栏是最底元素）。
- 窄屏抽屉/轨迹 overlay（z 20/30）纵向仍贯通到视口底，但 **footer z-index 40 不透明覆盖其下缘**——零测量解决重叠，与 header 同层一致。
- 删除 `header` 的 `backdrop-filter` 相关两行（骨架行不再需要遮掩）。

### 3.2 状态栏数据

**store.js**：
- `setVersion(v)`：恢复双写，目标改为 `["version-login", "status-version"]`。
- 新增 `setStatusModel(m)`：写 `#status-model`（语义同被删的 `setModelBadge`，落点改状态栏）。

**app.js**：`loadModels` 恢复/切换模型时调 `setStatusModel(v)`（原位恢复 84a303a 删除的两处调用，函数名换新）。

### 3.3 #to-bottom 简化

```css
#to-bottom { position: absolute; right: 16px; bottom: var(--space-lg); /* 其余不变 */ }
```

- main 已 `position: relative`（app.css:51），absolute 相对 main 定位；轨迹开/关不再影响位置。
- 删除 `#layout.trajectory-open #to-bottom` 规则（app.css:105）与窄屏 `#layout.trajectory-open #to-bottom { display:none }`（app.css:317，轨迹全宽覆盖后按钮本就被遮，规则冗余）。

### 3.4 配置模态化

**index.html** 新增模态壳（放 `#dirpicker` 旁）：

```html
<div id="config-modal" class="modal" hidden>
  <div class="modal-panel cfg-modal-panel">
    <div class="modal-head">
      <span class="modal-title">配置管理</span>
      <span id="cfg-status" class="cfg-status"></span>
      <button id="cfg-close" class="ghost" type="button" aria-label="关闭">✕</button>
    </div>
    <div class="modal-body" id="config-body"></div>
  </div>
</div>
```

**config.js**（最小侵入，渲染逻辑零改动）：
- `renderConfigPage()` 改名 `openConfigModal()`：不再碰 `eventsViewport`；改为
  1. `$("config-modal").hidden = false`；
  2. 在 `#config-body` 内建原 `#config-page` 结构（去掉 `cfg-header` 的 `<h2>`，标题已在 modal-head；`#cfg-status` 移至 modal-head，原 header 位不再渲染）；
  3. `loadConfig()` 不变。
- `renderAll` 容器选择器 `#config-page` → `#config-body`（ID 挂到 body 内的页面 div 上，或直接用 body，二选一实施时取改动最小者）。
- 新增 `closeConfigModal()`：`hidden = true` + 清空 `#config-body`；绑定 `#cfg-close` 点击、`.modal` 背板点击（`e.target === modal`）、Esc 键。
- 删除 `import { eventsViewport } from "./views.js"`。
- 未保存修改关闭即丢弃——与现状（切会话即丢）语义一致，不加 dirty 守卫。

**app.js**：`$("config-btn")` 监听器 `renderConfigPage()` → `openConfigModal()`，import 同步；保留 `document.body.classList.remove("nav-open")`。

**app.css**：

```css
.cfg-modal-panel { width: min(90vw, 960px); max-height: 85vh; }
#config-page { padding: 0; max-width: none; margin: 0; /* 原页面居中约束作废 */ }
```

- 窄屏沿用现有 `.modal-panel { width: 100vw; max-height: 100dvh; border-radius: 0 }`（app.css:313），配置模态自动全屏，零新增规则。
- 收益：配置不再清空对话区，关闭即回到原对话，流式渲染不中断（修复 §1 问题 4）。

## §4 文件改动清单

| 文件 | 改动 |
|------|------|
| `static/index.html` | composer 移入新 `<footer>` + `#status-bar`；新增 `#config-modal` |
| `static/app.css` | `#app` 三行 grid；header 去 sticky；footer/status-bar 样式；to-bottom absolute；删 2 条 trajectory-open 补偿规则；cfg-modal-panel |
| `static/store.js` | `setVersion` 双写；新增 `setStatusModel` |
| `static/app.js` | config-btn 改开模态；2 处 `setStatusModel` 调用 |
| `static/config.js` | renderConfigPage→openConfigModal + 关闭逻辑；去 eventsViewport 依赖 |

零 Go 变更（embed 显式列表不含新增文件，无需动 `assets.go`）。

## §5 验收 DoD

1. 三行骨架：窗口任意缩放，header/footer 固定，仅 `#events`（及侧栏/轨迹各自）滚动；`document.scrollingElement` 无纵向滚动条。
2. 开轨迹面板，composer 不再变窄（通栏）；`#to-bottom` 始终贴在 main 右下角，不漂。
3. 状态栏显示版本与当前模型；切模型后状态栏同步；空值时占位不留白缝。
4. ⚙ 配置开模态：表单/JSON 双模式、providers 卡片、保存回填全部不回退；Esc/背板/✕ 三途径可关；关闭后对话区内容原样保留（含进行中的流式输出）。
5. 三断点（1280/768/375）走查：窄屏配置模态全屏；抽屉/轨迹下缘被 footer 覆盖无穿透。
6. verify-gate 全绿；`node --check` 过 5 个改动 JS。

## §6 提交计划（2 commits）

1. `web: three-row shell with fixed header and footer` —— §3.1–§3.3
2. `web: show config in modal instead of page` —— §3.4

（v1 经验：单 commit 内 hunk 纠缠风险低——两 commit 文件集仅 app.css/app.js 有少量交叠，交叠处为独立 hunk 可 `add -p` 拆分；若仍纠缠则合并并说明。）
每 commit 前给 diff 摘要待审阅（L0 #2）。
