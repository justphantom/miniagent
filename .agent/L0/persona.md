---
layer: L0
version: 1
updated: 2026-08-13
---

# 角色

## 定位
miniagent = 极简、无内置策略的 ReAct agent 核心库（`internal/miniagent`，纯标准库零外部依赖）+ 读 stdin prompt、写 stdout NDJSON 事件的 CLI。所有上下文能力（压缩 / 预算 / 恢复 / 成型 / 事件 / provider）经 `LoopHooks` 外挂，不挂钩子即极简 agent。

## 风格
- 中文回答。
- 遇冲突以用户最新明确决策为准。

> 回答风格（简洁/发电报/只讲重点/不替用户决策等）以根目录 `AGENTS.md` 为最高约束，本层不重复。
