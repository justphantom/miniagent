# WebUI 排版根因深度排查 与 全面优化方案（v3）

基线：`5dcd277`（HEAD）。部署机 `100.66.1.5:18787` 现网核对：
- 静态资源 `/app.css`、`/app.js` 与仓库 HEAD **逐字节一致**（curl 对比）→ 排除「线上旧版 / embed 未重编」。
- 版本号 `v6.4.0-31-g5dcd277` 即 HEAD → 服务端二进制与仓库同步。
- `Cache-Control: no-store`（web.go:194）→ 排除浏览器缓存。
- 结论：**页面症状 = 仓库代码直接渲染结果**，以下根因分析对部署页 100% 成立。

---

## §0 结论（TL;DR）

**根因唯一且确定**：`app.js:43` 的内联 `style.display="flex"` 以 CSS 级联最高优先级（内联样式 > 样式表任意选择器）覆盖了 `app.css:37` 的 `display:grid`。`#app` 的「三行 grid 骨架」退化为 flex **横行**（`flex-direction` 未设置，默认 `row`），header / `#layout` / footer 三个子块横向并排——footer（composer + 状态栏）被排到最右成为竖列，header 被挤成内容宽度窄条。截图所见即此，无第二个原因。

修复手段也在同一句里：**显示切换改 class 驱动，JS 永不再写 `#app` 内联 display**（+ 窄屏、配置模态、状态栏等 6 个顺带发现的一并修复，见 §6）。

---

## §1 截图症状 ⇄ 代码映射

| 截图所见 | 代码位置 | 说明 |
|---|---|---|
| 输入框 + 发送 +「工作目录·模型」+ 版本/模型 呈**右侧竖列** | footer = `#app` grid 行3（index.html:44-64）；flex-row 下 footer 成为最右块 | **根因直接症状**。v2 设计（WEBUI_LAYOUT_V2.md §2）footer 应通栏贴底 |
| 左上顶部一行：☰ miniagent ≡ ◐ 退出 | header = 行1（index.html:22-31）；flex-row 下成为最左窄条，`.flex-1` spacer 被压没 | 同根因。header 本应横贯整页顶 |
| 会话列表在左侧、`#session-id` 截断 | `#layout` 内部仍是 grid 两列 `240px 1fr`（app.css:46），所以 nav/main 关系没坏 | 根因不破坏栅格内部，只破坏三行骨架 |
| 主区 780px 居中、空态提示 | `#events > .view`（app.css:53）、`.ev.empty-state`（app.css:103） | 正常 |
| 会话副标题「…Now the backe…」边缘截断 | `.sid`/`.preview` ellipsis（app.css:70,78） | **正常行为**，非缺陷；缺 tooltip 属可用性（P1-3） |

## §2 根因证据链（不再依赖猜测）

1. `app.css:37`：`#app { display: none; grid-template-rows: var(--header-top) minmax(0,1fr) auto; }` —— 意图的骨架：行1 header / 行2 `#layout` / 行3 footer(auto)。
2. `app.js:41-43`：`showLogin()` 写 `$("app").style.display="none"`；`showApp()` 写 `$("app").style.display="flex"`。
3. CSS 级联规则：内联 `style` 优先级高于一切样式表规则 → `display:flex` 生效，`grid-template-rows` 被彻底架空（grid 属性在非 grid 容器上毫无作用）。
4. `#app` **没有** `flex-direction`（v1 时代是 `flex-direction:column`，v2 改 grid 时删掉了）→ 回落默认 `row`。
5. 三子块横排；`#layout` 恰好带 `flex:1`（app.css:47，grid 中是死代码，此处"意外复活"）→ 独吞剩余宽度，header/footer 保持内容宽。
6. 现网核对（见头）：部署页 = HEAD 代码 → 第 1-5 步对线上即"症状幂等"。

> 排他性：已排除 缓存（no-store）、embed 旧版（字节一致）、浏览器插件/缩放。`#app` 之外无任何元素产生该症状。

## §3 事故链复盘（为什么"本应三行"变"三列"）

