# WebUI 三项特性实现方案：工作目录选择器 / Token 用量可视化 / 工具轨迹视图

> 对应 `WEBUI_NEXT.md` 路线图的 P0（工作目录选择器、工具轨迹视图）+ P1（Token 用量可视化）。
> 本文档为详细实现方案（已定稿）：共享后端事件增强、三个特性 API/前端设计、UI 美化、**响应式适配**、安全边界、测试计划。
> 决策状态：原「决策清单」已逐项定案（见 §九）。

---

## 〇、共享支点：NDJSON 事件模型增强

三个特性都受限于当前事件模型。分析现有事件字段（`miniagent/event/event.go`）：

| 事件 | 现有字段 | 缺失 |
|------|---------|------|
| `text_delta` / `reasoning_delta` | `step`, `text`, `ts` | — |
| `tool_use` | `name`, `call_id`, `input`, `ts` | **`step`**（无法归属到步骤） |
| `tool_result` | `name`, `call_id`, `output`, `truncated`, `is_error`, `exit_code`, `ts` | **`step`** |
| `result` | `text`, `model`, `input_tokens`, `output_tokens`, `steps`, `llm_requests`, ... | 无 per-step 用量 |
| — | — | **无 per-step 用量事件** |

### 增强 A：`tool_use` / `tool_result` 增加 `step` 字段

- `emit.go` 的 `OnToolUse`/`OnToolResult` 钩子当前签名是 `(name, callID, input)` / `(name, callID, r)`，无 step。
- **定案**：`miniagent/loop_api.go` 的 `OnToolUse`/`OnToolResult` 钩子签名增加 `step int` 参数（核心接口改动，同步更新 `buildHooks`、测试、`looptest`）。`handleToolCalls` 调用处已有 step，透传即可。
- 序列化：`toolUseEvent`/`toolResultEvent` 增加 `Step int \`json:"step"\``。
- **兼容**：全部消费方（CLI stdout 消费者、前端、session 解析）只读新字段，无破坏；旧会话 replay 无 step 字段时前端降级（见 §三）。

### 增强 B：新增 `step_usage` 事件（per-step 用量）

- **关键现成钩子**：`miniagent/loop_api.go` 的 `OnStep(ctx, StepSnapshot)`，`StepSnapshot` 已含 `Step`/`InputTokens`（累计）/`OutputTokens`（累计）/`LLMRequests`。
- 但 StepSnapshot 是**累计值**，需要差值算出本步增量。`recordStepUsage`（`loop_extra.go:178`）在每步后已把本步 usage 累进 `total`。
- **定案**：`LoopHooks` 新增可选字段 `OnStepUsage func(step, in, out, toolCalls int)`；`recordStepUsage` 末尾调用（增量直接可得，无需差值）。web 层 `assembleHooks` 装配为向 NDJSON 流写：
  ```
  {"type":"step_usage","step":1,"input_tokens":11400,"output_tokens":333,"tool_calls":1}
  ```
  `tool_calls` 为本步工具数（`len(resp.ToolCalls)`）。
- CLI 路径不装配（`OnStepUsage` 为 nil 时零开销）。
- 该事件同时服务：用量可视化（per-step 数据）+ 轨迹视图（步骤边界的时间戳/用量锚点）。

---

## 一、特性 1：工作目录选择器

### 现状
- `#workdir` 是纯文本输入（`app.js send()` 读取 + 服务端绝对路径校验）。
- 每视图 `v.workdir`；localStorage `saveWorkdir` 记忆最近一次。
- 无目录浏览能力，输入长路径易错。

### 目标
- 目录浏览选择 + 最近使用目录快捷选择 + 保留手动输入。
- 与现有多会话视图联动：切视图时 workdir 跟随该会话（现状已支持）。

### 后端新增 API：`GET /api/tree`

```
GET /api/tree?path=/home/work/Codes
→ 200 {"path":"/home/work/Codes","parent":"/home/work","dirs":[{"name":"miniagent","path":"/home/work/Codes/miniagent"},...],"total":N}
```

