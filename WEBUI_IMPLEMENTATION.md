# miniagent WebUI 实施规格（可直接编码）

> 版本：2.0（规格化改写） · 状态：评审通过，可进入编码
> **注：本规格已落地（commit 9096515 / 8e517fd）。响应式与交互体验的后续修复以 `WEBUI_RESPONSIVE_UX.md` 为准（审计基线 = 8e517fd）。**
> 范围：工作目录选择器、Token 用量可视化、工具轨迹视图 + 全站美化与响应式
> 阅读对象：LLM 编码代理。本规格的每个断言均为**必须实现**的契约；示例代码为最终形态参考，非建议。

---

## 0. 总览

### 0.1 目标
- P0：目录选择器（`GET /api/tree` + 模态）、工具轨迹视图（step 分组抽屉）
- P1：Token 用量可视化（step_usage 事件 + 条形图 + 预算条）
- 既有代码同时完成：全站美化（登录页/等待态/焦点态/滚动条/空态/toast/卡片层次）、移动端自适应（Grid 骨架 + 抽屉）

### 0.2 全局硬约束
1. **CSP 无内联**：`style-src 'self'`；禁止 `element.style.*`、禁止 `<style>` 标签、禁止 `setAttribute("style")`。动态样式一律 `classList` + CSS 变量。
2. **无构建链**：纯 ES modules，新 JS 文件须注册 `cmd/miniagent/webstatic/assets.go` 的 `go:embed` 与 `assets_test.go` 的 `TestNames`。
3. **无第三方依赖**：图标用 Unicode/CSS 图形；字体仅系统栈。
4. **Go 约束**：非 `_test.go` 文件 ≤300 行；`gofmt -s` 干净；`make verify` 全绿。
5. **JS 约束**：每个新 JS 文件 `node --check` 通过；全量 `node --check cmd/miniagent/webstatic/static/*.js` 通过。
6. **XSS**：用户/工具内容一律 `textContent`；`innerHTML` 仅经过 `mdRender`（先 escape 再 md）的路径。

### 0.3 术语
| 词 | 含义 |
|----|------|
| step | ReAct 循环的一次 LLM 调用（1-based，`result.steps` 计数） |
| view | 前端一个会话的视图对象（`views.js` 的 view） |
| trajectory | 按 step 分组的工具执行轨迹（镜像数据，不改消息流 DOM） |

---

## 1. 共享支点：事件与钩子增强

### 1.1 `tool_use` / `tool_result` 增加 `step` 字段

**Go 接口变更**（`miniagent/loop_api.go`，第 54/94/98 行附近）：

```go
// 变更前
type OnToolUse func(name, callID, input string) error
type OnToolResult func(name, callID string, r ToolResult) error
// 变更后（step 插入为第一个 int 形参）
type OnToolUse func(step int, name, callID, input string) error
type OnToolResult func(step int, name, callID string, r ToolResult) error
```

**必须同步修改的调用方**（全量清单）：
| 文件 | 位置 | 改动 |
|------|------|------|
| `miniagent/loop_api.go` | 类型定义 | 签名加 `step int`（L54、L94、L98） |
| `miniagent/tool_handler.go` | L27-29 | `hooks.OnToolUse(step, tc.Name, tc.ID, tc.Args)` |
| `miniagent/tool_handler.go` | L51-52 | `hooks.OnToolResult(step, tc.Name, tc.ID, tres)` |
| `miniagent/tool_handler.go` | L62-64 | `hooks.OnToolResult(step, calls[j].Name, calls[j].ID, results[j])` |
| `cmd/miniagent/emit.go` | L26-31 `buildHooks` | 闭包签名同步加 `step` |
| `miniagent/event/event.go` | `toolUseEvent`/`toolResultEvent` | 增字段 `Step int \`json:"step"\``；`EmitToolUse`/`EmitToolResult` 签名加 `step int` |
| `miniagent/event/event.go` | `ToolUseWriter` | 签名同步 |

**序列化契约**（新增字段，旧消费方忽略）：
```json
{"type":"tool_use","step":2,"name":"read","call_id":"call_x","input":"{\"path\":\"a.go\"}"}
{"type":"tool_result","step":2,"name":"read","call_id":"call_x","output":"...","truncated":false,"is_error":false}
```

### 1.2 新事件 `step_usage`

**Go 接口变更**（`miniagent/loop_api.go` `LoopHooks` 结构体新增字段）：

```go
// 新增（可选字段，nil = 不装配，零开销）
OnStepUsage func(step, inTokens, outTokens, toolCalls int)
```

**装配点**（`miniagent/loop_extra.go` `recordStepUsage` 函数末尾，L189 之后）：

