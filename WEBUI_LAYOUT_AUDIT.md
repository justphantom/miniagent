# WebUI 排版审查与修复方案（v2 部署后回归）

基线：`5dcd277`。审查范围：三行骨架 + 配置模态全部改动文件 + 部署链路。
审查方法：逐行核对 `index.html`/`app.css`/`app.js`/`config.js`/`store.js` 与 `web.go` 静态服务实现。

## §0 根因速览

**P0-A：`showApp()` 内联 `display:flex` 覆盖三行 grid，页面横向排列。**

证据链：
- app.css:37 `#app { display: none; grid-template-rows: var(--header-top) minmax(0,1fr) auto; }` —— v2 把 `#app` 从 flex 改为 grid 三行。
- app.js:43 `$("app").style.display = "flex";` —— v1 时代写的内联样式未同步；内联优先级高于样式表。
- 致命点：v1 的 `#app` CSS 有 `flex-direction: column`，v2 改 grid 时删掉了；内联 `display:flex` 生效后 **flex-direction 回落默认 `row`** → header / #layout / footer 三个子元素横向并排：header 被挤成内容宽度竖条，主区居中，composer+状态栏被挤到右侧一列。

这就是部署后「排版和预期不一致」的直接原因。静态资源 `Cache-Control: no-store`（web.go:196）已排除浏览器缓存因素。

## §1 发现清单（按严重度）

| # | 级别 | 问题 | 证据 |
|---|------|------|------|
| A | P0 | `#app` 内联 flex 覆盖 grid，三行骨架横向坍塌 | app.js:43 vs app.css:37 |
| B | P0 | 窄屏 `<640px` `#layout{display:flex}` 下 main 无 `flex:1`，composer 移出后 main 仅剩 #events，宽度收缩不可靠 | app.css:307 |
| C | P1 | `#config-page` 重置规则（app.css:203）被同选择器原规则（app.css:229）覆盖——同特异性后者胜，模态内配置内容仍 720px 居中+16px padding | app.css:203 vs 229 |
| D | P1 | 状态栏模型首次使用（无 localStorage）时恒空——`if (saved)` 守卫跳过，`setStatusModel` 不被调用 | app.js:436 |
| E | P2 | `#layout { flex: 1 }` 在 grid 行中是无效死代码（minmax(0,1fr) 已接管） | app.css:47 |
| F | P2 | `#login, #app { display: none }`（app.css:135）与 `#app` 骨条规则（app.css:37）重复声明 display | app.css:37,135 |
| G | 部署 | embed 静态资源编译进二进制，部署必须 `make build` 重编+替换+重启；只改 static/ 文件不重编则线上恒为旧版 | webstatic/assets.go:12 |

## §2 修复规格

### 2.1 修复 A（P0）——显示切换改类驱动，JS 不再写内联 display

**app.css**：显式开态类，消灭内联猜测：

```css
#app { display: none; grid-template-rows: var(--header-top) minmax(0, 1fr) auto; height: 100dvh; }
#app.on { display: grid; }
#login.on { display: flex; }
```

（app.css:135 `#login, #app { display: none; }` 删除——冗余，见 F。）

**app.js:41-43**：

```js
function showLogin() { $("login").classList.add("on"); $("app").classList.remove("on"); $("key-input").focus(); }
function showApp() {
  $("login").classList.remove("on");
  $("app").classList.add("on");
  ...
```

### 2.2 修复 B（P0）——窄屏 main 撑满

```css
@media (max-width: 639px) {
  #layout { display: flex; }
  main { flex: 1; min-height: 0; }   /* 新增 */
  ...
```

### 2.3 修复 C（P1）——合并重复规则

删除 app.css:203 处的 `#config-page` 重置，改写 app.css:229 原规则为模态语境：

```css
#config-page { padding: 0; max-width: none; margin: 0; }
```

（原 720px 居中是整页时代的约束，模态内由 `.modal-body` 的 padding 承担。）

### 2.4 修复 D（P1）——状态栏模型兜底

app.js `loadModels` 循环后：

```js
setStatusModel(saved || "默认模型");
```

（替换 `if (saved) setStatusModel(saved);`；无记忆时显示服务端默认，与下拉框首项语义一致。）

### 2.5 修复 E/F（P2）——死代码清理

- app.css:47 `#layout` 删 `flex: 1`（grid 行已由 `minmax(0,1fr)` 定义）。
- app.css:135 删 `#login, #app { display: none; }`（#app 在 37 行已声明 none；#login 由 2.1 的 `.on` 类接管）。

### 2.6 部署核查（G）

非代码项，写入部署流程：
1. `make build` 重编二进制（embed 要求）。
2. 替换部署机二进制 + `systemctl restart`。
3. 验证：浏览器 DevTools → Network → `app.css` 响应含 `grid-template-rows`；或查看页面 footer 状态栏出现版本号即为新版。

## §3 修复后预期布局（回归基线）

```
┌──────────────────────────────────────────────────────────────┐
│ header（grid 行1，固定）                                      │
├──────────┬────────────────────────────────────┬──────────────┤
│ 侧栏      │ #events（唯一滚动区）              │ 轨迹         │
├──────────┴────────────────────────────────────┴──────────────┤
│ composer（grid 行3，通栏固定）                                │
│ 状态栏：vX.Y.Z · provider/model                              │
└──────────────────────────────────────────────────────────────┘
```

## §4 文件改动清单

| 文件 | 改动 |
|------|------|
| `static/app.css` | `.on` 类 2 条；删 `#login,#app` 重复声明；窄屏 `main{flex:1}`；`#config-page` 规则合并；删 `#layout` 死 `flex:1` |
| `static/app.js` | showLogin/showApp 改类切换；`setStatusModel` 兜底 |

仅 2 文件。零 Go 变更。

## §5 验收 DoD

1. **P0 回归**：登录进入后 header 在顶、footer 在底、主区居中——无横向并排；`getComputedStyle($("app")).display === "grid"`。
2. 375px 窄屏：主区撑满宽度，对话卡片无收缩。
3. 配置模态内表单横向铺满 body（无 720px 居中留白）。
4. 清空 localStorage 首次进入，状态栏显示「默认模型」。
5. 三断点走查 + verify-gate 全绿 + `node --check`。

## §6 提交计划（1 commit）

`web: fix app shell display regression and modal config width`

单 commit 即可：改动集中于 2 文件且同属「v2 部署回归修复」一事。
