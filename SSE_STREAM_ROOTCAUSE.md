# 日志根因分析与修复方案

## 一、日志事实

```
miniagent: warning: session model "local/deepseek-v4-flash" does not match this run "local/glm-5.2"
llm call failed step=1 error="parse sse chunk: unexpected end of JSON input"
miniagent: warning: session model "local/deepseek-v4-flash" does not match this run "local/k3"
llm call failed step=1 error="parse sse chunk: unexpected end of JSON input"
```

两条独立 run（间隔 ~27s），都是 `step=1` 首步 LLM 调用失败。

## 二、根因分析

### 问题 A：session model 不匹配警告（预期行为，非 bug）

- 来源：`cmd/miniagent/session.go:120 warnSessionMismatch`
- 触发条件：**resume 一个已存在的会话**，且当前 run 的 `-model` 与创建该会话时持久化的 `meta.Model` 不同。
- 判定：仅警告不阻塞（`os.Stderr` 打印后继续），因为 resume 沿用旧会话历史，但本次 run 换了新模型。
- 日志含义：会话 A 用 `deepseek-v4-flash` 创建，后被 `glm-5.2`、`k3` 分别 resume。
- 结论：**符合设计**，无修复必要；仅作信息提示。若想消除噪音，可在 CLI 提示用户 resume 换模型后的影响，或文档说明。

### 问题 B：`parse sse chunk: unexpected end of JSON input`（真实故障）

链路：`DoStream`(stream.go) → `parseSSE`(stream_parse.go:107 `json.Unmarshal`) 返回
`fmt.Errorf("parse sse chunk: %w", err)`，其中 err = `unexpected end of JSON input`。

根因：**上游 SSE 流的某一条 `data:` 行 JSON 未写完时连接被掐断**（provider/gateway 中途 reset / 限流掐流）。

已有兜底：`isTransientStreamError`(stream.go:219) 已把该错误归类为 transient，
并支持 `deltaSent==0` 时透明重试一次（stream.go:196）。

**为什么最终仍 `llm call failed`：**

关键在重试门槛 `deltaSent == 0`（stream.go:196）。`deltaSent` 统计本次 attempt
已 `onDelta` 推给调用方的 delta 数。失败落地的两种可能：

1. **`deltaSent > 0`（最可能）**：上游先发出若干**完整** data chunk（已含
   content/reasoning/tool_calls，已实时推送用户），随后在**下一条** data 行中途
   被掐断。此时 `parseSSE` 报 `unexpected end of JSON input`，但因已有内容
   streamed，重试被判定不可行（避免把已输出的半段内容再次整体重放、产生重复/冲突），
   按设计 `return perr` → `llm call failed`。这是 `deltaSent==0` 重试策略的**已知边界**。

2. `deltaSent == 0` 但重试 3 次（`MaxRetries=2`，attempt=0/1/2）连续同错失败。

结合时间线（两次 run 间隔 27s，远大于内部 backoff 0.5+1+2s），**判断为两条独立
run，各自在 `deltaSent>0` 分支直接落地**——即模型换到 `glm-5.2`/`k3` 后，该网关
流式行为在输出中途断连（可能 429 限流/网关超时），且断点在已出内容之后。

## 三、修复方案

### 方案 1（推荐）：对「已出部分内容后的断连」增加可配置续传/降级

- `DoStream` 中当 `deltaSent>0` 且 `isTransientStreamError` 命中时，不再直接 `return perr`，
  而是返回 `(partialResp, perr)`，让上层 `callLLMWithDowngrade` 判定：
  - 若已拿到有意义的 partial 文本且 finish_reason 缺失，按「半途截断」处理（复用现有
    finish_reason≠stop 的 truncation 警告路径），把 partial 文本作为本步结果推进；
  - 否则报错。
- 优点：与现有「不重放半段流」约束不冲突，只是把「丢弃已出内容」改为「保留已出内容 + 标记截断」。
- 前置：确认 `parseSSE` 在错误时能返回可用的 partial `Response`（当前 streamAccum 已聚合到
  出错点，需把 acc 暴露给错误返回）。

### 方案 2：缩小 deltaSent 重试边界——半段 tool_call 可安全重试

- `deltaSent>0` 中若**尚未完成任何一条 tool_call 参数**（`acc.calls` 无已收尾项）且文本为空，
  说明用户界面未收到有效最终结果，可视为 `deltaSent==0` 等效，允许透明重试。
- 实现简单，但需精确判定「未产出可见结果」，边界需测试覆盖。

### 方案 3：针对 429/限流掐流的请求层前置控制

- 若确认是网关 429 中途掐流，可在流开始前检查上游限流头 / 降低并发；属运维调优，非代码主径。

### 建议
- 先在本仓库用 `glm-5.2`/`k3` 网关复现，抓完整 SSE 原始字节，确认断点位置与响应头
  （是否 429/502/连接 reset）——决定走方案 1 还是方案 3。
- 方案 1 收益最大且风险可控，建议优先实施。

## 四、影响范围与验证
- 改动集中 `provider/openai/stream.go` + `stream_parse.go`；上层 `loop_extra.go` 增加截断分支。
- 验证：单测覆盖「deltaSent>0 + transient 错误 → 返回 partial + 截断标记」；跑全绿 verify-gate。