```go
// 伪代码——确切插入位置：berr 判断之后、return nil 之前
if hooks.OnStepUsage != nil {
    hooks.OnStepUsage(step, resp.Usage.InputTokens, resp.Usage.OutputTokens, len(resp.ToolCalls))
}
```

> 注意：`resp.Usage.InputTokens` 是**本步增量**（已由 provider 返回），非累计值；勿用 `total`。

**Web 装配**（`cmd/miniagent/emit.go` 新增函数，`assembleHooks` 内部条件设置）：
- 新增 `turnSpec.emitStepUsage bool`（`cmd/miniagent/run_turn.go` 结构体新字段），web 路径（`cmd/miniagent/web_turn.go` 构造 turnSpec 处）置 `true`，CLI 默认 `false`。
- 当 `spec.emitStepUsage` 时，`assembleHooks` 设置 `hooks.OnStepUsage`，向 NDJSON 写：

```json
{"type":"step_usage","step":1,"input_tokens":11400,"output_tokens":333,"tool_calls":1}
```

**字段表**：`type="step_usage"`（新），`step int`（必填），`input_tokens`/`output_tokens int`（本步增量，可为 0），`tool_calls int`。无 `ts`（前端用到达时间）。

### 1.3 兼容与降级规则（前端 `events.js` 强制）
- `tool_use`/`tool_result` 事件**缺 `step`**（旧会话 replay / CLI 记录）时：归属 `view.curStep`（见 §4.2 捕获逻辑），不得崩溃。
- 无 `step_usage` 事件（旧数据）时：轨迹卡片头显示 `-` 代替用量，不阻塞。

---

## 2. 特性 1：工作目录选择器

### 2.1 API 规格：`GET /api/tree`

| 项 | 值 |
|----|----|
| 方法/路径 | `GET /api/tree` |
| 鉴权 | `requireAuth`（web.go 的 api mux 内登记，与 `/api/config` 同级） |
| 查询参数 | `path`：绝对路径（必填）。附加可选 `hidden=1`（含隐藏目录，默认不含） |
| 成功 200 | `{"path":"/x","parent":"/","dirs":[{"name":"a","path":"/x/a"},...],"total":N,"truncated":false}` |
| 错误 400 | path 非绝对/非法 → `{"error":"path must be an absolute path"}` |
| 错误 404 | path 不存在/不可读 → `{"error":"readdir <path>: no such file or directory"}`（`os.ReadDir` 原错误） |
| 截断 | `total` 超过 500 时：返回前 500 条，`truncated:true` |
| 安全 | 跳过 `fs.ModeSymlink` 条目；只列目录；无写入 |

**处理器规格**（新文件 `cmd/miniagent/web_tree.go`，≤300 行）：
```go
func (s *webServer) handleTree(w http.ResponseWriter, r *http.Request) // 实现按上表
```
- 路由在 `web.go mux()`：`api.HandleFunc("GET /api/tree", s.handleTree)` 加在 `GET /api/config` 旁。
- `dirs` 按名称字典序；`parent` 用 `filepath.Dir`；根目录 `parent` 为 `/`。

**后端测试**（新文件 `cmd/miniagent/web_tree_test.go`，用例名固定）：
| 用例 | 断言 |
|------|------|
| `TestTree_ListDirs` | 临时目录含子目录+文件+隐藏目录；确认只列子目录、无隐藏、`total` 正确 |
| `TestTree_ParentPath` | 深层路径的 `parent` 正确；根目录 parent 为 `/` |
| `TestTree_RejectsRelative` | `path=foo` → 400 |
| `TestTree_MissingDir` | `path=/definitely/not/exist` → 404 |
| `TestTree_SkipsSymlink` | 含 symlink 子目录 → 响应不含它 |
| `TestTree_Truncates` | 构造 >500 子目录 → `truncated:true` 且 `total==500` |

### 2.2 前端组件规格：`dirpicker.js`（新文件）

**导出**：`export function attachDirPicker(opts)`；`opts.onPick(path)` 回填回调。
**挂载点**：`app.js` 的 boot 成功后调用一次。

**DOM 模板**（模态，`#dirpicker` 单例，懒创建）：
```html
<div id="dirpicker" class="modal" hidden>
  <div class="modal-panel" role="dialog" aria-label="选择工作目录">
    <div class="modal-head">
      <span class="modal-title">选择工作目录</span>
      <button class="ghost" id="dp-close" aria-label="关闭">✕</button>
    </div>
    <div class="modal-body">
      <div class="dp-recent" id="dp-recent"><!-- 最近目录快捷区，无则隐藏 --></div>
      <div class="dp-path-bar">
        <button class="ghost" id="dp-up" aria-label="上级目录">↰</button>
        <span class="dp-path" id="dp-path"></span>
      </div>
      <div class="dp-tree" id="dp-tree"><!-- 目录列表 --></div>
    </div>
    <div class="modal-foot">
      <button class="ghost" id="dp-cancel">取消</button>
      <button id="dp-choose">选择此目录</button>
    </div>
  </div>
</div>
```

