---
layer: L2
type: incident
tags: [webui, layout, css, display, inline-style, grid, app-shell]
created: 2026-08-25
confidence: high
---

# app-shell 内联 display 覆写 grid：结构骨架与显示开关不同源

## 现象
v2 三行骨架部署后页面横向坍塌：header / 主区 / footer 三子块横排。用户见「排版与预期不一致」。

## 根因
- `app.css` 把 `#app` 由 `flex column` 改为 `display:none + grid-template-rows` 三行骨架；JS `showApp()` 仍残留 v1 的内联 `el.style.display="flex"`。
- CSS 级联优先级：**内联 style > 样式表任何选择器** → `display:flex` 生效，`grid-template-rows` 架空；且 `#app` 已无 `flex-direction:column` 兜底，回落默认 `row` → 三子块横排。
- 副因：`#layout { flex:1 }` 本是为 flex 写的死代码，在 flex-row 事故中"意外复活"独吞宽度。
- 流程教训：v2 验收只人工走查视觉效果，未核对 JS 是否残留内联 display——**结构骨架改动与显示开关改动必须同 PR 同查**。

## 做法（已修复）
- 显示切换改 **class 驱动**：`#app.on`（grid）/`#login.on`（flex），JS 永不再写 `#app` 内联 display；`showLogin/showApp` 只切 `.on` 类。
- 顺带修窄屏 main 无 `flex:1`、`.cfg-*` 规则覆盖、状态栏模型首用恒空、`#layout` 死 `flex:1` 清理等（过程文档 docs/WEBUI_LAYOUT_ROOTCAUSE.md 已删，git 历史可查）。
- 验收：`getComputedStyle($("app")).display === "grid"`。

## 可复用经验
1. **CSS 内联 style（HTML `style=` 属性或 JS `el.style.*`）优先级高于一切样式表规则**——单测/人工走查验证 `display`/骨架类属性时必须查 JS 是否残留内联赋值，仅看 CSS 不够。
2. **骨架/布局体系重构时，把「显示/隐藏开关」与「结构定义」做成同源（同一组 class）**，杜绝两处各自为政。

## 参考
- `cmd/miniagent/webstatic/static/app.css`（`#app.on` / `#login.on`）
- `cmd/miniagent/webstatic/static/app.js`（showLogin/showApp classList 切换）
- commits：三行骨架恢复 1216a10（根因修复）、v2 骨架 5dcd277（引入）