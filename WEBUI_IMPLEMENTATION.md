# WebUI 三项特性实现方案：工作目录选择器 / Token 用量可视化 / 工具轨迹视图

> 对应 `WEBUI_NEXT.md` 路线图的 P0（工作目录选择器、工具轨迹视图）+ P1（Token 用量可视化）。
> 本文档为详细实现方案：共享后端事件增强、三个特性各自的 API/前端设计、安全边界、测试计划。

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
- **方案**：`miniagent/loop_api.go` 的 `OnToolUse`/`OnToolResult` 钩子签名增加 `step int` 参数（核心接口改动，同步更新 `buildHooks`、测试、`looptest`）。`handleToolCalls` 调用处已有 step，透传即可。
- 序列化：`toolUseEvent`/`toolResultEvent` 增加 `Step int \`json:"step"\``。
- **兼容**：全部消费方（CLI stdout 消费者、前端、session 解析）只读新字段，无破坏；旧会话 replay 无 step 字段时前端降级（见下文轨迹视图）。

### 增强 B：新增 `step_usage` 事件（per-step 用量）

- **关键现成钩子**：`miniagent/loop_api.go` 的 `OnStep(ctx, StepSnapshot)`，`StepSnapshot` 已含 `Step`/`InputTokens`（累计）/`OutputTokens`（累计）/`LLMRequests`。
- 但 StepSnapshot 是**累计值**，需要差值算出本步增量。`recordStepUsage`（`loop_extra.go:178`）在每步后已把本步 usage 累进 `total`——在该点新增钩子回调 `OnStepUsage(step, inDelta, outDelta)` 最干净，或在 `metrics.NewStepEmitter` 的 CLI 路径之外为 web 单独装配。
- **最终方案**：`LoopHooks` 新增可选字段 `OnStepUsage func(step, in, out int)`；`recordStepUsage` 末尾调用（增量直接可得，无需差值）。web 层 `assembleHooks` 装配为向 NDJSON 流写：
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
- **安全边界**：
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
- 最近目录：localStorage `miniagent.web.recent_dirs`（去重、上限 10、最近在前），模态顶部快捷区。
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
  steps: [],            // [{step, in, out, toolCalls}]
  totals: {in, out},    // 当前返回的 view.tokens 保持
  budget: 0,            // max_tokens_total，0=无预算
};
```
- `view.usage.budget` 初始化：打开配置页或 boot 时从 `GET /api/config` 读取 `run.max_tokens_total`（无则 0=隐藏预算条）。缓存于 store。

**渲染**（新文件 `usage.js`）：
- `renderUsageBar(wrapper, {in, out, budget})`：flex 双色条（input=蓝、output=绿），tooltip `in=N out=N`；有预算时叠加红色预算标线 + 百分比文本。
- `renderStepUsage(container, steps)`：每步一行迷你条 + `step N · in=a out=b · tools=k`，折叠在 `<details>` 内。
- 钩入 `events.js`：
  - `case "result"`：除现有 usage 文本外，追加用量条形图；
  - `case "step_usage"`：`view.usage.steps.push(ev)`，若用量面板已展开则增量重绘（节流：沿用 300ms 定时器思路，或 step_usage 每步一次直接重绘，成本低）。
- header：`in=N out=M` 保持文本；有预算时改为 `in=N out=M / BUDGET`。

**展示入口**：每 result 卡片内折叠「用量详情」；视图 header 加「用量」切换按钮弹面板（或并入现有 header token 徽标点击）。

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
- 视图 header 加「轨迹」按钮 → 右侧抽屉面板（或视口内悬浮面板，CSS `position:absolute`）。
- 面板内容：按 `order` 渲染步骤卡片：
  - 卡片头：`Step N` + 用量迷你条（复用 usage.js）+ 工具数 + 时间；
  - 卡片体：每个工具一行（`name` + 折叠 input/output pre，复用 copy 按钮逻辑）；
  - 点击卡片 → 滚动定位到消息流中该 step 的首个节点（节点上加 `data-step` 属性，`scrollIntoView`）。
- 折叠状态：面板整体 `<details>`；每步卡片默认折叠，仅显示摘要。

**与消息流的关系**：轨迹面板是**镜像视图**，不改动 `appendToolUse`/`appendToolResult` 的现有 DOM 结构（避免回归）。

### 测试
- 后端：`tool_use`/`tool_result` 事件带 `step` 断言（`web_turn_test.go`）。
- 前端 `node --check` + 断点定位（构造含步骤跳转的流）。

---

## 四、接口改动汇总

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
| `webstatic/static/{app,index,app.css}` | 入口接入（header 按钮、composer 改造、样式） |
| `webstatic/assets.go` | 新 JS 文件进 go:embed + TestNames 钉死 |

**接口兼容性**：钩子签名变化影响 `miniagent` 包内部与测试，属库内 API 变更（semver minor）；NDJSON 事件均为新增字段/新事件类型，向后兼容。

---

## 五、安全边界

| 面 | 风险 | 控制 |
|----|------|------|
| 目录树 API | 任意路径列举（信息泄露） | 只读；`requireAuth`；不跟随 symlink；无写入能力。agent 本身 shell 无约束（既有声明），目录列举不放大权限 |
| `step_usage` 事件 | 无新增风险（数值来自已有计数） | — |
| 前端渲染 | XSS（工具输入/输出渲染） | 全部走 `textContent`（现逻辑已是），`innerHTML` 仅 mdRender 转义路径 |
| 大数据 | 超大目录/巨量 step | 目录 500 条截断；轨迹步骤上限（如 500 步截断 + 提示） |

---

## 六、实施步骤

1. **后端事件增强**（共享支点）：
   - 钩子签名 + `step` 字段 + `step_usage` 事件 + `emitStepUsage` 装配 + 全部测试更新
   - 跑通 `make verify`
2. **特性 2 用量可视化**（依赖 step_usage）：
   - `usage.js` + `views.js` 模型 + `events.js` 钩入 + header 预算
   - 先做这个，因为轨迹也要用 step_usage 锚点
3. **特性 3 轨迹视图**（依赖 step 字段）：
   - `trajectory.js` + 事件捕获 + 面板 + 节点定位
4. **特性 1 目录选择器**（独立，可并行）：
   - `web_tree.go` API + `dirpicker.js` + composer 改造
5. **收尾**：assets 注册/TestNames、`CHANGELOG`/`ARCHITECTURE`/`README`、`WEBUI_NEXT.md` 勾选完成项

每步独立提交、独立 verify。

---

## 七、决策清单

| 问题 | 建议 | 备选 |
|------|------|------|
| 钩子签名加 `step`（库 API 变更） vs 事件层后补 | 钩子签名加 step（数据源头正确） | 事件层猜 step（text_delta 已有 step，可推导但不精确） |
| `step_usage` 走新钩子 vs 复用 OnStep 差值 | 新钩子 `OnStepUsage`（增量直得，无差值误差） | OnStep 累计值算差值（受 compaction 影响有偏差） |
| 目录树 API 是否放开任意绝对路径 | 放开但只读+symlink 跳过（与 agent 既有边界一致） | 限定配置白名单根（更严，但增加配置面） |
| 轨迹面板形态 | 视图内右侧抽屉（不破坏消息流） | 消息流内插入轨迹摘要（破坏现有 DOM 结构） |
| 预算条数据来源 | `GET /api/config` 的 `run.max_tokens_total` | 每次 result 事件带累计预算换算（后端改 result 结构） |
| 最近目录存储 | localStorage（浏览器本地） | 后端 session meta（跨浏览器，但增加写入面） |