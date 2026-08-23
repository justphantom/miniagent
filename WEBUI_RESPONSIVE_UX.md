# WebUI 响应式与交互体验修复规格（PC / 移动端）

> 版本：1.0 · 基线：commit `8e517fd`（三特性 + UI 重设计已落地，后端 step/step_usage 链路完整）。
> 范围：**只修复与打磨**当前实现的响应式排版与交互体验，无新特性。
> 阅读对象：LLM 编码代理。每项均为必须实现的契约；`问题/证据` 行给出 file:line 依据。

## 0. 硬约束（沿用项目规约）

1. CSP `style-src 'self'`：禁止 HTML 内联 `style="..."` 属性与 `<style>` 标签；**运行时 CSSOM 赋值（`el.style.width`）合法**（app.js:128-129 已有先例）。
2. 无构建链/第三方库；图标用 Unicode/CSS。
3. 改动后：`node --check cmd/miniagent/webstatic/static/*.js` 全绿 + `make verify` 全绿。
4. 不改动本规格未列出的行为；每项验证点必须逐条核对。

---

## 1. 审计结论总表

| 编号 | 等级 | 问题 | 证据 | 影响 |
|------|------|------|------|------|
| F1 | **P0** | 移动端抽屉无遮罩、点空白不关闭 | app.css:54 全局 `#overlay{display:none}`，269 行 nav-open 未覆盖 display | 抽屉打开时背景可交互，误触 |
| F2 | **P0** | 平板档（640-799px）☰ 汉堡点击无反应 | `body.nav-open #nav{display:grid}` 仅写在 <640 块（app.css:268），`#menu-btn` 在 <800 全显示（app.css:255 只在 ≥800 隐藏） | 平板无法打开会话列表 |
| F3 | **P0** | `#dirpicker` 双节点重复 id | index.html:58 静态空节点 + dirpicker.js:27-50 再建同 id 并 appendChild | DOM 重复 id；静态节点永不清理 |
| F4 | P1 | composer 第二行布局：textarea 被 #wait 挤压；窄屏 `input{width:100%}` 破坏 workdir+📁 同行 | index.html:49-53 结构；app.css:283 | 移动端 📁 独占一行；流式时输入框被压 |
| F5 | P1 | 轨迹面板不随流式刷新、切换已打开会话不刷新 | trajectory.js 仅 toggleTrajectory/loadReplay 完成时刷新（app.js:565）；events.js:243-248 无刷新钩子 | 面板开着时新步骤不出现；切视图显示旧轨迹 |
| F6 | P1 | 窄屏 header 仍显示超长 `会话 <32位id>` | store.js:43；app.css 窄屏块未隐藏 #session-id | 挤占 tokens/按钮空间 |
| F7 | P1 | 刘海屏（viewport-fit=cover）header 被状态栏遮挡 | index.html:5 有 viewport-fit=cover；header 无 safe-area-inset-top | notch 设备顶栏内容不可见 |
| F8 | P1 | 桌面开轨迹面板时 #to-bottom 被面板覆盖不可点 | app.css:100 `right:16px; z-index:2` < 面板 z-30（:167） | 长对话回底按钮失效 |
| F9 | P1 | 平板开轨迹面板时左侧出现 240px 空洞 | 640-799 档 `#layout` 仍是 2 列 grid + trajectory-open 变 3 列，但 #nav 该档为 fixed 抽屉不占格 | main 左侧空白 |
| F10 | P1 | 触摸目标过小：`.sess-del`、`.dp-tree-item` | app.css:66（12px 字号无尺寸）、:198（4px 8px padding） | 移动端误触/难点 |
| F11 | P1 | dirpicker fetchDirs 无竞态保护 | dirpicker.js:59-87 快速点击两个目录慢响应覆盖快响应 | 目录列表错乱 |
| F12 | P1 | 桌面用户消息未右对齐（决策 #12 未落地） | app.css:76 `.ev` 统一 900px；仅窄屏 281 行右对齐 | 用户/助手消息无对话方向感 |
| F13 | P2 | confirmInline 全内联 cssText，违反样式集中规约 | app.js:343-356 | 样式不可主题化/难维护 |
| F14 | P2 | 无 `theme-color` meta（移动端浏览器 chrome 配色） | index.html:3-8 | 地址栏与应用底色割裂 |
| F15 | P2 | 滚动链穿透：#events 滚到头带动整页橡皮筋 | 无 overscroll-behavior | iOS 手感差 |
| F16 | P2 | 安卓点击高亮闪蓝框 | 无 -webkit-tap-highlight-color | 观感廉价 |
| F17 | P2 | 登录表单 <340px 屏溢出 | app.css:122 `width:320px` 固定 | 小屏横向滚动 |
| F18 | P2 | sticky header + backdrop-filter 在内部滚动布局下无效 | app.css:37-39（#events 是滚动容器，内容不流经 header 下方） | 冗余声明（保留无害，本规格注明不动） |