**状态**：模块级 `let curPath = ""`；`let recent = loadRecent()`。
- `loadRecent()`：`JSON.parse(localStorage.getItem("miniagent.web.recent_dirs") || "[]")`。
- `saveRecent(p)`：去重（同值前移）、上限 10、`localStorage.setItem`。
- 打开（`openPicker()`）：`curPath = 当前输入框值或 "/"` → `fetchDirs(curPath)` → 显示模态；URL 参数：`fetch(\`/api/tree?path=${encodeURIComponent(curPath)}\`, {headers:{...authHeaders()}})`。
- 行点击 dir → `curPath = p.path; fetchDirs(curPath)`；`#dp-up` → `curPath = parent; fetchDirs`。
- 「选择此目录」→ `saveRecent(curPath)`；`opts.onPick(curPath)`；关闭。

**交互/无障碍**：模态显示时焦点在 `#dp-close`；`Escape` 关闭；`#dp-choose` Enter 触发。

### 2.3 集成点
| 文件 | 改动 |
|------|------|
| `index.html` | workdir 行加 📁 按钮：`<input id="workdir" ...> <button id="workdir-browse" type="button" aria-label="浏览目录">📁</button>` |
| `app.js` | boot 后 `attachDirPicker({onPick: p => { $("workdir").value = p; saveWorkdir(p); }})`；`#workdir-browse` click → 打开选择器 |
| `app.css` | §5.4/§5.5 的 modal/dp-* 样式 |

---

## 3. 特性 2：Token 用量可视化

### 3.1 数据模型（`views.js` `createView` 内新增）
```js
view.usage = {
  budget: 0,          // run.max_tokens_total，0 = 无预算
  steps: []           // [{step, in, out, toolCalls}]
};
```
**budget 初始化**：`store.js` 新增 `let budgetCache = 0;` + `export async function refreshBudget()`（调 `GET /api/config`，取 `run.max_tokens_total||0`，写 `budgetCache`）。`app.js boot()` 调一次；`config.js saveConfig` 成功后调一次。
- `view.usage.budget` 在 `renderEvent` 的 `case "result"` 里从 `budgetCache` 读（防启动竞态）。

### 3.2 渲染（新文件 `usage.js`）

**导出函数**（签名固定）：
```js
export function renderUsageBar(container, { in: tin, out: tout, budget }) // → 追加 .usage-bar 块
export function renderStepUsageList(container, steps)                      // → 追加折叠 <details>
```
**DOM**（`renderUsageBar`）：
```html
<div class="usage-bar" title="in={{tin}} out={{tout}}{{budget ? ` / 预算 ${budget}` : ''}}">
  <div class="usage-in" style="width:CALC"></div>   <!-- 注意：宽度须经 CSSOM，禁内联 -->
  <div class="usage-out" style="width:CALC"></div>
</div>
```
> 禁内联约束与宽度计算冲突 → **方案**：`.usage-bar` 本身是 flex；两个子块宽度用 `%` 通过 `element.style.width` 设置会违反 CSP 吗？不会——CSP `style-src 'self'` 只管 `<style>`/内联属性，**CSSOM `element.style.width` 不受限**（N1 教训只针对 `style="display"` 属性初值被丢弃，运行时 CSSOM 赋值是允许的；见 app.js 现用 `el.style.display` 先例）。因此宽度用 `el.style.width = pct + "%"` 合法。文档明示：**CSSOM 赋值允许，HTML 内联属性禁**。
- 计算：`total = tin+tout`；`in% = tin/total*100`、`out% = 100-in%`；`budget>0 且 total>budget` 时追加 `<div class="usage-budget-mark">`（绝对定位 `left: budget/total*100%`）；total==0 时整条 `.usage-empty` 灰条。

**DOM**（`renderStepUsageList`）：
```html
<details class="usage-steps">
  <summary>用量详情（N 步）</summary>
  <div class="usage-step" title="step 1 · in=a out=b · tools=k">
    <span class="usage-step-label">step 1</span>
    <div class="usage-step-bar"><div class="usage-in" style="width:a%"></div><div class="usage-out" style="width:b%"></div></div>
    <span class="usage-step-meta">in=a out=b · tools=k</span>
  </div>...
</details>
```

