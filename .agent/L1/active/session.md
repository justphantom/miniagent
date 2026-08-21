---
layer: L1
type: session
updated: 2026-08-21
---

# 当前会话

## 状态
v6.1.0 已发版并推送（tag + main → origin；bin/miniagent -version 验证 v6.1.0）。本轮交付：WebUI 会话删除（DELETE API + ✕ 按钮 + 移动端可见）、vanilla markdown 渲染（XSS 转义、流式累积→一次性渲染）、表格语法、header token 累计、版本号双点展示（登录页+header）、体验全面优化（中断/重试/智能滚动/预览/折叠/复制/暗亮主题/键盘适配）、前端拆 ES modules、CHANGELOG 整理（v6.0.0 段落从 Unreleased 拆出并回填定版、去重 Changed 段）。

## 待办
- 无。工作区干净，与 origin 同步。

## 备注
- 发版发现：v6.0.0 tag 打在 CHANGELOG 未回填的 Unreleased 上，v5.1.0 段缺失；本次已补 [6.0.0] 段（含轮管道库化/移除 provider/库化/loopCfg 重构）。release-checklist 已沉淀此坑（定版前必须同类合并 + tag 前确认 Unreleased 已 pin）。
- verify-gate 全绿；非 _test.go Go 文件均 ≤300 行。