- `path` 必填绝对路径（复用 `filepath.IsAbs` + `Clean`）。
- 只列出**目录**（选择器只需要目录），跳过隐藏目录可选（前端 UI 开关）。
- **安全边界（定案：放开任意绝对路径，只读 + 加固）**：
  - 只读列举，无写入能力；
  - 不跟随符号链接（`fs.DirEntry.Type()&fs.ModeSymlink` 跳过）——与 `OpenNoFollow` 加固一致；
  - 每层上限 500 条（防超大目录拖垮响应），超出返回 `{"truncated":true}`；
  - `path` 不存在/不可读 → 404 `{"error":"..."}`。
- 路由登记：`web.go mux`: `api.HandleFunc("GET /api/tree", s.handleTree)`，`requireAuth` 覆盖（与 `/api/config` 同级）。
- 实现文件：新增 `cmd/miniagent/web_tree.go`（≤300 行约束，必要时拆）。

### 前端设计

**组件**（新文件 `dirpicker.js`）：
- workdir 输入行改造：输入框 + 📁 按钮 + 最近目录 `<datalist>`。
- 📁 打开模态目录树：当前路径 + 上级回退 + 子目录列表，点击目录进入，底部「选择此目录」「取消」。
- 最近目录（定案：localStorage）：`miniagent.web.recent_dirs`（去重、上限 10、最近在前），模态顶部快捷区。
- `send()` 仍取输入框值；选择器选中后回填输入框并 `saveWorkdir`。

**与视图联动**：模态选择器是全局的（选择后写当前活跃视图的 `v.workdir` 与输入框）。切视图时 `app.js openSession` 已有 `loadReplay` 回填逻辑，保持不变。

### 测试
- 后端：`web_tree_test.go`——绝对路径校验、目录列举、symlink 跳过、越界路径、截断上限、空目录。
- 前端：`node --check` + 手工验证（目录深链、最近目录去重、选择回填）。

---

## 二、特性 2：Token 用量可视化

### 现状
- `view.tokens = {in, out}` 逐 result 累加（`events.js:182-183`）。
- header `in=N out=M`（`store.js showSessionID`）。
- result 卡片内一行文本 `steps=N in=N out=N [compacted] [elapsed]`（`events.js:194`）。
- 无任何图形化、无 per-step、无预算对比。

### 目标
- 每轮 result 卡片内：输入/输出 Token 堆叠条形图 + steps + 耗时。
- 每视图「用量面板」（可折叠）：per-step 条形图（in/out 分色）+ 各步工具调用数。
- 预算条：`run.max_tokens_total` 已配置时显示累计消耗/预算占比。

### 前端设计

**数据模型**（`views.js`）：
```js
view.usage = {
  steps: [],            // [{step, in, out, toolCalls, ts}]
  totals: {in, out},    // 当前返回的 view.tokens 保持
  budget: 0,            // max_tokens_total，0=无预算
};
```
- `view.usage.budget` 初始化（定案：boot 时经 `GET /api/config` 读 `run.max_tokens_total`，缓存于 store；配置页保存后一并刷新）：无则 0=隐藏预算条。

**渲染**（新文件 `usage.js`）：
- `renderUsageBar(wrapper, {in, out, budget})`：flex 双色条（input=蓝 `--usage-in`、output=绿 `--usage-out`），tooltip `in=N out=N`；有预算时叠加红色预算标线 + 百分比文本。
- `renderStepUsage(container, steps)`：每步一行迷你条 + `step N · in=a out=b · tools=k`，折叠在 `<details>` 内。
- 钩入 `events.js`：
  - `case "result"`：除现有 usage 文本外，追加用量条形图；
  - `case "step_usage"`：`view.usage.steps.push(ev)`，若用量面板已展开则增量重绘（step_usage 每步一次直接重绘，成本低，不节流）。
- header：`in=N out=M` 保持文本；有预算时改为 `in=N out=M / BUDGET`，右侧加预算占比迷你条。

