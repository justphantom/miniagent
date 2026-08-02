package miniagent

import (
	"context"
	"errors"
	"fmt"

	"log/slog"
)

// maxIterations：单轮 LLM 调用上限默认值，防工具循环烧 token；可经 MaxIterations 覆盖。
const maxIterations = 20

// summaryRequestPrompt 在迭代上限前一步工具调用后注入，引导 LLM 输出总结性回复而非继续调用工具。
const summaryRequestPrompt = "所有工具调用已完成。请输出一段总结性回复，说明完成了什么、关键结果是什么。不要在此之后继续调用工具。"

// Run 单轮 ReAct 循环：把 userPrompt 发给 llm，模型若请求工具则执行后回灌，
// 直到模型给出无 tool_calls 的最终文本或撞 maxIterations 上限。logger 为 nil 时静默。
//
// Result.Messages 是截至返回的全量 transcript，所有 return 路径均带回。配对：OnToolUse 非
// denied 的 error 路径尾部可能留下"有 tool_calls、无 tool 结果"的 assistant 消息（不可续跑）；
// OnToolResult error 路径已补占位 tool 消息保配对完整（P2-1）。出错分支不应持久化。
func Run(ctx context.Context, chat *ChatClient, stream *StreamClient, cfg LoopConfig, userPrompt string, hooks LoopHooks, logger *slog.Logger) (result Result, err error) {
	if chat == nil {
		return Result{}, errors.New("miniagent: chat client is nil")
	}
	if cfg.Stream && stream == nil {
		return Result{}, errors.New("miniagent: stream client is nil in stream mode")
	}
	// thinkingDowngraded/compacted 跨循环累积，统一经 defer 写入命名返回 result 的对应字段，
	// 避免在多个 return 点逐一展开赋值（loop.go 行数受限）。Compacted 任一步摘要成功即置位；
	// ThinkingDowngraded 任一次降级即置位。
	var thinkingDowngraded, compacted bool
	defer func() {
		result.ThinkingDowngraded = thinkingDowngraded
		result.Compacted = compacted
	}()
	toolByName := buildToolIndex(cfg.Tools, logger)

	// 复制 History：接续对话时调用方可能复用同一 slice，原地 append 会越界写其数据。
	msgs := make([]Message, 0, len(cfg.History)+1)
	msgs = append(msgs, cfg.History...)
	// 阶段 1（Run 入口统一）：屏障掉最新 summary 之前的旧历史（审查 v3 #3）。
	msgs = applyCompactionBarrier(msgs)
	// newMsgs 仅记本轮新增，main 据此 append-only 落盘；与 msgs 分离：裁剪只动 msgs。
	var newMsgs []Message
	appendMsg(&msgs, &newMsgs, Message{Role: roleUser, Content: userPrompt})
	total := Usage{}
	// 调用方可经 cfg.MaxIterations 覆盖默认上限；<=0 回退到包默认 maxIterations。
	iterLimit := cfg.MaxIterations
	if iterLimit <= 0 {
		iterLimit = maxIterations
	}
	// ContextBudget 一次构造、循环复用：摘要回调捕获 llm 与 maxChars，解耦 context.go 与 HTTPClient。
	maxChars := cfg.SummaryMaxChars
	if maxChars <= 0 {
		maxChars = summaryMaxChars
	}
	budget := ContextBudget{
		ContextWindow:    cfg.ContextWindow,
		KeepRecent:       cfg.ContextKeepRecent,
		SummarizerPrompt: cfg.SummarizerPrompt,
		CompactionModel:  cfg.CompactionModel,
		Model:            cfg.Model,
		System:           cfg.System,
		Tools:            cfg.Tools,
		Summarize: func(ctx context.Context, model, sys string, middle []Message) (string, Usage, error) {
			return summarizeMiddle(ctx, chat, model, sys, maxChars, middle)
		},
	}

	for step := 1; step <= iterLimit; step++ {
		if err := ctx.Err(); err != nil {
			return Result{Usage: total, Steps: step - 1, Messages: msgs, NewMessages: newMsgs}, err
		}
		// 阶段 2（loop 每 step 前）：超 window 摘要中段 + 有损 fallback + 终止（见 FitHistory）。
		// 摘要调用的 usage 累加入 total（MaxTotalTokens 预算含摘要调用——审查 P2 摘要 token 不入预算）；
		// summarized=true 时记 compacted，供 defer 写 result.Compacted（交互层据此 rewrite session），
		// 并把 summary 插入 newMsgs（insertSummaryIntoNewMsgs 保排序与去重）。
		fitted, summaryMsg, summarized, sumUsage, fitErr := FitHistory(ctx, msgs, budget, logger)
		if fitErr != nil {
			return Result{Usage: total, Steps: step - 1, Messages: msgs, NewMessages: newMsgs}, fitErr
		}
		msgs = fitted
		if summarized {
			compacted = true
			total.InputTokens += sumUsage.InputTokens
			total.OutputTokens += sumUsage.OutputTokens
			insertSummaryIntoNewMsgs(&newMsgs, summaryMsg)
		}
		// downgraded=true 时固化 cfg（P2-2：thinking 降级跨步生效，避免每步重撞 400）。
		resp, downgraded, err := callLLMWithDowngrade(ctx, chat, stream, cfg, step, msgs, hooks, logger)
		if downgraded {
			cfg.ThinkingLevel, cfg.Thinking = "", nil
			// 记录降级供 defer 写 result.ThinkingDowngraded：交互层据此清 baseCfg，
			// 避免下一轮重传原 thinking 值再撞 400（审查 P2 thinking 跨轮固化）。
			thinkingDowngraded = true
		}
		if errors.Is(err, ErrContextLength) {
			// 撞 context 上限：收紧历史（清 reasoning + 压 tool content）后重试一次，仍超则上抛。
			msgs = trimHistoryForContext(msgs)
			if logger != nil {
				logger.Warn("context length exceeded; trimmed history; retrying step", "step", step)
			}
			resp, _, err = callLLMWithDowngrade(ctx, chat, stream, cfg, step, msgs, hooks, logger)
		}
		if err != nil {
			return Result{Usage: total, Steps: step - 1, Messages: msgs, NewMessages: newMsgs}, err
		}
		total.InputTokens += resp.Usage.InputTokens
		total.OutputTokens += resp.Usage.OutputTokens
		// usage 全零（流式端点常不 honor include_usage / 非流式缺失）：预算熔断静默失效，warn 暴露（P1-5）。
		if logger != nil && resp.Usage.InputTokens == 0 && resp.Usage.OutputTokens == 0 {
			logger.Warn("llm returned no usage; budget enforcement may be ineffective", "step", step)
		}
		// 预算熔断：以端点返回的真实 usage 累计判定，超限即停（error 路径 + 退出码 1）。
		if cfg.MaxTotalTokens > 0 && total.InputTokens+total.OutputTokens > cfg.MaxTotalTokens {
			return Result{Usage: total, Steps: step, Messages: msgs, NewMessages: newMsgs}, fmt.Errorf(
				"%w: input=%d output=%d（累计超 MaxTotalTokens %d）",
				ErrBudgetExceeded, total.InputTokens, total.OutputTokens, cfg.MaxTotalTokens,
			)
		}

		if len(resp.ToolCalls) == 0 {
			// 最终文本入历史：接续对话需要看到上一轮的回答。
			appendMsg(&msgs, &newMsgs, Message{Role: roleAssistant, Content: resp.Text, Reasoning: resp.Reasoning})
			return Result{Text: resp.Text, Usage: total, Steps: step, Finish: finishStop, Messages: msgs, NewMessages: newMsgs}, nil
		}

		msgs, err = handleToolCalls(ctx, cfg, step, resp, toolByName, msgs, &newMsgs, hooks, logger)
		if err != nil {
			return Result{Usage: total, Steps: step, Messages: msgs, NewMessages: newMsgs}, err
		}
		// 阶段 3（Option B：末尾工具调用后强制总结）。
		// 若当前步已达迭代上限且刚执行完工具，注入总结请求让 LLM 输出最终文本而非继续调工具。
		// 允许一次额外 LLM 调用（step+1），确保有总结性回复；若 LLM 仍要调工具或出错，
		// 回落至 finishMaxIterations。
		if step == iterLimit {
			summaryReq := cfg.SummaryRequest
			if summaryReq == "" {
				summaryReq = summaryRequestPrompt
			}
			appendMsg(&msgs, &newMsgs, Message{Role: roleSystem, Content: summaryReq})
			if logger != nil {
				logger.Info("injecting summary request at iteration limit", "step", step)
			}
			resp2, _, err2 := callLLMWithDowngrade(ctx, chat, stream, cfg, step+1, msgs, hooks, logger)
			if err2 == nil && len(resp2.ToolCalls) == 0 {
				appendMsg(&msgs, &newMsgs, Message{Role: roleAssistant, Content: resp2.Text, Reasoning: resp2.Reasoning})
				total.InputTokens += resp2.Usage.InputTokens
				total.OutputTokens += resp2.Usage.OutputTokens
				return Result{Text: resp2.Text, Usage: total, Steps: step + 1, Finish: finishStop, Messages: msgs, NewMessages: newMsgs}, nil
			}
			if err2 != nil && logger != nil {
				logger.Warn("summary LLM call failed", "step", step+1, "error", err2)
			}
		}
	}
	// 达到迭代上限：返回 nil error，让上层仍能消费已累积的 Usage。
	// Finish=finishMaxIterations 是终止信号（无最终 Text）。
	return Result{Usage: total, Steps: iterLimit, Finish: finishMaxIterations, Messages: msgs, NewMessages: newMsgs}, nil
}

// appendMsg 同时追加到 msgs（LLM context）与 newMsgs（持久化）。所有产生点统一经此，
// 保证 session 落盘与上下文一致（审查 v1 #1 + v2 #4）。
func appendMsg(msgs, newMsgs *[]Message, m Message) {
	*msgs = append(*msgs, m)
	*newMsgs = append(*newMsgs, m)
}