### 3.3 钩入（`events.js` `renderEvent`）
- `case "result"`（现有 180-198 行块内）：在追加 `.usage` 文本行**之后**调用 `renderUsageBar(d, {in: ev.input_tokens||0, out: ev.output_tokens||0, budget: view.usage.budget})` 和 `renderStepUsageList(d, view.usage.steps)`；然后 `view.usage.steps.length = 0`（每轮结束清空 per-step 缓存）。
- `case "step_usage"`（新）：`view.usage.steps.push({step: ev.step, in: ev.input_tokens||0, out: ev.output_tokens||0, toolCalls: ev.tool_calls||0})`；**不**触发重绘（等 result 一次性渲染）。

### 3.4 Header 徽标（`store.js showSessionID`）
- `tok.textContent = in/out 文本` 后追加：`budget>0 && total>0` 时 `tok.textContent += \` / ${budget}\``。
- startWait 的「等待响应…」动画保留。

### 3.5 测试
- 后端：`cmd/miniagent/web_turn_test.go` 新用例 `TestWebTurn_StepUsageEvents`：
  1. 流中包含 `step_usage` 且 step 从 1 递增；
  2. `Σ step_usage.input_tokens == result.input_tokens`，output 同理；
  3. CLI 路径（`metalStep=false`）不产生 `step_usage`（断言流中无该 type）。
- 前端 `node --check`。

---

## 4. 特性 3：工具轨迹视图

### 4.1 数据模型（`views.js` `createView` 新增）
```js
view.trajectory = { order: [], steps: new Map() };
view.curStep = 0;   // 最近一次 delta 的 step，降级归属用
```

### 4.2 事件捕获（`events.js renderEvent` 追加分支，不改现有 DOM 渲染）
| 事件 | 捕获行为 |
|------|---------|
| `text_delta`/`reasoning_delta` | `view.curStep = ev.step`（若存在） |
| `tool_use` | `step = ev.step ?? view.curStep`；若无序 → `view.trajectory.order.push(step)`；`Map.get(step)` 不存在则创建 `{step, ts: ev.ts||Date.now(), in:0, out:0, tools:[]}`；`tools.push({name: ev.name, callID: ev.call_id, input: ev.input, output:"", isError:false, ts: ev.ts})` |
| `tool_result` | 按 `ev.call_id` 在全部 step 的 tools 中查找（`trajectory.steps` 迭代），补 `output=ev.output`、`isError=ev.is_error`；未找到则忽略 |
| `step_usage` | `view.usage.steps.push(...)`（同 §3.3）+ 同步 `trajectory.steps.get(ev.step)` 的 `in/out`（存在时） |

**兜底**：`case "result"` 时把 `view.curStep = 0` 重置（新一轮）。

### 4.3 渲染（新文件 `trajectory.js`）
**导出**：`export function attachTrajectory(view)（创建面板 DOM 并挂到 #layout，仅当 view 有 tool 事件时生效）`；内部函数 `renderStepCard(view, entry)`。
**DOM 结构**：
```html
<div id="trajectory-panel" class="trajectory-panel" hidden>
  <div class="trajectory-head">
    <span>轨迹</span>
    <button class="ghost" id="tj-close" aria-label="关闭轨迹">✕</button>
  </div>
  <div class="trajectory-body" id="tj-body"></div>
</div>
```
**卡片模板**：
```html
<details class="trajectory-step" data-step="{{step}}">
  <summary class="trajectory-step-head">
    <span class="tj-step-no">Step {{step}}</span>
    <span class="tj-step-meta">{{in ? `in={{in}} out={{out}}` : '—'}} · {{tools.length}} 工具</span>
  </summary>
  <div class="trajectory-tools">
    <details class="trajectory-tool">
      <summary>{{name}} <span class="time">{{fmtTime(ts)}}</span></summary>
      <pre class="tj-input">{{input}}</pre>
      <pre class="out{{isError ? ' err' : ''}}">{{output}}</pre>
    </details>
  </div>
</details>
```
- 全部内容 `textContent` 填充（含 pre）。
- 点击 `summary.trajectory-step-head`：除了原生 details 开合，还要 `scrollToStep(step)` → 找到 `#events` 内 `[data-step="${step}"]` 节点（见 §4.4），`scrollIntoView({behavior:"smooth", block:"start"})` + `.pulse-highlight` 类（2s 后移除）。

### 4.4 消息流节点打标（`events.js` 小改）
- `appendToolUse`、`appendDelta`、`appendToolResult`、result 的 `evDiv` 创建后：若 `ev.step` 存在，`d.dataset.step = ev.step`；`tool_result` 配对时把 output pre 加在 target（已带 data-step）。
- 「首个节点」判定：`document.querySelector(\`#events .view[data-view-key="${view.key}"] [data-step="${step}"]\`)`。view DOM 在 `createView` 时已有 key 属性？无——**增加**：`createView` 里 `dom.dataset.viewKey = key`（一行，向后兼容）。
- `scrollToStep` 找不到节点（replay 无 step）→ 静默忽略。

