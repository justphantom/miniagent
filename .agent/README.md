# .agent — 项目级 Agent 记忆

本目录存放 `miniagent` 项目级 Agent 记忆，分三层：

- `L0/`：永久约束（角色 / 架构不变量 / 流程策略），每次会话加载。编码标准以根目录 `AGENTS.md` 为最高约束，本层补充、不重复。
- `L1/`：过程上下文，仅 `active/session.md` 单文件（任务追踪统一于此）。
- `L2/`：经验教训与可复用知识（`patterns/` / `decisions/` / `incidents/`）。

`index.md` 为生效记忆索引。加载协议见根目录 `AGENTS.md`「路由」。

## 维护约定

1. Agent 与用户均可读写。
2. L0 更新需用户明确授权或手动编辑。
3. 文件使用 Markdown + YAML frontmatter（必填字段见 `L2/schema.md`）。
4. 可复用经验沉淀到 L2。
5. 引用只用已纳入版本跟踪的路径（`internal/*`、`cmd/*`、commit 等）；未跟踪路径（如 `docs/`）内联说明，不作为依赖。
6. 检索优先用精确关键词与标签，必要时辅以语义搜索。
7. **L1 session.md 是单会话工作内存，非历史档案**：任务完成后删历史流水账条目，只留「当前任务 + 本轮极简摘要（每条 ≤2 行）」。已完成且无复用价值的条目不保留；有沉淀价值的提炼进 L2 后从 session.md 删除。
8. **跨会话接力**：多天任务用 `L1/active/carryover.md` 传递上下文（格式见该文件开头）。
9. **检索反馈闭环**：L2 检索后在 session.md 记 `retrieved: <path> confidence: <high/medium/low>`，低置信度触发用户确认。
10. **记忆自测试**：`make verify` 含 `scripts/verify-memory.sh`，检查 L2 frontmatter 完整性、index 覆盖度、引用有效性。