> 以下**确认无问题**（审计过、不改动）：双主题令牌完整；`:focus-visible` 已有（:33）；空态已有（:97 + app.js:519）；usage-bar CSSOM 宽度合法；running 点与删除钮不同行不重叠；trajectory 窄屏 fixed 全宽（:287）；`#prompt` 16px 防 iOS 缩放（:105）；safe-area-bottom（:104）。

---

## 2. 逐项修复规格

### F1+F2（P0，同一重构）抽屉遮罩与平板档修复

**根因**：RESPONSIVE 段把抽屉规则拆在 <640 块；全局 `#overlay{display:none}` 无人覆盖。

**改动**（`app.css` RESPONSIVE 段重构，替换现有 249-288 行）：

```css
/* ==== RESPONSIVE ==== */

/* Drawer (<800px): sidebar as fixed overlay + real mask */
@media (max-width: 799px) {
  #nav { position: fixed; left: 0; top: var(--header-top); bottom: 0; width: var(--sidebar-width);
         z-index: 20; display: none; background: var(--bg);
         box-shadow: var(--shadow-modal); }
  body.nav-open #nav { display: grid; grid-template-rows: auto auto 1fr; }
  body.nav-open #overlay { display: block; position: absolute; inset: 0;
         background: rgba(0,0,0,.5); z-index: 15; }
  #version-badge { display: none; }
}

/* Desktop ≥800px: nav always visible */
@media (min-width: 800px) {
  #nav { display: grid; grid-template-rows: auto auto 1fr; }
  #overlay { display: none; }
  #menu-btn { display: none; }
}

/* Narrow-only (<640px) */
@media (max-width: 639px) {
  #layout { display: flex; }
  header { padding: var(--space-sm) var(--space-md); gap: var(--space-sm); }
  header h1 { font-size: 14px; }
  #session-id { display: none; }          /* F6 */
  #model-badge, #traj-btn { display: none; }
  #logout { font-size: 0; } #logout::after { content: "⏻"; font-size: 18px; }
  #session-tokens { max-width: 90px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  #events { padding: var(--space-sm); }
  .ev { max-width: 100%; }
  .ev.user { max-width: 90%; margin-left: auto; }
  #model { width: 100%; flex: none; }     /* F4 窄屏部分 */
  #workdir { min-width: 0; }              /* 允许与 📁 同行 */
  .modal-panel { width: 100vw; max-height: 100dvh; border-radius: 0; }
  .trajectory-panel { position: fixed; right: 0; top: var(--header-top); bottom: 0;
         width: 100vw; grid-area: unset; }
  #layout.trajectory-open #to-bottom { display: none; }  /* F8 窄屏：面板全宽盖住按钮 */
}
```