### 4.5 打开/关闭
- 开关按钮：`index.html` header 加 `<button id="traj-btn" class="ghost" title="工具轨迹">≡</button>`（aria-label「工具轨迹」，窄屏隐藏见 §6.4）。
- `app.js`：`#traj-btn` click → `toggleTrajectory(view)`（`#trajectory-panel.hidden` 切换 + `#layout.trajectory-open` 类切换）。
- 面板内容刷新时机：`view` 激活（`activate`）时重建一次；运行中 `step_usage`/`tool_use` 到达时若面板可见则增量 append（追加卡片，不重绘已有）。

### 4.6 测试
- 后端：`cmd/miniagent/web_turn_test.go` 用例 `TestWebTurn_ToolEventsCarryStep`：fake LLM 两步（step1 工具+step2 文本），断言 `tool_use`/`tool_result` 事件的 `step` 分别为 1/1。
- 前端 `node --check`。

---

## 5. 美化（规格——含全部既有缺口）

> 现状缺口盘点（已核对 app.css/events.js/store.js/index.html）：
> 登录页无层次；header 元素无响应式收敛；无等待 spinner（`#wait` 纯文本）；`#to-bottom` 无动效；主区无自定义滚动条；无 `:focus-visible`；`#events` 无空态；`inlineHint` 复用 `#wait`（等待与错误提示混淆）；会话列表 running 点与删除钮可能重叠；工具卡 emoji 用 `🔧` 文本前缀不一致；配置页 `.cfg-*` 未接入新设计令牌。

### 5.1 设计令牌（`app.css` 顶部，双主题完整值）

```css
:root {
  /* 既有（保留） */
  --bg:#0d1117; --panel:#161b22; --border:#30363d;
  --fg:#e6edf3; --muted:#8b949e; --accent:#58a6ff; --err:#f85149; --ok:#3fb950;
  /* 新增层级背景 */
  --bg-elev-1:#1c2128; --bg-elev-2:#21262d; --bg-hover:#21262d;
  /* 语义色 */
  --info:#58a6ff; --warn:#d29922; --usage-in:#58a6ff; --usage-out:#3fb950;
  /* 度量 */
  --radius-sm:6px; --radius-md:8px; --radius-lg:12px;
  --space-xs:4px; --space-sm:8px; --space-md:12px; --space-lg:16px;
  --font-mono:ui-monospace,SFMono-Regular,"JetBrains Mono",monospace;
  --shadow-modal:0 8px 24px rgba(0,0,0,.4);
  --transition-fast:120ms ease; --transition-med:200ms ease;
}
:root[data-theme="light"] {
  --bg:#f6f8fa; --panel:#ffffff; --border:#d0d7de;
  --fg:#1f2328; --muted:#59636e; --accent:#0969da; --err:#cf222e; --ok:#1a7f37;
  --bg-elev-1:#eef1f4; --bg-elev-2:#e6e9ec; --bg-hover:#eef1f4;
  --info:#0969da; --warn:#9a6700; --usage-in:#0969da; --usage-out:#1a7f37;
  --shadow-modal:0 8px 24px rgba(31,35,40,.15);
}
```

### 5.2 全局交互态
```css
:focus-visible { outline:2px solid var(--accent); outline-offset:1px; }
body { font: 14px/1.6 -apple-system,"Segoe UI",sans-serif; }          /* 既有 */
#events, #session-list, .trajectory-body { scrollbar-width:thin; scrollbar-color:var(--border) transparent; }
#events::-webkit-scrollbar, #session-list::-webkit-scrollbar, .trajectory-body::-webkit-scrollbar { width:8px; }
#events::-webkit-scrollbar-thumb, #session-list::-webkit-scrollbar-thumb, .trajectory-body::-webkit-scrollbar-thumb { background:var(--border); border-radius:4px; }
```

### 5.3 登录页
```css
#login { background: radial-gradient(1200px 600px at 20% -10%, color-mix(in srgb, var(--accent) 12%, transparent), transparent 60%), var(--bg); }
#login-form { width:320px; padding:32px 28px; background:var(--bg-elev-1); border:1px solid var(--border); border-radius:var(--radius-lg); box-shadow:var(--shadow-modal); }
#login-form h1 { font-size:22px; letter-spacing:.5px; }
#login-form input { width:100%; }
#login-form button { width:100%; background:var(--accent); color:#fff; border:none; }
#login-err { color:var(--err); font-size:13px; min-height:18px; text-align:center; }
```

