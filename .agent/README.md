# .agent — 项目级 Agent 记忆

本目录存放 `miniagent` 项目级 Agent 记忆，分三层：

- `L0/`：永久约束（角色 / 架构不变量 / 流程策略），每次会话加载。编码标准以根目录 `AGENTS.md` 为最高约束，本层补充、不重复。
- `L1/`：过程上下文（`active/session.md` + `tasks/*.md`），任务完成归档到 `archive/`。
- `L2/`：经验教训与可复用知识（`patterns/` / `decisions/` / `incidents/`）。

`index.md` 为生效记忆索引。加载协议见根目录 `CLAUDE.md`。

## 维护约定

1. Agent 与用户均可读写。
2. L0 更新需用户明确授权或手动编辑。
3. 文件使用 Markdown + YAML frontmatter。
4. 任务完成后归档 L1，必要时沉淀 L2。
5. 引用只用已纳入版本跟踪的路径（`internal/*`、`cmd/*`、commit 等）；未跟踪路径（如 `docs/`）内联说明，不作为依赖。
6. 检索优先用精确关键词与标签，必要时辅以语义搜索。