要点：
- **删除**原 252-256 桌面块、259-261 平板块、264-288 窄屏块，以上述三块替代。
- `--header-top` 新令牌（配合 F7，见下）：`:root` 加 `--header-top: calc(52px + env(safe-area-inset-top, 0px));`
- 平板档（640-799）现在继承 drawer 块：☰ 可用、遮罩显示、version 隐藏。
- 窄屏 `#session-id` 隐藏（F6）。

**验证**：① 375px 宽 DevTools：☰ 打开抽屉出现半透明遮罩、点遮罩/Escape 关闭；② 700px 宽：☰ 打开抽屉正常；③ ≥800px：无 ☰、侧栏常驻、无遮罩元素占位。

### F3（P0）dirpicker 复用静态节点

**改动**（`dirpicker.js` buildDOM，替换 25-57 行开头部分）：

```js
function buildDOM() {
  if (pickerEl) return;
  // F3: reuse the static empty shell in index.html instead of creating a duplicate id
  pickerEl = document.getElementById("dirpicker");
  if (!pickerEl) {
    pickerEl = document.createElement("div");
    pickerEl.id = "dirpicker";
    pickerEl.className = "modal";
    document.body.appendChild(pickerEl);
  }
  pickerEl.hidden = true;
  pickerEl.innerHTML = `...原模板不变...`;
  // ...原事件绑定不变...
}
```

**验证**：打开选择器后 `document.querySelectorAll("#dirpicker").length === 1`。

### F4（P1）composer 行布局

**改动 1**（`index.html` 第二行加类）：
```html
<div class="composer-row main-row">
```

**改动 2**（`app.css` composer 段追加）：
```css
.composer-row.main-row { justify-content: flex-end; align-items: flex-end; }
#prompt { flex: 1 1 100%; resize: none; min-height: 60px; max-height: 200px; }
#wait { align-self: center; margin-right: var(--space-sm); }
#send { flex: none; }
```
（`#prompt` 原 106 行的 resize/min-height 合并入新声明，删除旧 106 行避免重复。）

**验证**：① 桌面：prompt 占满一行，wait+发送在下一行右对齐；② 375px：workdir 与 📁 同行、model 独占一行；③ 流式时 prompt 不被 wait 挤压。

### F5（P1）轨迹面板增量刷新

**改动 1**（`events.js` 顶部 import 追加）：
```js
import { refreshPanel } from "./trajectory.js";
```

**改动 2**（`events.js` `case "step_usage"` 分支末尾追加，246-247 行同步之后）：
```js
if (!document.getElementById("trajectory-panel")?.hidden) refreshPanel();
```
（仅在 step_usage 刷新——每步一次，tool_use 每步多条不触发，避免抖动。）

**改动 3**（`trajectory.js` refreshPanel 保持已展开卡片，替换 46-51 行）：
```js
export function refreshPanel() {
  ensurePanel();
  const view = _activeViewGetter ? _activeViewGetter() : null;
  if (!view || !view.trajectory) { bodyEl.textContent = ""; return; }
  const openSteps = new Set(
    [...bodyEl.querySelectorAll("details.trajectory-step[open]")].map(d => d.dataset.step)
  );
  renderBody(view);
  for (const d of bodyEl.querySelectorAll("details.trajectory-step")) {
    if (openSteps.has(d.dataset.step)) d.open = true;
  }
}
```
（同时删除 getAllViews/window.__activeView 死代码 118-127 行，`_activeViewGetter` 已是模块内变量。）

**改动 4**（`app.js` openSession，511 行 ensureEmptyState 之后追加）：
```js
refreshPanel(); // 切换视图后刷新轨迹面板（面板可能正开着）
```

**验证**：① 打开面板后发起带工具的对话：新 Step 卡片逐步出现；② 手动展开某步卡片，等下一步到达后该卡片仍展开；③ 切换到另一会话，面板内容变为该会话轨迹。

### F7（P1）刘海屏安全区

