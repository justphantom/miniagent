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
	// §P1-B：overflowPending 记录「上一步真实 usage 命中静默溢出」，本步 FitHistory 前注入 budget.Force
	// 强制压缩（撞 provider 前用真实 usage 先压）。每步消费后归零、callLLM 后重新采样。
	var overflowPending bool
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
		ContextWindow:        cfg.ContextWindow,
		KeepRecent:           cfg.ContextKeepRecent,
		KeepReasoning:        cfg.ContextKeepReasoning,
		KeepToolArgs:         cfg.ContextKeepToolArgs,
		KeepReasoningChars:   cfg.ContextKeepReasoningChars,
		SummarizerPrompt:     cfg.SummarizerPrompt,
		CompactionModel:      cfg.CompactionModel,
		Model:                cfg.Model,
		System:               cfg.System,
		Tools:                cfg.Tools,
		UseRealUsage:         cfg.ContextUseRealUsage,
		PreserveRecentTokens: cfg.PreserveRecentTokens,
		Compacting:           hooks.OnCompacting,
		SessionID:            cfg.SessionID,
		Summarize: func(ctx context.Context, model, sys, prevSummary string, middle []Message) (string, Usage, error) {
			c := chat
			if cfg.CompactionChat != nil {
				c = cfg.CompactionChat
			}
			return summarizeMiddle(ctx, c, model, sys, prevSummary, maxChars, middle)
		},
	}

	// §P1-A：工具输出落盘 store（cfg.ToolOutputDir 空=禁用，走 trimForHistory 一次性硬截断）。
	// Run 入口构造一次、跨步复用；启动时机会性清理过期文件。
	var store *toolOutputStore
	if cfg.ToolOutputDir != "" {
		store = newToolOutputStore(cfg.ToolOutputDir, cfg.ToolOutputRetention, logger)
		store.cleanup()
	}

	for step := 1; step <= iterLimit; step++ {
		if err := ctx.Err(); err != nil {
			return Result{Usage: total, Steps: step - 1, Messages: msgs, NewMessages: newMsgs}, err
		}
		// 阶段 2（loop 每 step 前）：超 window 摘要中段 + 有损 fallback + 终止（见 FitHistory）。
		// 摘要调用的 usage 累加入 total（MaxTotalTokens 预算含摘要调用——审查 P2 摘要 token 不入预算）；
		// summarized=true 时记 compacted，供 defer 写 result.Compacted（交互层据此 rewrite session），
		// 并把 summary 插入 newMsgs（insertSummaryIntoNewMsgs 保排序与去重）。
		// §P1-B：上一步真实 usage 命中静默溢出时，本步强制压缩（Force 跳过 estimateTokens 4/5 门控）。
		budget.Force = overflowPending
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
		// §P1-C overflowRecovered：显式 ErrContextLength 与静默溢出两路径共享 guard，本步合计最多恢复一次。
		overflowRecovered := false
		if errors.Is(err, ErrContextLength) {
			// 撞 context 上限：收紧历史（清 reasoning + 压 tool content）后重试一次，仍超则上抛。
			msgs = trimHistoryForContext(msgs)
			if logger != nil {
				logger.Warn("context length exceeded; trimmed history; retrying step", "step", step)
			}
			resp, _, err = callLLMWithDowngrade(ctx, chat, stream, cfg, step, msgs, hooks, logger)
			overflowRecovered = true
		}
		if err != nil {
			return Result{Usage: total, Steps: step - 1, Messages: msgs, NewMessages: newMsgs}, err
		}
		// §P1-C 静默溢出（provider 200 成功但 finish/usage 暗示超窗）：仅本步未恢复过时 trim+重试一次，
		// 复用同一条 trimHistoryForContext + callLLMWithDowngrade 通路。不与显式路径叠加（overflowRecovered guard）。
		if !overflowRecovered && cfg.ContextWindow > 0 && isSilentContextOverflow(resp, cfg.ContextWindow) {
			if logger != nil {
				logger.Warn("silent context overflow detected; trimmed history; retrying step", "step", step, "finish", resp.FinishReason)
			}
			msgs = trimHistoryForContext(msgs)
			resp, _, err = callLLMWithDowngrade(ctx, chat, stream, cfg, step, msgs, hooks, logger)
			if err != nil {
				return Result{Usage: total, Steps: step - 1, Messages: msgs, NewMessages: newMsgs}, err
			}
		}
		total.InputTokens += resp.Usage.InputTokens
		total.OutputTokens += resp.Usage.OutputTokens
		// usage 全零（流式端点常不 honor include_usage / 非流式缺失）：用本地估算 fallback，
		// 避免预算熔断静默失效（P1-5）。估算计入请求侧（msgs+system+tools）和响应侧。
		if resp.Usage.InputTokens == 0 && resp.Usage.OutputTokens == 0 {
			if logger != nil {
				logger.Warn("llm returned no usage; using local token estimate for budget enforcement", "step", step)
			}
			total.InputTokens += estimateTokens(msgs, cfg.System, cfg.Tools)
			total.OutputTokens += estimateResponseTokens(resp)
		}
		// §P1-B：采集下一步的静默溢出判定（撞 provider 前用真实 usage 预压）。resp.Usage 全零
		// （fallback 路径）时 isUsageOverflow 恒 false，不影响；ContextWindow<=0 或 CompactionAuto=false 亦恒 false。
		overflowPending = isUsageOverflow(resp.Usage, cfg.ContextWindow, cfg.MaxTokens, cfg.CompactionReserved, cfg.CompactionAuto)
		// 预算熔断：以端点返回的真实 usage（或零 usage 时的本地估算）累计判定，超限即停。
		if cfg.MaxTotalTokens > 0 && total.InputTokens+total.OutputTokens > cfg.MaxTotalTokens {
			return Result{Usage: total, Steps: step, Messages: msgs, NewMessages: newMsgs}, fmt.Errorf(
				"%w: input=%d output=%d（累计超 MaxTotalTokens %d）",
				ErrBudgetExceeded, total.InputTokens, total.OutputTokens, cfg.MaxTotalTokens,
			)
		}

		if len(resp.ToolCalls) == 0 {
			// 最终文本入历史：接续对话需要看到上一轮的回答。§P0-B：附真实 usage 供后续防陈旧估算。
			appendMsg(&msgs, &newMsgs, Message{Role: roleAssistant, Content: resp.Text, Reasoning: resp.Reasoning, Usage: &resp.Usage})
			return Result{Text: resp.Text, Usage: total, Steps: step, Finish: finishStop, Messages: msgs, NewMessages: newMsgs}, nil
		}

		msgs, err = handleToolCalls(ctx, cfg, step, resp, toolByName, msgs, &newMsgs, hooks, store, logger)
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
			// 总结请求是内部引导消息，不持久化到 session（LoadSession 拒绝 roleSystem）。
			msgs = append(msgs, Message{Role: roleSystem, Content: summaryReq})
			if logger != nil {
				logger.Info("injecting summary request at iteration limit", "step", step)
			}
			resp2, _, err2 := callLLMWithDowngrade(ctx, chat, stream, cfg, step+1, msgs, hooks, logger)
			if err2 == nil && len(resp2.ToolCalls) == 0 {
				appendMsg(&msgs, &newMsgs, Message{Role: roleAssistant, Content: resp2.Text, Reasoning: resp2.Reasoning, Usage: &resp2.Usage})
				total.InputTokens += resp2.Usage.InputTokens
				total.OutputTokens += resp2.Usage.OutputTokens
				if resp2.Usage.InputTokens == 0 && resp2.Usage.OutputTokens == 0 {
					if logger != nil {
						logger.Warn("llm returned no usage; using local token estimate for budget enforcement", "step", step+1)
					}
					total.InputTokens += estimateTokens(msgs, cfg.System, cfg.Tools)
					total.OutputTokens += estimateResponseTokens(resp2)
				}
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
// §P0-B：对 Ts==0 的消息统一打 Unix 毫秒戳（供「真实 usage 防陈旧」判定）；已显式设 Ts 的（如
// compactWithSummary 的 summaryMsg）不覆盖——0 可靠表示「未承载」。
func appendMsg(msgs, newMsgs *[]Message, m Message) {
	if m.Ts == 0 {
		m.Ts = nowMs()
	}
	*msgs = append(*msgs, m)
	*newMsgs = append(*newMsgs, m)
}