### 5.4 Header
```css
header { position:sticky; top:0; z-index:40; height:52px; padding:0 var(--space-lg);
         background: color-mix(in srgb, var(--bg) 85%, transparent); backdrop-filter: blur(8px); -webkit-backdrop-filter: blur(8px); }
header h1 { font-size:15px; }
#session-tokens { font-variant-numeric:tabular-nums; }
```
> `position:sticky` 与 §6 的 Grid 布局共存：header 独立于 #layout 之上，sticky 保证滚动时毛玻璃常驻。

### 5.5 等待态与提示（解决 #wait 复用歧义）
- `#wait` 仅作等待指示：加 spinner。
```css
#wait:not(:empty)::before { content:""; display:inline-block; width:12px; height:12px; margin-right:6px;
  border:2px solid var(--border); border-top-color:var(--accent); border-radius:50%;
  animation:spin 800ms linear infinite; vertical-align:-2px; }
@keyframes spin { to { transform:rotate(360deg); } }
#wait.msg-error { color:var(--err); }
#wait.msg-error::before { border-top-color:var(--err); animation:none; content:"⚠"; }
```
- `app.js inlineHint`（L321-328）：内部改为 `el.classList.add("msg-error")` + 2.5s 后移除类并清空（替换现有直接改 style.color 的做法——现有 `el.style.color="var(--err)"` 是 CSSOM 允许的先例，但统一走类更干净）。
- `startWait`（L376-380）：`el.classList.remove("msg-error")`。

### 5.6 会话列表
```css
#nav { background:var(--bg); }
#new-chat { background:var(--accent); color:#fff; border:none; font-weight:500; }
#new-chat:hover { filter:brightness(1.1); }
.sess-item { transition: border-color var(--transition-fast), background var(--transition-fast), transform var(--transition-fast); }
.sess-item:hover { transform:translateX(2px); }
.sess-del { right:8px; }                    /* 修复与 running 点重叠：running 点左移至 right:22px */
.sess-item.running::after { right:22px; }
.sess-item .preview { }                     /* 既有，保留 */
```

### 5.7 消息卡片与工具卡
```css
.ev { border-radius:var(--radius-md); }
.ev.user { border-left:3px solid var(--accent); }
.ev.result { border-left:3px solid var(--ok); }
.ev.error { border-left:3px solid var(--err); }
.ev.tool summary::before { content:"◈"; color:var(--accent); margin-right:6px; font-size:11px; }
.ev.tool pre { font-family:var(--font-mono); }
.copy-btn, .expand-btn { border-radius:var(--radius-sm); transition: color var(--transition-fast); }
```

### 5.8 用量条 / 轨迹卡（伴随 §3/§4 组件）
```css
.usage-bar { display:flex; height:8px; border-radius:4px; overflow:hidden; background:var(--bg-elev-1); margin-top:6px; position:relative; }
.usage-in { background:var(--usage-in); }
.usage-out { background:var(--usage-out); }
.usage-empty { background:var(--bg-elev-1); }
.usage-budget-mark { position:absolute; top:0; bottom:0; width:2px; background:var(--err); }
.usage-steps { margin-top:6px; font-size:12px; }
.usage-step { display:flex; align-items:center; gap:8px; padding:2px 0; }
.usage-step-bar { flex:1; max-width:200px; display:flex; height:6px; border-radius:3px; overflow:hidden; background:var(--bg-elev-1); }
.usage-step-label { width:52px; color:var(--muted); }
.usage-step-meta { color:var(--muted); font-size:11px; }
.trajectory-panel { position:fixed; right:0; top:52px; bottom:0; width:360px; background:var(--bg-elev-1);
  border-left:1px solid var(--border); z-index:30; display:flex; flex-direction:column; transition:transform var(--transition-med); }
.trajectory-panel[hidden] { display:none; }        /* 隐藏用 hidden，动画仅作用于可见态切换以外的 transform */
.trajectory-head { display:flex; justify-content:space-between; align-items:center; padding:10px 12px; border-bottom:1px solid var(--border); }
.trajectory-body { flex:1; overflow-y:auto; padding:8px 12px; }
.trajectory-step { border:1px solid var(--border); border-radius:var(--radius-md); margin-bottom:8px; background:var(--panel); }
.trajectory-step-head { display:flex; justify-content:space-between; cursor:pointer; padding:8px 10px; }
.trajectory-step[open] { border-color:var(--accent); }
.pulse-highlight { animation: pulseGlow 1s ease; }
@keyframes pulseGlow { 0%,100% { outline-color:transparent; } 50% { outline-color:var(--accent); } }
```

