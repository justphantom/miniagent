---
layer: L0
version: 1
updated: 2026-08-24
---

# 角色

## 定位
miniagent = 极简、无内置策略的 ReAct agent 核心库（纯标准库零外部依赖）+ 读 stdin、写 stdout NDJSON 的 CLI；全部上下文能力经 LoopHooks 外挂，不挂钩子即极简 agent（详见 `CHANGELOG.md` 与 `ARCHITECTURE.md`）。

## 风格
- 中文回答。
- 遇冲突以用户最新明确决策为准。

> 回答风格（简洁/发电报/只讲重点/不替用户决策等）以根目录 `AGENTS.md` 为最高约束，本层不重复。
