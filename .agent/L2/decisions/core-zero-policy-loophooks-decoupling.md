---
layer: L2
type: decision
tags: [architecture, loophooks, decoupling, library, core, subpackage]
created: 2026-08-09
confidence: high
---

# 循环核心零策略 + LoopHooks 外挂 + 渐进子包化解耦

## 背景
miniagent 定位为极简、无内置策略的 ReAct agent 核心库。核心循环 `loop.go:Run`（`loop.go` / `loop_api.go` / `loop_tools.go` / `loop_extra.go` / `message.go` / `errors.go` / `provider_api.go`）只做五件事：注册钩子 / 拼上下文 / 调 LLM / 执行工具 / 无 tool_calls 退出。

## 决策
一切上下文能力经 `LoopHooks`（`loop_api.go`）的 8 个可 nil 函数字段 + `CompactingHook` 外挂：压缩 `NewCompaction`、预算 `NewDefaultOnBudget`、溢出恢复 `NewDefaultOnLLMError`、工具成型 `NewDefaultShapeToolResult`、NDJSON 事件、`provider/openai`。装配集中在 `cmd/miniagent/main.go`。

`StepOutput` 五字段（`View` / `Commit` / `Persist` / `ExtraUsage` / `Compacted`）是策略外挂的关键语义：`Commit=true` 替换运行 transcript；`Persist` 带 `Kind` 条目替换同 `Kind` 旧条目（多次压缩只留最新 summary）。

渐进子包化（4.1.0 → 4.2.x，commits `7ce5c47` / `d641129`）：拆 `compaction` / `event` / `provider` / `tools` / `policy` / `config` / `session` / `looptest` 子包。依赖方向严格单向无环（核心不 import 任一子包），领域类型 `Message/Response/Usage/Request/Delta` 留核心。

## 理由
- 不挂钩子即极简 agent；挂默认工厂即恢复完整能力。
- 换压缩 / 预算策略只需实现自己的钩子，核心零改动。
- `nil` 语义清晰；刻意不引入接口 / 中间件链（节制抽象，对齐 `AGENTS.md`）。钩子用函数字段而非接口，便于装配期自由组合。

## 关键约束
1. 真正库化（移出 `internal/`、开放外部导入）缓至 **5.0.0** breaking；此前 `internal/` 下 API 无外部承诺。
2. 明确**不做**：拆 `LoopConfig` 载体字段、`Message` 双层类型——改动面 > 收益。
3. 依赖方向是当前架构最健康的一点，子包外迁会断链。

## 参考
- `internal/miniagent/loop.go`、`internal/miniagent/loop_api.go`
- `cmd/miniagent/main.go`（钩子装配）
- commits `7ce5c47`、`d641129`