### 5.9 空态（`#events` 无消息时）
- `app.js`：`loadReplay`/`send` 前检查 `view.dom.children.length===0` 时插入：`<div class="ev empty-state muted">暂无对话，输入任务开始</div>`（`.empty-state{ text-align:center; padding:48px 0; border-style:dashed; }`）；首条事件到达时 `view.dom.querySelector(".empty-state")?.remove()`。
- 实现点：`appendUserPrompt` 与 `renderEvent` 首个分支前。

### 5.10 配置页令牌接入
- 把 `.cfg-*` 的魔法值替换为令牌：`--radius-md`、`--space-*`、`--bg-elev-1`（`.cfg-provider-head` 背景）、`--font-mono`（`.cfg-json textarea` 已有，改令牌）。
- `.cfg-save`/`.cfg-mode.active` 用 `--accent` 底白字（现已有，确认一致）。

### 5.11 `#to-bottom`
```css
#to-bottom { transition: transform var(--transition-fast), opacity var(--transition-fast); }
#to-bottom:hover { transform:scale(1.1); }
```

---

## 6. 响应式（规格）

### 6.1 断点（与 §5.8 一致，仅两档）
- NARROW：`@media (max-width: 639px)`
- TABLET：`@media (min-width: 640px) and (max-width: 799px)`
- DESKTOP：`@media (min-width: 800px)`

### 6.2 Grid 骨架（`#layout`）
```css
#layout { display:grid; grid-template-columns: var(--sidebar-width,240px) 1fr; grid-template-areas:"nav main"; flex:1; min-height:0; }
#nav { grid-area:nav; }
main { grid-area:main; min-width:0; }
#layout.trajectory-open { grid-template-columns: var(--sidebar-width,240px) 1fr var(--trajectory-width,360px); grid-template-areas:"nav main trajectory"; }
#trajectory-panel { grid-area:trajectory; position:relative; top:0; height:auto; }
@media (max-width: 639px) { #layout { display:flex; } }
```

