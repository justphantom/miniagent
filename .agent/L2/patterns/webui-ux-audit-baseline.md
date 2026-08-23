---
layer: L2
type: pattern
tags: [webui, audit, layout, ide, archive]
created: 2026-08-24
confidence: medium
---

# WebUI UX 审计基线（IDE 骨架 · 576989f 起）

## 现象
第四轮复审重立审计基线：前身 `WEBUI_RESPONSIVE_UX.md`（基线 8e517fd，F1-F18）基于旧布局体系（`#nav`/`#overlay`/`nav-open` 抽屉），被 `651711e` IDE 重构整体作废——file:line 与选择器全部失配，照单实施会改错文件。

## 根因
布局体系整体替换后，逐条 file:line 级审计文档寿命 ≤ 一次大重构。过程文档须随基线重写，不可增量修补。

## 做法
- 修复合入清单（截至 576989f）：桌面 header 回归修复合入（1216a10）、响应式细节 F4/F5/F7/F10/F11 + P2 全档（7bf31da）、spec 缺口（8e517fd）、滚动条统一 349a541、app-shell 显示回归 1216a10、会话列表横滚 1211853、lag-cut 自愈 3c7025d。
- 未修观察项（非阻塞，实施前需以当前基线重核）：F2 平板档汉堡（旧体系结论，可能已随 IDE 重构消失）、F6 窄屏 header 长 id、F15 iOS 滚动链穿透（无 overscroll-behavior）。
- 布局现状：`.app-shell` 三列 grid（rail/main/panel），断点 800px/640px，panel 窄屏转 fixed 抽屉。
- 过程文档归档位置：仓库 git 历史（14 个 WEBUI_*/SSE_* 文档于 2026-08-24 从根目录删除，历史可查）；`.gitignore` 含 `docs/`，磁盘 docs/ 目录不跟踪。

## 参考
- 布局定义 `cmd/miniagent/webstatic/static/app.css`（.app-shell 断点）
- 入口 `cmd/miniagent/webstatic/static/app.js` / `views.js` / `trajectory.js` / `usage.js` / `dirpicker.js`
- commit 576989f（审计基线锚点）、651711e（IDE 重构）、7bf31da（前代修复）