**展示入口（定案：双入口）**：每 result 卡片内折叠「用量详情」；视图 header 的 token 徽标改为可点击，弹出该视图用量面板（`<dialog>`，复用 §四 模态组件规范）。

### 后端（见共享支点 增强 B）
- `step_usage` 事件由 `recordStepUsage` 装配输出。
- `result` 事件已含累计 `steps`/`llm_requests`，无需改。

### 测试
- 后端：`web_turn_test.go` 断言流中出现 `step_usage` 事件且 step 递增、数值与 result 累计一致（`sum(step_usage.in) == result.input_tokens`）。
- 前端 `node --check`。

---

## 三、特性 3：工具轨迹视图

### 现状
- 每个 `tool_use` → `<details class="ev tool">`：summary `🔧 name` + input `<pre>`；`tool_result` 按 `call_id` 配对追加 output `<pre>`（`events.js appendToolUse/appendToolResult`）。
- 平铺在消息流中，无步骤分组/序号，无整体时间线，不能跨步骤跳转。

### 目标
- 会话内「轨迹」面板：按 step 分组展示每次 LLM 调用与其工具执行（输入/输出/耗时/用量），点击可定位到消息流中的对应节点。
- 与正常工作流并列，不破坏现有消息列表。

### 前端设计

**数据模型**（`views.js`）：
```js
view.trajectory = {
  steps: new Map(),     // step → {step, ts, in, out, tools: [{name, callID, input, output, isError, ts}]}
  order: [],            // step 顺序
};
```

**事件捕获**（`events.js`）：
- `case "tool_use"`：`step = ev.step ?? 当前步`；`trajectory.steps.get(step).tools.push({...ev})`；同时保留现有消息流渲染。
- `case "tool_result"`：按 `call_id` 在轨迹中补 output。
- `case "step_usage"`：建立/更新步骤用量 `{step, in, out}`——同时提供步骤时间戳锚点。
- `case "text_delta"/"reasoning_delta"`：`view.curStep = ev.step`（供降级归属）。
- **降级**（旧会话 replay / CLI 缺 step）：`ev.step` 缺失时归属 `view.curStep`；replay 起始 `curStep=0`，首个 tool_use 归步 0。

**渲染**（新文件 `trajectory.js`）：
- 视图 header 加「轨迹」按钮 → 右侧抽屉面板。
- 面板内容：按 `order` 渲染步骤卡片：
  - 卡片头：`Step N` + 用量迷你条（复用 usage.js）+ 工具数 + 时间；
  - 卡片体：每个工具一行（`name` + 折叠 input/output pre，复用 copy 按钮逻辑）；
  - 点击卡片 → 滚动定位到消息流中该 step 的首个节点（节点上加 `data-step` 属性，`scrollIntoView` + 高亮脉冲）。
- 折叠状态：面板整体 `<details>`；每步卡片默认折叠，仅显示摘要。

**与消息流的关系（定案）**：轨迹面板是**镜像视图**，不改动 `appendToolUse`/`appendToolResult` 的现有 DOM 结构（避免回归）。

### 测试
- 后端：`tool_use`/`tool_result` 事件带 `step` 断言（`web_turn_test.go`）。
- 前端 `node --check` + 断点定位（构造含步骤跳转的流）。

---

## 四、WebUI 美化设计（新增）

> 现状：GitHub-Dark 风格双主题（`app.css` 顶部 `:root` 变量），纯语义、无设计系统。  
> 原则：**加变量不加魔法值**、不引入构建链 / 第三方 UI 库（与「纯 stdlib + go:embed」约束一致）、CSP `style-src 'self'` 禁内联（保持 N1 的 CSSOM 做法）。

### 4.1 Token（设计令牌）

扩充 `:root` 变量，形成语义化设计令牌（双主题各一套）：

