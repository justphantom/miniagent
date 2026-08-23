# WebUI 统一滚动条样式方案

基线：`1216a10`。依据：《滚动条分析》（11 个滚动容器盘点）。纯 CSS 改动，零 JS、零 Go。

## §0 硬约束

- 沿用现有设计令牌（`var(--border)` thumb、transparent track），不改色不改风格——只做**统一与补漏**。
- 双引擎覆盖：标准属性（Firefox：`scrollbar-width`/`scrollbar-color`）+ webkit 伪元素（Blink/WebKit）。
- 不引入 overlay/hover 出现等花哨行为；`scrollbar-gutter` 不启用（避免布局位移，现状无此问题反馈）。

## §1 现状问题（分析结论）

| # | 问题 | 位置 |
|---|------|------|
| 1 | 侧栏样式挂错节点：样式在 `#session-list`（不滚动，死规则），真正滚动的 `#nav` 无样式 | app.css:63,66-68 |
| 2 | 模态体系无样式：`.modal-body`（配置模态 85vh 高频滚动）、`.dp-tree`、`.cfg-json textarea` 均默认粗条 | app.css:201,223,273 |
| 3 | 横向滚动条仅 webkit：`.md pre/code` 有 6px 伪元素但无 `scrollbar-width`，Firefox 裸奔；`.md table`/`.ev.tool pre` 完全无样式 | app.css:159-160,95,154 |
| 4 | `#prompt` 封顶 200px 后内部滚动无样式 | app.css:113 |
| 5 | 相同样式块在 `#events`/`#session-list`/`.trajectory-body` 复制 3 份 | app.css:51-53,66-68,184-186 |

## §2 设计方案：单块统辖全部滚动容器

### 2.1 容器分组

**纵向组**（8px）：
```
#events, #nav, .trajectory-body, .modal-body, .dp-tree, .cfg-json textarea, #prompt
```

**横向组**（6px，内容内部横滚）：
```
.md pre, .md code, .md table, .ev.tool pre, .cfg-json textarea, #prompt
```
（textarea/prompt 兼具两轴，同入两组。）

### 2.2 规则块（替换 §1 问题 5 的三份拷贝）

```css
/* ---- unified scrollbars ---- */
/* 纵向组：标准属性（Firefox） */
#events, #nav, .trajectory-body, .modal-body, .dp-tree,
.cfg-json textarea, #prompt {
  scrollbar-width: thin;
  scrollbar-color: var(--border) transparent;
}
/* 纵向组：webkit */
#events::-webkit-scrollbar, #nav::-webkit-scrollbar, .trajectory-body::-webkit-scrollbar,
.modal-body::-webkit-scrollbar, .dp-tree::-webkit-scrollbar,
.cfg-json textarea::-webkit-scrollbar, #prompt::-webkit-scrollbar { width: 8px; }
#events::-webkit-scrollbar-thumb, #nav::-webkit-scrollbar-thumb, .trajectory-body::-webkit-scrollbar-thumb,
.modal-body::-webkit-scrollbar-thumb, .dp-tree::-webkit-scrollbar-thumb,
.cfg-json textarea::-webkit-scrollbar-thumb, #prompt::-webkit-scrollbar-thumb {
  background: var(--border); border-radius: 4px; }

/* 横向组：标准属性 + webkit（高度 6px） */
.md pre, .md code, .md table, .ev.tool pre,
.cfg-json textarea, #prompt {
  scrollbar-width: thin;
  scrollbar-color: var(--border) transparent;
}
.md pre::-webkit-scrollbar, .md code::-webkit-scrollbar, .md table::-webkit-scrollbar,
.ev.tool pre::-webkit-scrollbar, .cfg-json textarea::-webkit-scrollbar,
#prompt::-webkit-scrollbar { height: 6px; }
.md pre::-webkit-scrollbar-thumb, .md code::-webkit-scrollbar-thumb, .md table::-webkit-scrollbar-thumb,
.ev.tool pre::-webkit-scrollbar-thumb, .cfg-json textarea::-webkit-scrollbar-thumb,
#prompt::-webkit-scrollbar-thumb { background: var(--border); border-radius: 3px; }
```

> 说明：`scrollbar-width: thin` 对横竖两轴同时生效（Firefox 无法分轴定宽，可接受）；webkit 分轴由 `width`/`height` 分别控制。重复声明的通用属性合并书写，不引入新令牌。

### 2.3 删除项

| 删除 | 原因 |
|------|------|
| `#events` 上的 scrollbar 三行（app.css:51 内联属性）+ 51-53 伪元素 | 并入 §2.2 |
| `#session-list` scrollbar 三行 + 66-68 伪元素 | 死规则（无 overflow）；侧栏滚动由 `#nav` 接管 |
| `.trajectory-body` scrollbar 内联属性（app.css:184）+ 184-186 伪元素 | 并入 §2.2 |
| `.md code/pre::-webkit-scrollbar` 旧规则（app.css:159-160） | 并入 §2.2 |

### 2.4 顺带补齐（顺手、零风险）

- `.md table`、`.ev.tool pre` 补 `overscroll-behavior: contain;`（防横向滚动链穿透到 #events，与既有容器一致）。
- `.cfg-json textarea` 已有 `overflow: auto`，不动；`#prompt` 由 JS 控高，overflow 走原生默认，不动。

## §3 文件改动清单

仅 `cmd/miniagent/webstatic/static/app.css` 一个文件：
- 新增 §2.2 统一规则块（置于 `/* ---- layout skeleton ---- */` 之后）
- 删除 §2.3 四处旧规则
- §2.4 两行 `overscroll-behavior`

## §4 验收 DoD

1. 侧栏会话列表超长时滚动条为 8px thin 样式（此前为默认粗条）。
2. 配置模态（表单模式滚动 + JSON 模式 textarea 双轴）、目录选择器树滚动条均为 thin 样式。
3. 代码块/表格/工具输出横向滚动条 6px；**Firefox** 下同样为 thin（此前裸默认）。
4. 对话区/轨迹面板滚动条视觉与改动前一致（回归无损）。
5. `#prompt` 输入超 200px 后内部滚动条为 thin。
6. `grep -c "scrollbar" app.css` 规则数下降（3 份拷贝→1 块统辖）；无 `#session-list` scrollbar 残留。
7. verify-gate 全绿（纯 CSS，Go 无感）。

## §5 提交计划（1 commit）

`web: unify scrollbar styling across all scroll containers`

## §6 明确不做

- 不做 hover 才显示/auto-hide 滚动条（macOS overlay 风格）——改动交互预期，需单独确认。
- 不启用 `scrollbar-gutter: stable`——会引入布局预留位移，现状无滚动条抖动反馈。
- 不改滚动行为 JS（stickBottom/jumpToBottom/scrollIntoView）——行为无缺陷报告。