**改动**（`app.css`）：
`:root` 追加令牌（并入 F1 的 `--header-top`）；header 规则（37 行）改为：
```css
header { display: flex; align-items: center; gap: 12px; padding: 0 var(--space-lg);
  padding-top: env(safe-area-inset-top, 0px); height: var(--header-top); ...其余不变... }
```
（box-sizing:border-box 下 height 含 padding；桌面 env=0 无变化。）

**验证**：Chrome DevTools 设备模拟 iPhone 14 Pro（notch）：header 内容不被圆角遮挡。（无 notch 环境则目测 height 计算值 = 52px。）

### F8（P1）to-bottom 与轨迹面板

已在 F1 窄屏块处理（面板打开时隐藏）。桌面补充（`app.css` 追加）：
```css
#layout.trajectory-open #to-bottom { right: calc(16px + var(--trajectory-width) + 8px); }
```

**验证**：① 桌面开面板：按钮右移至面板左侧仍可点；② 375px 开面板：按钮消失，关面板恢复。

### F9（P1）平板档轨迹面板 overlay 化

**改动**（`app.css` 在 drawer 块之后追加）：
```css
@media (min-width: 640px) and (max-width: 799px) {
  .trajectory-panel { position: fixed; right: 0; top: var(--header-top); bottom: 0;
    width: min(360px, 60vw); grid-area: unset; z-index: 30; box-shadow: var(--shadow-modal); }
}
```

**验证**：700px 宽开轨迹：面板右侧贴边浮层，主区无 240px 空洞。

### F10（P1）触摸目标

**改动**（`app.css`）：
```css
.sess-item .sess-del { position: absolute; top: 2px; right: 2px; width: 32px; height: 32px;
  display: grid; place-items: center; padding: 0; font-size: 12px; color: var(--muted); cursor: pointer; }
```
（替换 66 行；running 点 :70 的 `right:22px` 相应改 `right:38px` 避让。）
drawer 块内追加：
```css
@media (max-width: 799px) { .dp-tree-item { padding: 10px 12px; } }
```

**验证**：移动端点 ✕ 删除与目录行均易命中；running 点与 ✕ 不重叠。

### F11（P1）dirpicker 竞态

**改动**（`dirpicker.js`）：
模块级加 `let fetchSeq = 0;`；fetchDirs 开头 `const seq = ++fetchSeq;`；`const data = await r.json();` 之后立即 `if (seq !== fetchSeq) return;`（错误分支 `treeEl.textContent = j.error...` 前同样判断）。

**验证**：快速连点两个目录，列表最终显示最后一次点击的目录。

### F12（P1）桌面用户消息右对齐

**改动**（`app.css` 消息卡段，78 行替换）：
```css
.ev.user { border-left: 3px solid var(--accent); max-width: 720px; margin-left: auto; }
```
（窄屏块已有 `max-width:90%; margin-left:auto` 覆盖，不冲突。）

**验证**：桌面用户消息靠右、宽 ≤720px；助手/result 仍靠左 900px。

### F13（P2）confirmInline 样式类化

**改动 1**（`app.css` 追加）：
```css
.confirm-overlay { position: fixed; inset: 0; background: rgba(0,0,0,.5); z-index: 1000;
  display: flex; align-items: center; justify-content: center; }
.confirm-box { background: var(--panel); color: var(--fg); border: 1px solid var(--border);
  border-radius: var(--radius-lg); padding: 20px; max-width: 320px; width: 90%; box-shadow: var(--shadow-modal); }
.confirm-box p { margin: 0 0 16px; }
.confirm-row { text-align: right; }
.confirm-ok { padding: 6px 16px; background: var(--err); color: #fff; border: none; border-radius: var(--radius-sm); cursor: pointer; }
.confirm-cancel { padding: 6px 16px; background: transparent; color: var(--fg); border: 1px solid var(--border);
  border-radius: var(--radius-sm); cursor: pointer; margin-right: 8px; }
```
**改动 2**（`app.js` confirmInline 339-379）：删除全部 `style.cssText`，改为 `overlay.className = "confirm-overlay"`、`box.className = "confirm-box"`、`p` 删样式、`btnOk.className = "confirm-ok"`、`btnCancel.className = "confirm-cancel"`、`row.className = "confirm-row"`。Promise 逻辑不变。