```css
:root {
  /* 现有保留 */
  --bg: #0d1117; --panel: #161b22; --border: #30363d;
  --fg: #e6edf3; --muted: #8b949e;
  --accent: #58a6ff; --err: #f85149; --ok: #3fb950;
  /* 新增：层级背景（会话卡片、抽屉、模态分层） */
  --bg-elev-1: #1c2128; --bg-elev-2: #21262d;
  /* 新增：语义色扩展 */
  --info: #58a6ff; --warn: #d29922; --usage-in: #58a6ff; --usage-out: #3fb950;
  /* 新增：布局度量 */
  --radius-sm: 6px; --radius-md: 8px; --radius-lg: 12px;
  --space-xs: 4px; --space-sm: 8px; --space-md: 12px; --space-lg: 16px;
  --font-mono: ui-monospace, SFMono-Regular, "JetBrains Mono", monospace;
  --shadow-modal: 0 8px 24px rgba(0,0,0,.4);
  --transition-fast: 120ms ease;
  --transition-med: 200ms ease;
}
:root[data-theme="light"] { /* 同键位换值 */ }
```

### 4.2 布局与导航

- **header**：加高至 `52px`，`backdrop-filter: blur(8px)` + 半透明背景（`color-mix(in srgb, var(--bg) 85%, transparent)`），滚动时内容从下穿过产生毛玻璃层次；元素间距统一 `--space-md`。
- **侧栏（#nav）**：
  - 顶部「＋ 新会话 / ⚙ 配置」按钮组改为 `display:grid; grid-template-columns: 1fr auto`（新会话占满、配置窄按钮）；
  - 会话项 hover/active 态圆角 `--radius-md` + `translateX(2px)` 微位移；
  - 会话预览行字号统一 12px `--muted`，running 呼吸点保留（换用 `--ok` 语义）。
- **间距网格**：`#events` 内边距、卡片间距、composer 间距全部改用 `--space-*` 变量，消除魔法值。

### 4.3 消息卡片（.ev）

- 统一圆角 `--radius-md`（8px），边框 `1px var(--border)`；
- **用户消息**：右侧对齐卡片（`margin-left:auto; max-width:720px`）+ 顶部 accent 竖条（`border-left:3px var(--accent)`），与 assistant（左对齐无竖条）形成视觉对比；
- **assistant/result**：保持左侧，宽度 `max-width:900px`；
- **工具卡片（.ev.tool）**：summary 加 emoji 图标色（`🔧` 改为 CSS 前缀色点 + 工具名），input 区域 `--font-mono`、`font-size:12.5px`，output 区保留 `.out.err` 红边逻辑；
- **时间戳**：绝对定位右上（tag 行），`font-variant-numeric: tabular-nums` 防跳动；
- **折叠态**：`max-height` 渐变遮罩（现有）保留，新增「展开」按钮圆角 `--radius-sm`、hover 变色。

### 4.4 用量可视化样式（本轮新组件）

```css
.usage-bar { display:flex; height:8px; border-radius:4px; overflow:hidden; background:var(--bg-elev-1); }
.usage-bar .in  { background:var(--usage-in); }   /* 蓝 */
.usage-bar .out { background:var(--usage-out); }  /* 绿 */
.usage-bar .budget-mark { width:2px; background:var(--err); position:relative; } /* 预算标线 */
```
- 用量面板表格行 hover 高亮；step 条迷你 bar 高度 6px。

### 4.5 轨迹抽屉（本轮新组件）

- 抽屉：`position:fixed; right:0; top:52px; bottom:0; width:360px; background:var(--bg-elev-1); border-left:1px var(--border); transform:translateX(100%) → 0; transition:var(--transition-med)`；
- 遮罩复用现有 `#overlay` 模式（`z-index` 高于 header）；
- 步骤卡片：折叠态圆角 `--radius-md`；展开时 `border-color:var(--accent)`；
- 定位高亮：目标节点 `outline:2px var(--accent); animation: pulseGlow 1s`（`@keyframes pulseGlow { 0%,100%{outline-color:transparent} 50%{outline-color:var(--accent)} }`）。

### 4.6 模态与目录选择器样式

