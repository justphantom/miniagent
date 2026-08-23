## 最高约束
- 有话直说，只讲重点，像发电报一样一个字都不要多给
- 只做明确要求的：隐含需求先确认，每行改动可溯源，不擅自引入 CI/容器/监控/APM 等运维脚手架
- 不确定就问；多解释全部呈现，不替用户决定
- 标准库优先；第三方需说明理由及最小用法
- 节制抽象：函数单一职责，不预建接口/基类/工厂，重复 <3 处不抽
- 错误直接返回标准库类型，不自定义（除非语义不足）
- 功能必须有标准库测试，用例即需求文档
- 单个代码文件行数不超过300行（`_test.go` 豁免：测试按场景聚合，允许超）
- 注释只写"为什么"，且仅非直观或特殊约定时
- 所有二进制文件只能保存在bin目录
- Commit：subject ≤72 字符、祈使、无句号，一次一事
- 纳入版本跟踪的文件中不可引用未纳入版本跟踪文件的任何内容
- 改后必跑全绿 verify-gate：`gofmt -s -l .`（空）/ `go build ./...` / `go vet ./...` / `go test -race ./...` / `golangci-lint run ./...` / 非 `_test.go` 文件 ≤300 行
- 回复 ≤1500 字符

## 路由
- 会话启动：读 `.agent/L0/`（永久约束：角色/架构不变量/流程策略，每次加载）
- 架构/钩子契约/不变量时序：读 `ARCHITECTURE.md` §4–§5 与 `HOOKS.md`
- 历史决策/陌生报错/选型：先查 `.agent/L2/` 再猜
- 当前会话上下文/任务进展：`.agent/L1/active/session.md`（L1 单会话文件）；跨会话任务先读 `.agent/L1/active/carryover.md`
- **检索反馈闭环**：读取 L2 条目后，在 L1 session.md 记录 `retrieved: <path> confidence: <high/medium/low>`。低置信度（`medium` 或 `low`）须向用户呈现候选列表 + 请求确认，不得自行猜测。多次检索同一主题无稳定命中 → 创建新 L2 条目并标记 `confidence: evolving`
- **检索失败结构化兜底**：L2 无匹配或匹配置信度低 → 向用户呈现「检索结果 + 候选方向 + 请求确认」，不得自行假设

## .agent 记忆体系

### 分层结构
`.agent/` 目录按三层组织项目级 Agent 记忆，本文件是体系总纲，具体记忆内容存于各层文件中：

| 层 | 路径 | 性质 | 加载时机 |
|---|------|------|---------|
| **L0** | `.agent/L0/` | 永久约束（角色/架构不变量/流程策略） | 每次会话必加载 |
| **L1** | `.agent/L1/active/session.md` + `.agent/L1/active/carryover.md` | 单会话过程上下文 + 跨会话交接单 | 当前任务追踪；新会话从 carryover 恢复上下文 |
| **L2** | `.agent/L2/` | 经验教训与可复用知识 | 按需检索 + 检索反馈闭环 |

#### L0 — 永久约束（`.agent/L0/`）
- `constraints.md` — 交互红线、架构不变量（核心零策略/工具配对/session 标记/thinking 钉死/NDJSON 契约等）、记忆系统元规则
- `persona.md` — 角色定位（miniagent = 极简无内置策略 ReAct 核心库 + CLI）、风格约定
- `policies.md` — 流程策略（verify-gate/config-first/钩子红线/CHANGELOG/改动落点/记忆闭环）

#### L1 — 过程上下文（`.agent/L1/active/session.md`）
- 单会话工作内存，非历史档案
- 只保留「当前任务 + 本轮极简摘要（每条 ≤2 行）」
- 已完成且无复用价值的条目不保留；有沉淀价值的提炼进 L2 后从 session.md 删除

#### L2 — 经验教训（`.agent/L2/`）
- `patterns/` — 可复用的设计模式与实现套路（压缩体系/三态裁决/记忆反过期/对抗评审等）
- `decisions/` — 架构决策记录 ADR（核心零策略/多 provider 分派/WebUI 架构等）
- `incidents/` — 事故复盘（jsonl 持久化/SSE 鲁棒性/thinking 降级/钩子红线等）

### 内容更新规则
1. Agent 与用户均可读写 `.agent/` 下文件。
2. L0 更新需用户明确授权或手动编辑（`constraints.md` §17）。
3. 文件使用 Markdown + YAML frontmatter（含 `layer`/`type`/`tags`/`created` 等字段）。
4. 可复用经验沉淀到 L2，新建前先查 `.agent/index.md` 避免重复。
5. 引用只用已纳入版本跟踪的路径（`internal/*`、`cmd/*`、commit 等）；未跟踪路径（如 `docs/`）内联说明，不作为依赖。
6. 检索优先用精确关键词与标签，必要时辅以语义搜索。
7. L1 session.md 是单会话工作内存：任务完成后删历史流水账条目，只留当前任务 + 极简摘要。已完成且无复用价值的不保留；有沉淀价值的提炼进 L2 后从 session.md 删除。
8. **跨会话交接**：若任务预计跨多会话（多天），在当前会话结束前写入 `.agent/L1/active/carryover.md`，格式：`## 任务名` → `### 已完成`（自由文本）→ `### 待办`（列表）→ `### 关键上下文`（引用 L2 条目 + 关键决策）。新会话启动时若 carryover.md 存在，先读后清零。
9. **检索反馈记录**：每次 L2 检索后在 session.md 追加 `retrieved: path/to/entry.md confidence: <high/medium/low>`。低置信度条目触发用户确认流程（见路由节）。
10. **L2 生命周期**：`confidence` 字段（`high`/`medium`/`low`/`evolving`）标记条目可信度。`evolving` 表示新条目待验证。`status: superseded` 条目自动从索引优先级降级。verify-gate 中检查 superseded 条目引用的代码路径有效性（grep 确认仍存在）。

### 索引说明
- **L0**：每次会话启动时自动加载；路由见本章「路由」节。
- **L1**：仅 `active/session.md` 单文件，任务进展统一追踪于此。
- **L2**：按主题分 `patterns/`/`decisions/`/`incidents/`，精确索引见 `.agent/index.md`（含所有 L2 条目分类清单与关键词标签）；检索优先关键词。
- **L2 写入规则**：新建条目先查 `.agent/index.md` 避免重复；使用 Markdown + YAML frontmatter；结构为现象/背景 → 根因/理由 → 做法 → 参考；参考段只引已版本跟踪路径；与代码现状不一致时更新条目并记 `updated`。

### 记忆系统元规则
- 遇历史决策/陌生报错/选型时的检索路由由本章「路由」节定义，L0 不重复。
- 版本跟踪引用纪律由本章「最高约束」第 12 条定义，L0 不重复。
- L0 更新需用户显式授权或手动编辑。