1. **v1**（84a303a 前置实现）：`#app` 是 `flex column`，JS 内联 `display:flex` —— 两者匹配，正常。
2. **v2**（5dcd277）：`app.css` 把 `#app` 改为 `display:none + grid-template-rows` 三行骨架；同时删掉了 `flex-direction` 兜底。
3. ** JS 未同步**：`showApp()` 仍写 v1 的内联 `display:flex`。改结构改了 CSS，没改开关逻辑——内联样式直接覆写新骨架。
4. 流程教训：v2 验收（WEBUI_LAYOUT_V2.md §5「三行骨架」DoD）只人工走查，未核对 JS 是否残留内联 display。**结构骨架与显示开关必须同 PR 同查**。

## §4 全面问题清单（P0/P1/P2/P3）

> 审计（WEBUI_LAYOUT_AUDIT.md）已列 A/B/C/D/E/F，**一项都未修**；本次新增 3 项（P1-3、P2-3、伪问题排除）。

### P0 —— 主结构（页面万恶之源）
| # | 问题 | 证据 | 状态 |
|---|---|---|---|
| P0-1 | `#app` 内联 flex 覆盖 grid，三行骨架横行坍塌，footer/状态栏成右列 | app.js:41-43 vs app.css:37 | 未修（审计 A） |
| P0-2 | 窄屏 <640px `#layout{display:flex}` 下 main 无 `flex:1`，主区宽度收缩不可靠 | app.css:306-311 | 未修（审计 B） |

### P1 —— 功能/可用性缺陷
| # | 问题 | 证据 | 状态 |
|---|---|---|---|
| P1-1 | `#config-page` 同选择器双规则，203 被 229 覆盖 → 模态内表单仍 720px 居中留白（`#config-page` 重置作废） | app.css:203 vs 229 | 未修（审计 C） |
| P1-2 | 状态栏模型首用（无 localStorage）恒空：`if (saved) setStatusModel(saved)` 无兜底 | app.js:439 | 未修（审计 D） |
| P1-3 | 会话条目 ✕ 绝对定位 `top:2px; right:2px` 32×32，长模型名会被盖住；sid/preview 截断无 tooltip | app.css:71-73 | **新增** |

### P2 —— 死代码/样式卫生
| # | 问题 | 证据 | 状态 |
|---|---|---|---|
| P2-1 | `#layout { flex:1 }`：grid 行内无效死代码；P0-1 事故中还成为 header/footer 收窄帮凶 | app.css:47 | 未修（审计 E） |
| P2-2 | `#login, #app { display:none }` 重复声明（#app 已在 37 行声明） | app.css:135 | 未修（审计 F） |
| P2-3 | 状态栏版本/模型对齐未定义（无 justify-content），右列事故中观感随机 | app.css:56-58 | **新增**（建议空间两端对齐） |

### P3 —— 部署（非代码，流程固化项）
- embed 静态资源编译进二进制（assets.go:12）：只改 static/ 不 `make build` 重编则线上恒旧。本次已排除，但要在部署流程写死「重编→替换→重启→curl 核对」。

---

## §5 目标布局（复述 v2 预期，作为修复回归基线）

```
┌──────────────────────────────────────────────────────────────┐
│ header（grid 行1）☰ miniagent 会话ID·token    ≡ ◐ 退出         │
├──────────┬────────────────────────────────┬───────────────────┤
│ 侧栏 240  │ #events（唯一滚动区，780px 居中） │ 轨迹面板(可选)     │
│ 自滚动    │                                 │                   │
├──────────┴────────────────────────────────┴───────────────────┤
│ composer（grid 行3，通栏）：输入任务…              [发送]        │
│ ▸ 工作目录 · 模型（折叠）  [📁目录] [模型▾]                     │
│ 状态栏：v6.4.0 …（左）｜当前模型（右）                           │
└──────────────────────────────────────────────────────────────┘
```

## §6 全面优化方案（按实施顺序）

### 修复 1 —— P0-1：显示切换改 class 驱动，消灭内联 display

**app.css**，删 `#app` 内 grid 声明并改开态类：

```css
#app { display: none; }
#app.on { display: grid; grid-template-rows: var(--header-top) minmax(0, 1fr) auto; height: 100dvh; }
#login { display: none; }        /* 替代被删的 app.css:135 */
#login.on { display: flex; }     /* 复用 #login 133-134 的 flex 属性 */
```