- 统一模态组件规范（目录选择器、用量弹窗共用）：居中 `position:fixed; inset:0; display:grid; place-items:center`，面板 `--bg-elev-2` + `--shadow-modal` + 圆角 `--radius-lg`，`max-width:560px; max-height:70vh; overflow:auto`；
- 目录行：`padding:8px 12px; border-radius:var(--radius-sm)`，hover 背景 `--bg-elev-2`；父目录行显示 `↰` 前缀；
- 按钮主次分阶：主按钮实心 `--accent` 底白字，次按钮 `ghost`（现有）。

### 4.7 动效
- 统一 `transition` 仅作用于交互态（hover/active/open），不用入场动画（避免 CSP 内联 style 争议 + 性能）；
- 唯一例外：running 呼吸点（已有）+ 轨迹定位 `pulseGlow`。

### 4.8 无障碍
- 所有图标按钮带 `aria-label`（目录选择器📁按钮、轨迹开关、用量弹窗）；
- 抽屉/模态获得焦点时 `Escape` 关闭（复用 `app.js` confirmInline 的 keydown 模式）；
- 颜色对比：`--muted` 在暗色主题下 ≥ 4.5:1（#8b949e on #0d1117 达标，保持）。

### 4.9 CSP 合规注意
- 全部新样式进 `app.css`（`style-src 'self'` 允许）；**禁止** `element.style.xxx` 内联（N1 教训）——轨迹抽屉开关用 classList、用量 bar 用 `--usage-*` 变量；
- 图标/emoji：用 Unicode（现有）或 CSS 图形，不引入 iconfont/外部字体（离线 + CSP 双约束）。

### 4.10 美化实施范围

| 项 | 改动文件 | 随哪个特性落地 |
|----|---------|--------------|
| Token 变量 + 间距网格 | `app.css` | 公共（第 1 步先落） |
| header 毛玻璃 + 高度 | `app.css` + `index.html` | 公共 |
| 消息卡片左右对齐 + 工具卡样式 | `app.css` + `events.js`（class 调整） | 特性 3 |
| 用量条/面板样式 | `app.css` + `usage.js` | 特性 2 |
| 轨迹抽屉样式 | `app.css` + `trajectory.js` | 特性 3 |
| 模态组件规范 | `app.css` + `dirpicker.js` | 特性 1 |
| 无障碍属性补全 | `events.js`/`app.js`/`index.html` | 各特性随行 |

---

## 五、响应式布局适配（参考 DSH 排版）

> 参考 DSH 前端布局策略：CSS Grid 三栏主布局 + `position:fixed` 抽屉窄屏适配 + CSS 设计令牌系统，不依赖大量 `@media` 断点。  
> 现状：miniagent 已有基础的 sidebar drawer 模式（`@media min-width:800px` 换列 + `nav-open` 类控制抽屉），但布局平面化、无 Grid 骨架、无窄屏的 composer/消息卡片/header 专项适配。

### 5.1 布局架构（三态）

| 屏幕宽度 | 布局 | 侧栏 | 轨迹面板 | 说明 |
|---------|------|------|---------|------|
| ≥ 800px | **三栏 Grid** | 固定 240px 侧栏 | 可选 360px 右侧抽屉 | 桌面/平板横屏 |
| 640–799px | **双栏** | 固定 240px 侧栏 | 叠加遮罩抽屉 | 平板竖屏 |
| < 640px | **单栏全屏** | 遮罩抽屉 | 遮罩抽屉 | 手机 |

**实现**：`#layout` 从 `display:flex` 改为 `display:grid`：

```css
#layout {
  display: grid;
  grid-template-columns: 240px 1fr;                 /* 侧栏 + 主区 */
  grid-template-rows: 1fr;
  grid-template-areas: "nav main";
}
/* 轨迹面板打开时（三栏）*/
#layout.trajectory-open {
  grid-template-columns: 240px 1fr 360px;
  grid-template-areas: "nav main trajectory";
}
#trajectory-panel { grid-area: trajectory; }
/* 窄屏 < 640px：回退到 flex drawer（现有 nav-open 模式）*/
@media (max-width: 639px) {
  #layout { display: flex; }
}
```

### 5.2 侧栏 drawer（现有改进）

