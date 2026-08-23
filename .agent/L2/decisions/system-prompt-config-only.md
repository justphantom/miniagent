---
layer: L2
type: decision
tags: [system-prompt, project-rules, config, breaking, migration]
created: 2026-08-11
updated: 2026-08-11
confidence: high
---

# system prompt 来源统一为 config-only（.miniagent/ 层收口）

## 决策

system prompt 来源从「config `defaults.system_prompt` + `workdir/.miniagent/persona.md`/`rules.md`」收敛为**仅 config `defaults.system_prompt`**（未配则内置 `defaultSystemPrompt` + subagent guidance）。继全局 `~/.miniagent/` 层（v4.2.0 已删）之后的第二次收口。

## 背景

- v4.2.0 删全局 `~/.miniagent/` 层，但保留 workdir 单层 `.miniagent/` 读 `persona.md`/`rules.md`。
- `.miniagent/` 目录本身已被用作 session 存储（`.agent/` 类似角色），两套语义混在一个目录下制造歧义。
- system prompt 来源散在 config + 文件系统两处，迁移/迁移指导/权限判断都不干净。

## 关键细节

1. **删除** `cmd/miniagent/project.go`（`loadProjectRules`/`mergeSystemPrompt`/`projectRules`/`readTrimmedFile`）+ `project_test.go`。
2. `assembleSystemPrompt` 砍 `pr projectRules` 参数：`base=="" → defaultSystemPrompt` + `injectSubagentGuidance` 两步。
3. **NEW-1 回归测试保留**（`TestAssembleSystemPrompt_DefaultApplied`）：默认 config 空 base 必须回落到 `defaultSystemPrompt`，否则 `injectSubagentGuidance` 把空串变非空，loopCfg `if system==""` 兜底变死代码——这个坑在 v4.3.0 之前踩过。

## 迁移指引

- 原 `.miniagent/persona.md`（**取代默认**语义）→ 直进 `defaults.system_prompt`，语义等价。
- 原 `.miniagent/rules.md`（**追加**语义）→ 物进 `system_prompt` 文本时需**自行保留内置默认工作流约束**（Observe before acting / Verify / Review failures / Precise edits / Segment large files），否则接受其丢失。这是唯一真正的迁移风险点。
- `.miniagent/` 目录本身保留用于 session 存储，不受影响。

## 不变量

- 核心引擎仍不感知任何具体项目，只拼 system prompt；config-first 裁决优先级 `cli > config > builtin` 不变。
- NDJSON 事件契约、session 存储、tool 契约均不受影响。

## 补充：opt-in 规则文件（2026-08-12）

`defaults.rules_file`（opt-in，默认空）允许 workdir 下一个**纯 basename**（拒 `..`/绝对/子目录）的规则文件追加到 system prompt（base 后/guidance 前）。受控回归——默认行为仍 config-only（不变量默认下成立），仅显式配置时引入文件来源；满足原 rules.md「追加」语义需求（迁移指引点名的风险点），改 basename-only + opt-in 收窄「来源分散/越界」。实现：`assembleSystemPrompt` 加 workdir/rulesFile 形参 + `appendProjectRules`（限 64KiB + stderr 不致命）。

## 补充：workdir 绝对路径注入（2026-08-13）

system prompt 在 rules 后/guidance 前无条件追加一行 `Working directory (absolute): <wd>`（空 workdir 跳过）。不构成对 config-only 收口的违反：workdir 来自 `-workdir` flag（运行时信息，非文件来源），与 guidance 无条件注入 configAbsPath 同一性质。实现：`appendWorkdirLine`（prompts.go）。

## 备注

本项目记忆自身用 `.agent/` 目录（非 `.miniagent/`），本轮改动不影响 .agent/ 的读取与写入。