### 6.3 侧栏（修复现有 display 冲突 + 遮挡问题）
```css
/* 桌面常显 */
@media (min-width: 800px) { #nav { display:grid; grid-template-rows:auto 1fr; } }
/* 窄屏抽屉：position:fixed 不遮挡 header */
@media (max-width: 639px) {
  #nav { position:fixed; left:0; top:52px; bottom:0; width:240px; z-index:20; display:none; background:var(--bg); }
  body.nav-open #nav { display:grid; }
  #overlay { position:absolute; inset:0; background:rgba(0,0,0,.5); z-index:15; }
}
/* 覆盖既有规则：删除原 `#nav{display:none}`（L34）与 `@media min-width:800px 的 #nav{display:flex}`（L120）两处冲突 */
```

### 6.4 header 元素（按 5.5 表格逐项）
```css
@media (max-width: 639px) {
  header h1 { font-size:14px; }
  #model-badge { display:none; }
  #logout { font-size:0; } #logout::after { content:"⏻"; font-size:18px; }
  #session-tokens { max-width:90px; overflow:hidden; text-overflow:ellipsis; white-space:nowrap; }
  #traj-btn { display:none; }
}
@media (max-width: 799px) { #version-badge { display:none; } }
```

### 6.5 消息区/composer/模态
```css
#events { padding:var(--space-lg) calc(var(--space-lg) + 8px); }
#events > .view { max-width:900px; margin:0 auto; width:100%; }
.ev { max-width:100%; }
@media (max-width:639px) {
  #events { padding:var(--space-sm); }
  .ev.user { max-width:90%; margin-left:auto; }
}
.composer-row { gap:var(--space-sm); }
@media (min-width:800px) {
  #composer { padding-left:calc(50% - 450px + var(--space-lg)); padding-right:calc(50% - 450px + var(--space-lg)); }
}
.modal-panel { width:min(560px, calc(100vw - 32px)); max-height:70vh; }
@media (max-width:639px) { .modal-panel { width:100vw; max-height:100dvh; border-radius:0; } }
```

---

## 7. 文件改动清单（精确）

### 后端
| 文件 | 改动 | 状态 |
|------|------|------|
| `miniagent/loop_api.go` | OnToolUse/OnToolResult 签名；LoopHooks.OnStepUsage | 改 |
| `miniagent/tool_handler.go` | 3 处钩子调用加 step | 改 |
| `miniagent/loop_extra.go` | recordStepUsage 末尾 OnStepUsage | 改 |
| `miniagent/event/event.go` | toolUseEvent/toolResultEvent 加 step；EmitToolUse/EmitToolResult/ToolUseWriter 签名；stepUsageEvent+EmitStepUsage | 改 |
| `cmd/miniagent/emit.go` | buildHooks 闭包签名；assembleHooks 装配 OnStepUsage | 改 |
| `cmd/miniagent/run_turn.go` | turnSpec.emitStepUsage | 改 |
| `cmd/miniagent/web_turn.go` | turnSpec 构造置 emitStepUsage=true | 改 |
| `cmd/miniagent/web.go` | 路由 GET /api/tree | 改 |
| `cmd/miniagent/web_tree.go` | 新增 handleTree | 新 |
| 测试：`miniagent/loop_iteration_test.go`、`policy/loop_integration_test.go`、`internal/miniagent/looptest/llm.go`、各 `*_test.go` 中直接构造 OnToolUse/OnToolResult 处 | 签名适配 | 改 |

### 前端
| 文件 | 改动 | 状态 |
|------|------|------|
| `webstatic/static/app.css` | §5/§6 全部样式 | 改 |
| `webstatic/static/index.html` | workdir-browse 按钮；traj-btn；模态容器（可 JS 建） | 改 |
| `webstatic/static/views.js` | view.usage/trajectory/curStep；dom.dataset.viewKey | 改 |
| `webstatic/static/events.js` | step_usage 分支；step 打标；trajectory 捕获；result 渲染用量 | 改 |
| `webstatic/static/store.js` | refreshBudget；showSessionID 预算；inlineHint/startWait 类化 | 改 |
| `webstatic/static/app.js` | attachDirPicker/trajectory 挂载；空态；响应式无关小改 | 改 |
| `webstatic/static/usage.js` | renderUsageBar/renderStepUsageList | 新 |
| `webstatic/static/trajectory.js` | attachTrajectory/卡片渲染 | 新 |
| `webstatic/static/dirpicker.js` | 目录选择器 | 新 |
| `webstatic/assets.go` | go:embed 3 个新 JS | 改 |
| `webstatic/assets_test.go` | TestNames want 列表 +3 | 改 |

---

## 8. 实施顺序（每步独立 commit + `make verify`）

| 步 | 内容 | 依赖 |
|----|------|------|
| 1 | 后端事件增强（1.1+1.2）+ 全部 Go 测试适配 | 无 |
| 2 | 设计令牌 + 全局交互态 + 登录页 + header + 等待态 + 会话列表 + 消息卡（§5.1-5.7,5.10-5.11）+ Grid 骨架 + 侧栏修复 + header 响应式（§6.1-6.4） | 无 |
| 3 | 特性 2 用量（§3 + §5.8 usage 部分） | 步 1 |
| 4 | 特性 3 轨迹（§4 + §5.8 trajectory 部分 + §6.2 trajectory-open） | 步 1 |
| 5 | 特性 1 目录选择器（§2 + §5.9 空态 + §6.5 模态） | 无 |
| 6 | 收尾：assets/TestNames、CHANGELOG/ARCHITECTURE/README、WEBUI_NEXT 勾选 | 全部 |

---

## 9. 验收标准（DoD）

- [ ] `make verify` 全绿；`node --check cmd/miniagent/webstatic/static/*.js` 全绿
- [ ] 后端：`step_usage`、`tool_use/tool_result.step` 的 3 个新增测试通过（§3.5/§4.6/§2.1 用例）
- [ ] 前端：三特性手工验证——目录选择器深链+回填、result 卡用量条+详情、轨迹抽屉定位；无 `element.style` 内联属性新增（CSSOM 赋值除外）
- [ ] 移动端：<640px 浏览器（DevTools 375px）侧栏 drawer、header 收敛、消息全宽、模态全屏均正常
- [ ] 双主题：暗/亮切换后所有新增变量有值，无未定义 var
- [ ] 旧会话 replay（无 step/step_usage）不报错，轨迹面板显示占位

---

## 10. 决策记录（含先前 12 项，未再变更）

| # | 决策 | 状态 |
|---|------|------|
| 1-12 | 见上一版 §九（钩子加 step / OnStepUsage / 目录树开放只读 / 轨迹抽屉镜像 / 预算取 config / 最近目录 localStorage / 用量双入口 / CSS 变量禁内联 / 交互态动效 / Grid 三栏 / 窄屏 fixed 侧栏 / 消息居中右对齐） | 保持 |
| 13 | **CSSOM 赋值合法性**：`element.style.width` 等运行时赋值不受 CSP `style-src 'self'` 限制（HTML 内联属性 `style="..."` 才受限）——用量条宽度、modal 面板等可 CSSOM 设值 | 新增确认 |
| 14 | **等待/错误提示分离**：`#wait` 仅等待（spinner），`inlineHint` 切 `msg-error` 类显示错误，不再混用 style.color | 新增 |
| 15 | **轨迹面板定位**：桌面 `position:fixed` 叠层；Grid 三栏模式下 `#layout.trajectory-open` 用 `grid-area` 内嵌——两者通过断点分流，避免 CSSOM 切换 | 新增 |