- 现有 `#nav { display:none }` + `body.nav-open #nav { display:flex }` 模式保留，但：
  - 桌面（≥800px）**始终显示**，`#nav { display:grid }`，不再隐藏（当前 `#nav { display:none }` + `@media { #nav { display:flex } }` 有两处 `display` 声明，合并为 `#nav { display:grid; grid-template-rows: auto 1fr; }`，窄屏时 `display:none` + `nav-open` 覆盖）
  - 窄屏遮罩：`#overlay` 现有模式保留
  - 按钮组：`#new-chat` + `#config-btn` 用 `grid-template-columns: 1fr auto` 并排

### 5.3 消息区（#events）

- 当前：`padding: 16px`，单列流式，`max-width` 约束
- 桌面：`padding: 16px 24px`，`.ev` 卡片 `max-width: 900px; margin: 0 auto`（居中，非左对齐）
- 窄屏（<640px）：`padding: 10px`，`.ev` 卡片 `max-width: 100%; border-radius: 6px`（减小圆角省空间），`border-left` 竖条更窄（`2px`）
- 用户消息右对齐：`margin-left: auto` + `max-width: 80%`（桌面可到 720px，窄屏 `max-width: 90%`）

### 5.4 Composer（输入区）

- 当前：`#composer` 是固定底部 `display:flex;flex-direction:column`，`#prompt` 无最大宽度
- 桌面：`.composer-row` 内 elements 居中 `max-width: 900px; margin: 0 auto;`，与消息区对齐
- 窄屏：`#prompt` 字号保持 `16px`（防 iOS 缩放），`#send` 按钮宽度 `auto`（别占满）
- 安全区（safe-area）：`padding-bottom: calc(12px + env(safe-area-inset-bottom, 0px))` 已有，保留

### 5.5 Header 响应式

| 元素 | 桌面（≥800px） | 平板（640-799px） | 窄屏（<640px） |
|------|---------------|------------------|---------------|
| #menu-btn（☰） | `display:none`（现有） | 显示 | 显示 |
| h1 标题 | 显示 | 显示 | 缩小至 `14px` 或只显示图标 |
| #session-id | 显示 | 显示 | 仅显示截断短 id（`text-overflow`） |
| #session-tokens | 显示 | 显示 | 仅显示 `in=N`（省略 out） |
| #version-badge | 隐藏（现有 `@media max-width:500px`） | 隐藏 | 隐藏 |
| #model-badge | 显示 | 显示 | 隐藏（保留头像缩略） |
| #theme-btn | 显示 | 显示 | 显示 |
| #logout | 显示 | 显示 | 文字「退出」→ 图标 ⏻ |

### 5.6 轨迹抽屉响应式

- 桌面（≥800px）：`position:fixed; right:0; top:52px; bottom:0; width:360px`，`transform:translateX(0)` 开/关
- 窄屏（<640px）：`width:100vw; z-index:10`（覆盖全屏，包含遮罩），`top:0`（覆盖 header）
- 关闭按钮：窄屏显示 ✕ 浮动按钮，桌面可使用点击遮罩或 Escape

### 5.7 模态框（目录选择器、用量弹窗）

- 通用规范（见 §4.6）：`place-items:center` 居中
- 窄屏：`max-width: 100vw; max-height: 100dvh; border-radius: 0`（全屏模态）
- 目录树：窄屏时每行减小 padding（`6px 8px`），字号 13px

### 5.8 断点与变量化

```css
/* 响应式断点标量（于 :root 声明，便于集中管理） */
:root {
  --bp-narrow: 640px;
  --bp-tablet: 800px;
  --sidebar-width: 240px;
  --trajectory-width: 360px;
}
```

所有 `@media` 查询引用这些变量（CSS 变量在 `@media` 中无效，但注释约定维护）：

```css
/* NARROW: < 640px */
@media (max-width: 639px) { ... }
/* TABLET: 640-799px */
@media (min-width: 640px) and (max-width: 799px) { ... }
/* DESKTOP: ≥ 800px */
@media (min-width: 800px) { ... }
```

### 5.9 与现有架构的兼容性

