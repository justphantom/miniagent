# WebUI 对话列表横向滚动条消除方案

基线：`0548ae7`（工作树）。范围：会话列表面板（`#session-panel` slide panel）。方法：headless Chromium 151 实测复现 + 逐方案验证，非推测。

## §0 硬约束

- 只消除"不应出现的"横向滚动条：`#status-bar`（app.css:78 `overflow-x:auto`）与 `.md pre/table`（内容横滚）是**有意设计**，不动。
- 不引 JS 测试框架/依赖；验收用 headless chromium 脚本（§5）。
- 与 `WEBUI_SCROLLBAR.md`（滚动条**样式**统一）正交：本方案消除**溢出源**，样式规则保留。

## §1 根因分析（实测证据）

复现环境：真实 app.css + 镜像 app.js `loadSessions()` DOM 构造，面板宽 279px（280-1px border）。
测试脚本：`/tmp/hscroll-test/`（server2.js + test2.html，本机临时产物，不入库）。

### 1.1 根因链（三层）

**RC1 主因：`.sess-item` 按钮在 `.tree-group` 内保持 `display:inline-block`（UA 默认），shrink-to-fit 宽度被 nowrap 的 `.sid` min-content 撑爆。**

- 引入于 `4d35283`「project-tree session grouping」：`.sess-item` 从 `#session-list`（flex column）直接子项（flex blockify → 全宽）变为 `.tree-group`（`display:block`，app.css:95）子项 → 按钮回落 inline-block。
- shrink-to-fit = clamp(可用宽, min-content, max-content)；`.sid`（app.css:132 `white-space:nowrap`）的 min-content = 整行文本宽。真实会话 sid = `36字符id · workdir · 时间戳`（app.js:502）恒 >255px → **每条真实会话都触发**。
- 实测（baseline-real）：`disp=inline-block, itemW=559, panelSW=569, panelCW=279, 横向溢出=290px`。
- 附带症状：短会话条目宽度参差（30 条短 id 场景 itemW=76px，右边缘不齐）——同一根因。

**RC2 放大器：`.panel-body` 只声明 `overflow-y:auto`（app.css:92），`overflow-x` 按规范从 `visible` 计算为 `auto`** → 横向溢出 ≥1px 即渲染滚动条，而非裁剪。computed 实测 `overflowX=auto`。

**RC3 潜在：`.sess-item` 首行 `top` div（model/id，app.js:497-499）无 class、无 ellipsis/overflow 规则** → 不可断行模型名（实测 66 连续字符）在修复 RC1 后仍溢出 258px，经 RC2 成滚动条。

### 1.2 已排除的非因素（实测）

| 候选 | 结论 | 证据 |
|---|---|---|
| hover `translateX(2px)`（app.css:136） | 无影响 | 被 `.panel-body` 8px padding 吸收：translate 后 panelSW=279 不变 |
| 竖向滚动条占位（8px classic） | 无影响 | 20 条会话强制出竖条后 hOver=0（clientW 269 内容随之收缩）|
| 搜索框 `width:100%` | 无溢出 | scenario focus：hOver=0（`*{box-sizing:border-box}` 兜住 border/padding）|
| `.tree-group-title`/`.sid`/`.preview` | 已有 ellipsis | app.css:96-97,132,140 |
| focus outline（offset 1px） | 不参与 scrollable overflow | scenario 9：hOver=0 |

## §2 修复设计

三层根因各对应一条最小改动（A3 处共 ~4 行）：

| # | 改动 | 针对 | 实测效果 |
|---|---|---|---|
| F1 | `.tree-group { display: flex; flex-direction: column; }` | RC1 | itemW 559→261，hOver 290→0；条目恢复全宽对齐 |
| F2 | app.js `top.className = "sess-title"` + CSS `.sess-item .sess-title { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }` | RC3 | 66 字符不可断行模型名省略号截断，hOver 258→0 |
| F3 | `.panel-body { overflow-x: clip; }` | RC2 兜底 | 未来任何新溢出源被裁剪而非出滚动条 |

组合实测（FULL-STACK-real）：`itemW=261, panelSW=279, panelCW=279, hOver=0`。

### 2.1 方案选型（F1 的等价替代）

| 方案 | 改动 | 评估 |
|---|---|---|
| **A. `.tree-group` 变 flex column（采纳）** | 1 行 | 分组容器语义即列；组内未来任意子元素一并拉伸全宽 |
| B. `.sess-item { display: block }` | 1 行 | 等价，直接钉按钮；与 A 二选一，同时上属冗余 |
| C. 仅 `overflow-x: clip` | 1 行 | 否决单独使用：滚动条没了但内容被裁、条目参差仍在（治标）|

### 2.2 `clip` 与 `hidden` 说明

- CSS Overflow L3：`clip` 轴与 scrollable 轴（`overflow-y:auto`）组合时，clip 计算值为 hidden（Chromium 151 实测 computed=`hidden`）。两者渲染结果一致：**无横向滚动条、内容裁剪**；差异仅 clip 不产生可编程滚动容器（hidden 可被 JS scrollLeft 拖动）。
- 写 `clip`（语义正确；项目基线已用 `100dvh`/`color-mix`，浏览器面足够新）。

## §3 文件改动清单

| 文件 | 改动 |
|---|---|
| `cmd/miniagent/webstatic/static/app.css` | `.tree-group` 规则加 2 声明；新增 `.sess-item .sess-title` 1 条规则；`.panel-body` 加 `overflow-x: clip` |
| `cmd/miniagent/webstatic/static/app.js` | loadSessions 内 1 行：`top` div 加 `className`（app.js:497 附近） |

Go 零改动（embed 自动生效，verify-gate 覆盖编译）。

## §4 验收 DoD

1. 真实会话（36 字符 id + workdir + 时间戳）面板无横向滚动条；条目右边缘齐整（修参差）。
2. 66 字符不可断行模型名首行省略号截断，无横向滚动条。
3. ≥20 条会话出竖向滚动条时仍无横向滚动条。
4. hover 位移 / 搜索 focus / running 圆点 / 删除按钮 / 搜索过滤（panel.js 依赖 `style.display`，不受影响）行为不变。
5. 窄屏（<640px 面板全宽）同样无横向滚动条。
6. verify-gate 全绿。

## §5 验收方法（手工，不入库）

headless chromium 加载 `server2.js`（静态服务 app.css + 镜像 DOM 的 test 页），断言 `panelBody.scrollWidth <= panelBody.clientWidth`；场景：真实 sid / 不可断行 model / 20+ 条 / 窄屏。命令模板见本文档 §1.1 测试脚本。

## §6 提交计划（1 commit）

`web: fix session list horizontal overflow and ragged item widths`

## §7 明确不做

- 不给 `#session-list`/`.tree-group` 加 `min-width:0`（非 flex 主轴溢出路径，实测不需要）。
- 不动 `#status-bar` 与 `.md` 内容区的有意横滚。
- 不移除 hover `translateX(2px)`（实测无害，保留交互）。
- 不新增 JS 测试框架/CI 浏览器测试（标准库优先，不引依赖）。
