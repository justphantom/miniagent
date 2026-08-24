## 行为红线
- 只做明确要求的；隐含需求先确认，每行改动可溯源；不擅自引入 CI/容器/监控/APM
- 不确定就问，呈现选项与利弊，不替用户决定
- 禁止修改 `.gitignore`；提交前必须给 diff 摘要待审阅
- 回复精简：省略客套、铺垫、总结性复述，直接给结论与要点

## 编码标准
- 标准库优先；第三方需说明理由及最小用法
- 函数单一职责，不预建接口/基类/工厂，重复 <3 处不抽
- 错误返回标准库类型（语义不足才自定义）；功能必有标准库测试，用例即需求文档
- 注释只写"为什么"，仅非直观或特殊约定时；非 `_test.go` 文件 ≤300 行
- 二进制只进 `bin/`；commit subject ≤72 字符、祈使、无句号、一次一事

## 流程
- 改后必跑 `make verify` 全绿（gofmt 空 / build / vet / test -race / lint / 行数上限 / 记忆完整性）
- 纳入版本跟踪的文件不可引用未跟踪文件内容
- API/config/CLI 变更同步 `CHANGELOG.md`（Keep a Changelog + SemVer）

## 路由
- 会话启动：读 `.agent/L0/{persona,constraints,policies}.md`（永久约束，每次加载）
- 架构/钩子契约/不变量时序：读 `ARCHITECTURE.md` §4–§5 与 `HOOKS.md`
- 历史决策/陌生报错/选型：先查 `.agent/L2/` 再猜
- 当前会话上下文：`.agent/L1/active/session.md`；跨会话任务先读 `.agent/L1/active/carryover.md`
- 检索反馈闭环：L2 读取后在 session.md 记 `retrieved: <path> confidence: <high/medium/low>`；低置信度呈现候选+请求确认；同主题多次无稳定命中→建新条目标 `evolving`

## .agent 记忆体系

| 层 | 路径 | 性质 | 加载时机 |
|---|------|------|---------|
| L0 | `.agent/L0/` | 永久约束（角色/架构不变量/流程策略） | 每次会话必加载 |
| L1 | `.agent/L1/active/session.md` + `carryover.md` | 单会话工作内存 + 跨会话交接单 | 当前任务追踪 |
| L2 | `.agent/L2/`（`patterns/`/`decisions/`/`incidents/`） | 经验教训与可复用知识 | 按需检索 |

维护约定见 `.agent/README.md`；L2 schema 见 `.agent/L2/schema.md`；索引见 `.agent/index.md`。