- 现有 `#nav { display:none }` + `@media { display:flex }` 冲突 → 重构为：`#nav` 默认为 `display:grid`（桌面），窄屏 `display:none` + `body.nav-open` 覆盖
- `body.nav-open` 语义不变，但窄屏时 `#nav` 的 `position:relative` + `z-index:6` 改为 `position:fixed; left:0; top:52px; height:calc(100dvh - 52px); z-index:20`（不要遮挡 header）
- `#overlay` 在窄屏 `z-index:15`（header 与 nav 之间）
- 轨迹面板 `z-index:30`（高于侧栏，与其遮罩联动）

### 5.10 响应式实施范围

| 项 | 改动 | 随哪个特性落地 |
|----|------|--------------|
| `#layout` 改为 `display:grid` | `app.css` + `index.html`（#layout 类） | 公共（美化步骤 1） |
| 侧栏重构（grid 布局 + 窄屏 fixed） | `app.css` + `index.html` | 公共 |
| 消息区居中 + 窄屏适配 | `app.css` | 公共 |
| composer 对齐 | `app.css` | 公共 |
| header 响应式隐藏 | `app.css` | 公共 |
| 轨迹抽屉 responsive | `app.css` + `trajectory.js` | 特性 3 |
| 模态窄屏全屏 | `app.css` | 特性 1 |
| 断点统一 | `app.css`（注释约定） | 公共 |

---

## 六、接口改动汇总

| 位置 | 改动 |
|------|------|
| `miniagent/loop_api.go` | `OnToolUse`/`OnToolResult` 签名加 `step int`；`LoopHooks` 增 `OnStepUsage` |
| `miniagent/loop_tools.go`(handleToolCalls) | 透传 step 到钩子 |
| `miniagent/loop_extra.go` | `recordStepUsage` 末尾调用 `OnStepUsage` |
| `cmd/miniagent/emit.go` | `buildHooks` 适配新签名；装配 web 版 `OnStepUsage` 写 `step_usage` 事件 |
| `miniagent/event/event.go` | `toolUseEvent`/`toolResultEvent` 增 `step`；新增 `stepUsageEvent` + `EmitStepUsage` |
| `cmd/miniagent/web_turn.go` | turnSpec 增 `emitStepUsage bool`（serve 模式 true），传给 assembleHooks |
| `cmd/miniagent/web.go` | 路由 `GET /api/tree` |
| `cmd/miniagent/web_tree.go`（新） | 目录树 API |
| `webstatic/static/{views,events}.js` | view 数据模型扩展（usage/trajectory/curStep）+ 事件捕获 |
| `webstatic/static/{usage,trajectory,dirpicker}.js`（新） | 三个组件 |
| `webstatic/static/{app,index,app.css}` | 入口接入（header 按钮、composer 改造、§四 美化） |
| `webstatic/assets.go` | 新 JS 文件进 go:embed + TestNames 钉死 |

**接口兼容性**：钩子签名变化影响 `miniagent` 包内部与测试，属库内 API 变更（semver minor）；NDJSON 事件均为新增字段/新事件类型，向后兼容。

---

## 七、安全边界

| 面 | 风险 | 控制 |
|----|------|------|
| 目录树 API | 任意路径列举（信息泄露） | 只读；`requireAuth`；不跟随 symlink；无写入能力。agent 本身 shell 无约束（既有声明），目录列举不放大权限 |
| `step_usage` 事件 | 无新增风险（数值来自已有计数） | — |
| 前端渲染 | XSS（工具输入/输出渲染） | 全部走 `textContent`（现逻辑已是），`innerHTML` 仅 mdRender 转义路径 |
| 前端样式 | CSP 内联样式阻断 | 新样式全进 `app.css`；禁止 `element.style`，用 classList + CSS 变量（§4.9） |
| 大数据 | 超大目录/巨量 step | 目录 500 条截断；轨迹步骤上限（如 500 步截断 + 提示） |

---

## 八、实施步骤