**验证**：删除会话确认弹窗样式与原先一致（主题变量驱动）。

### F14（P2）theme-color

**改动 1**（`index.html` head 追加）：
```html
<meta name="theme-color" content="#0d1117">
```
**改动 2**（`app.js` applyTheme 内追加）：
```js
const meta = document.querySelector('meta[name="theme-color"]');
if (meta) meta.content = getComputedStyle(document.documentElement).getPropertyValue("--bg").trim();
```

**验证**：切换暗/亮主题后移动端浏览器地址栏底色跟随。

### F15+F16+F17（P2）滚动/高亮/登录

**改动**（`app.css`）：
```css
* { box-sizing: border-box; -webkit-tap-highlight-color: transparent; }   /* F16：并入 22 行 */
#events, #session-list, .trajectory-body, .modal-body, .dp-tree { overscroll-behavior: contain; }  /* F15 */
#login-form { display: flex; flex-direction: column; gap: 12px; width: min(320px, calc(100vw - 32px)); ... }  /* F17：122 行 width 替换 */
```

### F18（P2）说明性条目（不改动）

sticky/backdrop-filter 当前无效果（#events 内部滚动、内容不经 header 下方）。**保留不动**：无害，且若未来改 overlay 滚动即生效。记录在案防止误判为 bug。

---

## 3. 文件改动清单

| 文件 | 涉及项 |
|------|--------|
| `app.css` | F1/F2（RESPONSIVE 重构+令牌）、F4、F7、F8、F9、F10、F12、F13、F15/F16/F17 |
| `index.html` | F4（main-row 类）、F14（meta） |
| `dirpicker.js` | F3、F11 |
| `events.js` | F5（import + step_usage 刷新） |
| `trajectory.js` | F5（refreshPanel 保展开态 + 删死代码） |
| `app.js` | F5（openSession 刷新）、F13（confirmInline 类化）、F14（applyTheme meta） |

## 4. 实施顺序（3 个独立 commit，各自 `make verify` + `node --check`）

1. **commit 1（P0）**：F1+F2（含 `--header-top` 令牌）、F3 —— 三处功能性 bug。
2. **commit 2（P1）**：F4、F5、F7、F8、F9、F10、F11、F12。
3. **commit 3（P2）**：F13、F14、F15、F16、F17。

## 5. 验收矩阵

| 场景 | 375px（窄） | 700px（平板） | 1280px（桌面） |
|------|------------|--------------|---------------|
| ☰ 抽屉 | 遮罩+关闭 ✓（F1） | 可打开 ✓（F2） | 无 ☰ ✓ |
| header | 无长 id、tokens 截断（F6） | version 隐藏 | 全量显示 |
| composer | workdir+📁 同行、model 独行、prompt 整行（F4） | 同左 | wait/发送右对齐 |
| 用户消息 | 90% 右对齐 | 720px 右对齐（F12） | 720px 右对齐 |
| 轨迹面板 | 全宽覆盖、to-bottom 隐藏（F8） | 浮层无空洞（F9） | 三栏、按钮右移（F8） |
| 流式刷新 | 新 Step 卡片出现、展开态保持（F5） | 同左 | 同左 |
| 触摸 | ✕/目录行易点（F10）、无蓝框（F16） | 同左 | — |
| 主题 | 地址栏跟随（F14）、双主题无未定义 var | 同左 | 同左 |
| 回归 | `make verify` 全绿、`node --check` 全绿、`#dirpicker` 唯一 | | |
