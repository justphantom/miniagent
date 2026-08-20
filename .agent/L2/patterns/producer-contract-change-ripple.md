---
layer: L2
type: pattern
tags: [maintenance, change-ripple, grep, comments, docs, contract, post-change-checklist]
created: 2026-08-11
updated: 2026-08-11
confidence: high
---

# 改动涟漪：改/删 X 必 grep X 的代码+注释+测试+文档

## 模式
结构性改动（删函数/字段/兜底/文件加载、改生产者输出契约、改 wire 形态）后，X 的残留分布在四类位置，逐类 grep 才清干净：
1. **代码引用**：调用点、消费者、旁路逻辑
2. **注释**：解释 X 的 doc-comment（常先于代码过时）
3. **测试**：断言 X 行为、构造 X 的用例
4. **文档**：README/ARCHITECTURE/CONTRIBUTING/config.example（CHANGELOG 历史条目保留）

## 案例（本轮多起）
- 删 loopCfg `if system==""` 兜底 → prompts.go/prompts_test.go 注释仍引用「loopCfg fallback (dead code)」（注释类）
- 删 .miniagent persona/rules 文件加载 → ARCHITECTURE 目录树仍列 project.go、主流程仍「合并项目规则」、README `-workdir` help 仍「.miniagent 规则发现根」（文档类）
- compactWithSummary 改两路径都提取头 → jointTailBudget 消费者子句 + 注释未同步（代码+注释类，见 compaction-headadj-override-stale-clause）
- anthropic wire 折叠 role=system → 多个对称项漏（见 anthropic-provider-copy-asymmetry）

## 可复用经验（post-change checklist）
改 X 后立即跑：
- `grep -rn "X" --include=*.go` → 代码+注释
- `grep -rn "X" *_test.go` → 测试
- `grep -rn "X" *.md config.example.json` → 文档（CHANGELOG 历史保留）
对抗式 workflow 评审的「docs-consistency / assemble-changed-consistency」维度本质就是在跑这个 checklist。

## 关联
- `compaction-headadj-override-stale-clause`（L2/incidents）——案例
- `anthropic-provider-copy-asymmetry`（L2/incidents）——案例
- `adversarial-workflow-review`（L2/patterns）——机制化本 checklist

## 参考
- 复审 workflow（finder × verify 两轮）
