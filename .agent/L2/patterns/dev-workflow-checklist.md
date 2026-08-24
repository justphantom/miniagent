---
layer: L2
type: pattern
tags: [workflow, checklist, task-lifecycle, dev-process, plan-first]
created: 2026-08-24
confidence: evolving
---

# 开发流程检查单：需求 → 方案 → 实现 → 验证 → 审阅 → 提交 → 记忆

## 模式
单任务生命周期的顺序约束。规则本体分散在 `AGENTS.md`（行为红线）与 `.agent/L0/policies.md`，本条将其串成主线并补实践案例；编码标准本身见 `AGENTS.md` 不重复。

## 流程

1. **需求澄清**：只做明确要求的；隐含需求先确认；不确定就呈现选项与利弊，不替用户决定。
2. **方案先行**：多文件改动必须先出方案待确认——含改动文件清单、每处动作与理由、明确不动项。确认后才动手。
3. **实现**：函数单一职责；标准库优先；功能必有标准库测试（用例即需求文档）。改 compaction/session/NDJSON 契约按 `L0/policies.md` §5 落点检查。
4. **验证**：`make verify` 全绿（gofmt/build/vet/test -race/lint/300 行上限/memory-integrity），缺一不可；编译或测试不过自主修复 ≤3 轮，超限升级给用户。
5. **审阅与提交**：
   - 提交前必给 diff 摘要（`git diff --stat` + 关键 hunk 说明）待审阅；
   - **工作区可能有非本任务的遗留改动**——先 `git status` 核对，遗留文件不并入本任务提交（v6.6.5 会话实例：`events.js` 遗留混入 diff stat，识别后单独成提交）；
   - 一次一事单独提交；subject ≤72 字符、祈使、无句号。
6. **记忆闭环**：每轮更新 `.agent/L1/active/session.md`；任务结束评估沉淀进 L2 并在 index.md 登记。

## 可复用经验
- **方案先行是防误删主闸门**：AGENTS.md 这类常驻文件的精简，用户确认环节多次纠偏（删什么留什么的判断权在用户）。
- **diff stat 先看全量再 add**：`git status --short` 与预期文件清单逐一对照，多出的即遗留。
- 大改动评审升级用对抗式 workflow（见 `adversarial-workflow-review.md`）；结构性改动后跑涟漪检查（见 `producer-contract-change-ripple.md`）；发版走 `release-checklist.md`。

## 参考
- `AGENTS.md`（行为红线 / 编码标准 / 流程节）、`.agent/L0/policies.md`（verify-gate / 改动落点）
