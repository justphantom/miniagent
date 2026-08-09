# L2 — 经验教训

按主题存放：

- `patterns/`：可复用的设计模式、实现套路与最佳实践。
- `decisions/`：架构决策记录（ADR）。
- `incidents/`：线上 / 本地问题根因、排查过程与修复记录。

## 写入规则

1. 新建条目前先检索已有内容（查 `../index.md`），避免重复。
2. 使用 Markdown + YAML frontmatter，至少包含 `layer`、`type`、`tags`、`created`。
3. 条目结构：现象 / 背景 → 根因 / 理由 → 做法 → 参考。
4. **参考段只引已纳入版本跟踪的路径**（`internal/*`、`cmd/*`、commit hash）；未跟踪路径（如 `docs/`）内联说明，不作为依赖。
5. 与代码现状保持一致；结论过期或代码已改时更新条目并记 `updated`。