1. **后端事件增强**（共享支点）：
   - 钩子签名 + `step` 字段 + `step_usage` 事件 + `emitStepUsage` 装配 + 全部测试更新
   - 跑通 `make verify`
2. **公共美化 + 响应式骨架**（Token 变量/间距/header/布局）：
   - `app.css` 令牌层 + header 毛玻璃 + 间距网格替换魔法值
   - `#layout` 改 `display:grid` 三栏 + 侧栏重构（`position:fixed` 窄屏）+ 断点 640/800 统一
   - 消息区居中、composer 对齐、header 响应式隐藏
   - 独立提交（无逻辑依赖，可先于特性）
3. **特性 2 用量可视化**（依赖 step_usage）：
   - `usage.js` + `views.js` 模型 + `events.js` 钩入 + header 预算 + 用量样式
   - 先做这个，因为轨迹也要用 step_usage 锚点
4. **特性 3 轨迹视图**（依赖 step 字段）：
   - `trajectory.js` + 事件捕获 + 抽屉 + 节点定位 + 卡片美化
   - 抽屉响应式（桌面 360px / 窄屏全屏）随 §5.6 落地
5. **特性 1 目录选择器**（独立，可并行）：
   - `web_tree.go` API + `dirpicker.js` + composer 改造 + 模态样式（窄屏全屏随 §5.7）
6. **收尾**：assets 注册/TestNames、`CHANGELOG`/`ARCHITECTURE`/`README`、`WEBUI_NEXT.md` 勾选完成项

每步独立提交、独立 verify。

---

## 九、决策清单（已定案）

| # | 问题 | 定案 | 理由 |
|---|------|------|------|
| 1 | 钩子签名加 `step` vs 事件层后补 | **钩子签名加 `step int`** | 数据源头正确；`handleToolCalls` 已持有 step，改动集中；事件层猜测不精确 |
| 2 | `step_usage` 走新钩子 vs OnStep 差值 | **新钩子 `OnStepUsage`** | 增量直得无差值误差；OnStep 累计值受 compaction 影响（rewrite 历史后累计值不再反映本步） |
| 3 | 目录树 API 路径范围 | **放开任意绝对路径，只读 + symlink 跳过 + 500 截断** | 与 agent 既有 shell 无约束边界一致；白名单根增加配置面且限制用户浏览自由 |
| 4 | 轨迹面板形态 | **视图内右侧抽屉（镜像视图）** | 不破坏消息流 DOM；复用 `#overlay` 模式；CSS transform 动画（无内联 style） |
| 5 | 预算条数据来源 | **boot 时 `GET /api/config` 读 `run.max_tokens_total`** | 不改 result 事件结构；配置保存后前端刷新（config.js 已有回填钩子） |
| 6 | 最近目录存储 | **localStorage** | 零后端写入面；跨浏览器共享非刚需（部署通常单浏览器管理） |
| 7 | 用量面板入口 | **双入口：result 卡折叠 + header token 徽标点击弹 `<dialog>`** | 就近查看 + 全局汇总两不误 |
| 8 | 美化技术约束 | **CSS 变量 + app.css，禁内联 style/图标字体/第三方库** | 满足 CSP `style-src 'self'`；无构建链；离线可用（既有约束） |
| 9 | 动效 | **仅交互态 transition + 定位高亮例外** | 避免入场动画复杂性；性能与 CSP 双合规 |
| 10 | 响应式策略 | **CSS Grid 三栏骨架 + `position:fixed` 抽屉，断点仅 640/800 两档（参考 DSH）** | 桌面三栏/平板双栏/窄屏单栏；避免多断点维护（现仅 2 档引 4 条 @media） |
| 11 | 窄屏侧栏行为 | **`position:fixed` 抽屉 + `z-index` 分层（header<overlay<nav<trajectory）** | 修复现有 `position:relative` 侧栏遮挡 header 的问题；与轨迹抽屉共用 overlay 模式 |
| 12 | 消息区对齐 | **桌面 `.ev` 居中 `max-width:900px`，用户消息右对齐 80% 宽** | 参考 DSH 排版：集中注意力于内容列，窄屏全宽 |