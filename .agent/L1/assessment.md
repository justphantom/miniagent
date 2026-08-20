---
layer: L1
type: assessment
updated: 2026-08-18
---

# 深度精简评估

## 评分总览

| 维度 | 评分 | 关键优势 | 关键改进点 |
|------|------|----------|-----------|
| 精简 | 4.5/5 | 零外部依赖, 核心 219 行, 钩子全可 nil | 6 文件接近 300 行上限 |
| 可用 | 5/5 | NDJSON 稳定, -replay/-metrics, 退出码 0/1/130 | 降版本号, 加最小 config 模板 |
| 健壮 | 5/5 | panic 兜底, 信号保护, 配对补全, 溢出检测 | 版本号加 fallback |
| 灵活 | 5/5 | 9 钩子 + 5 字段 StepOutput, 接口化 Provider/Tool | 多个 BeforeLLM 需手动链式 |
| 正确 | 5/5 | msgs/newMsgs 分离, 配对三路径补全, 反陈旧 | 无实质性缺陷 |
| 规范 | 5/5 | 纯 std 库, 哨兵 error, NDJSON, 文档完整 | 确认 Go 版本 |

## 已落地改进项

### 1. main.go 精简（292→约260行）
- `buildRuntimeClients` 提取到 `setup_providers.go`
- `assembleHooks` 提取到 `setup.go`

### 2. config_load.go 拆分（227→约90行）
- `validateConfig` 拆到 `validate.go`

### 3. 版本号 fallback
- `version` 空时回落 `"dev"`

### 4. loop_tools.go 拆分（186→约100行）
- `handleToolCalls` 拆到 `tool_handler.go`

### 5. compaction/budget.go 拆分（287→约200行）
- `jointTailBudget`/`preserveRecentTokens`/`estimateRoundTokens` 拆到 `budget_tail.go`

### 6. 移除 anthropic + responses provider
- 删除 `internal/provider/anthropic/` 全部 17 文件
- 删除 `internal/provider/responses/` 全部 14 文件
- 简化 `providerKind` 仅支持 `""`/`"openai"`，`providerKind` 死代码清除
- 更新 config 校验（`validate.go` 删 anthropic 专属校验 / responses thinking.field 校验）
- 更新 `-list-models`/`FetchModelLimits` 统一 openai 路径
- 同步 README.md / ARCHITECTURE.md / CHANGELOG.md / config.example.json

## 架构设计亮点

### 钩子架构（核心最佳实践）
- `LoopHooks` 9 字段皆可 nil → 退化为极简 agent
- `BeforeLLM` 通过 `StepOutput.View/Commit/Persist/ExtraUsage/Compacted` 精确表达意图
- `NewCompaction` 封装为 `(before, after)` 对，压缩即外挂
- 错误路径 `OnToolResult`/`ShapeToolResult` 补 `fillPlaceholderTail` 保配对

### 精简模式对比
| 模式 | BeforeLLM | OnBudget | OnLLMError | ShapeToolResult |
|------|-----------|----------|------------|-----------------|
| 极简（无策略） | nil | nil | nil | nil |
| 默认（完整能力） | NewCompaction | NewDefaultOnBudget | NewDefaultOnLLMError | NewDefaultShapeToolResult |

### 健壮性模式
- `msgs`（上下文）与 `newMsgs`（持久化）分离，`appendMsg` 同步
- `captureDowngrade` 跨步固化 thinking 降级
- `mergePersisted` 同 Kind 去重
- `Ts` 反陈旧：新 summary 打新戳，下轮强制重新估算
- `isUsageOverflow` 静默溢出检测（从历史真实 usage 推断 Force）
- 配对补全三路径：OnToolUse error / OnToolResult error / ShapeToolResult error