**app.js:41-43**：

```js
function showLogin() { $("login").classList.add("on"); $("app").classList.remove("on"); $("key-input").focus(); }
function showApp() {
  $("login").classList.remove("on");
  $("app").classList.add("on");
  ...
```

评估（为何选 class 方案）：
- 备选 B（CSS 保留 `.grid` 常显 + JS 只切 hidden）也能用，但 class 方案与现有 `body.nav-open/nav-collapsed` 习惯一致，且**根除"内联 display 覆写"这一类事故**——本次教训就是内联样式捅的娄子。
- 备选 C（退回 v1 flex column + 补 `flex-direction:column`）是另一条正确路，但 v2 三行 grid 已验收，不倒退。

### 修复 2 —— P0-2：窄屏 main 撑满

```css
@media (max-width: 639px) {
  #layout { display: flex; }
  main { flex: 1; min-width: 0; min-height: 0; }   /* 新增 */
  ...
```

### 修复 3 —— P1-1：`#config-page` 规则归并

删 app.css:203 的重复重置；改 app.css:229 为模态语境：

```css
#config-page { padding: 0; max-width: none; margin: 0; }
```

（原 720px 居中/16px padding 是整页时代约束，模态内由 `.modal-body` padding 承担。）

### 修复 4 —— P1-2：状态栏模型兜底

app.js `loadModels` 末尾（替换 app.js:439）：

```js
setStatusModel(saved || "默认模型");
```

### 修复 5 —— P1-3：会话条目 ✕ 避让 + tooltip

- app.css `.sess-item`: 首行文本（模型名/标题行）加 `padding-right: 34px` 避开右上 ✕。
- app.js `loadSessions` 渲染 sid/preview 时 `setAttribute("title", text)`（原值）。

### 修复 6 —— P2-1/2/3：死代码清理 + 状态栏打磨

- app.css:47 删 `#layout` 的 `flex: 1`。
- app.css:135 删 `#login, #app { display:none }`（并入修复 1）。
- app.css:58 `#status-bar` 加 `justify-content: space-between; flex-wrap: wrap;`（版本左、模型右，窄屏可换行不重叠）。

## §7 验收 DoD

1. 登录后 `getComputedStyle($("app")).display === "grid"`（DevTools 执行）。
2. 三行骨架：header 顶、footer 通栏贴底（composer+状态栏横贯），缩放窗口仅 `#events`/侧栏/轨迹各自滚动，`document.scrollingElement` 无滚动条。
3. 三断点走查（1280/768/375）：375 主区撑满宽度无横向溢出/收缩；配置模态窄屏全屏。
4. 配置模态内表单横向铺满 modal-body（无 720px 居中留白）。
5. 清空 localStorage 首次进入：状态栏显示版本 +「默认模型」。
6. 会话条目：长模型名不被 ✕ 遮挡；悬停 sid/preview 显示完整 title。
7. `node --check` 过改动 JS；verify-gate 全绿（`gofmt -s -l .` 空 / `go build ./...` / `go vet ./...` / `go test -race ./...` / `golangci-lint run ./...`）。
8. 部署：`make build` 重编 → 替换二进制 → 重启 → `curl <host>/app.css | grep app.on` 命中即新版。

## §8 提交计划（一次一事，3 commits）

1. `web: fix app-shell display regression, restore three-row grid` —— 修复 1 + 6（P0-1/P2-1/P2-2/P2-3）
2. `web: config modal full-width, status-model fallback` —— 修复 3 + 4（P1-1/P1-2）
3. `web: sidebar item delete-overlap and tooltips` —— 修复 5（P1-3）

每 commit 前给 diff 摘要待审阅（L0 #2）。验证 §7.8 的现场核对同样适用于 commit 1 完成后。

## 附：已排除的"伪原因"

- 浏览器缓存 → `no-store` 实测缺失无效缓存（web.go:194）。
- 线上旧版静态资源 → curl 逐字节一致（见头部）。
- 调试态/前端 DPI 缩放 → 症状由代码推演逐项还原，与像素无关。
- 会话副标题「截断」→ 是 CSS ellipsis 的正常行为（.sid/.preview），仅缺 tooltip（P1-3）。