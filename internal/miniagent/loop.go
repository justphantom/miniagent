package miniagent

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"log/slog"
)

// maxIterations：单轮 LLM 调用上限默认值，防工具循环烧 token；可经 MaxIterations 覆盖。
const maxIterations = 20

// maxToolResultInHistory：单条 tool 结果入历史字符上限，平衡可读性与上下文预算。
const maxToolResultInHistory = 2000

// maxParallelTools：同一步并行工具上限，防耗尽 FD/连接或触发目标限流。
const maxParallelTools = 8

// summaryRequestPrompt 在迭代上限前一步工具调用后注入，引导 LLM 输出总结性回复而非继续调用工具。
const summaryRequestPrompt = "所有工具调用已完成。请输出一段总结性回复，说明完成了什么、关键结果是什么。不要在此之后继续调用工具。"

func safeCall(ctx context.Context, logger *slog.Logger, tool Tool, name, args string) (res ToolResult) {
	defer func() {
		if r := recover(); r != nil {
			if logger != nil {
				logger.Error("tool panic recovered", "tool", name, "panic", r)
			}
			res = ToolResult{IsError: true, Output: fmt.Sprintf("工具 %q 内部错误", name)}
		}
	}()
	return tool.Call(ctx, args)
}

// Run 单轮 ReAct 循环：把 userPrompt 发给 llm，模型若请求工具则执行后回灌，
// 直到模型给出无 tool_calls 的最终文本或撞 maxIterations 上限。logger 为 nil 时静默。
//
// Result.Messages 是截至返回的全量 transcript，所有 return 路径均带回。配对：OnToolUse 非
// denied 的 error 路径尾部可能留下"有 tool_calls、无 tool 结果"的 assistant 消息（不可续跑）；
// OnToolResult error 路径已补占位 tool 消息保配对完整（P2-1）。出错分支不应持久化。
func Run(ctx context.Context, llm *HTTPClient, cfg LoopConfig, userPrompt string, hooks LoopHooks, logger *slog.Logger) (result Result, err error) {
	if llm == nil {
		return Result{}, errors.New("miniagent: llm client is nil")
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

	for step := 1; step <= iterLimit; step++ {
		if err := ctx.Err(); err != nil {
			return Result{Usage: total, Steps: step - 1, Messages: msgs, NewMessages: newMsgs}, err
		}
		// 阶段 2（loop 每 step 前）：超 window 摘要中段 + 有损 fallback + 终止（见 compactIfOverWindow）。
		// 摘要调用的 usage 累加入 total（MaxTotalTokens 预算含摘要调用——审查 P2 摘要 token 不入预算）；
		// summarized=true 时记 compacted，供 defer 写 result.Compacted（交互层据此 rewrite session）。
		summarized, sumUsage, cerr := compactIfOverWindow(ctx, llm, cfg, &msgs, &newMsgs, logger)
		if cerr != nil {
			return Result{Usage: total, Steps: step - 1, Messages: msgs, NewMessages: newMsgs}, cerr
		}
		if summarized {
			compacted = true
		}
		total.InputTokens += sumUsage.InputTokens
		total.OutputTokens += sumUsage.OutputTokens
		// downgraded=true 时固化 cfg（P2-2：thinking 降级跨步生效，避免每步重撞 400）。
		resp, downgraded, err := callLLMWithDowngrade(ctx, llm, cfg, step, msgs, hooks, logger)
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
			resp, _, err = callLLMWithDowngrade(ctx, llm, cfg, step, msgs, hooks, logger)
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

		msgs, err = handleToolCalls(ctx, step, resp, toolByName, msgs, &newMsgs, hooks, logger)
		if err != nil {
			return Result{Usage: total, Steps: step, Messages: msgs, NewMessages: newMsgs}, err
		}
		// 阶段 3（Option B：末尾工具调用后强制总结）。
		// 若当前步已达迭代上限且刚执行完工具，注入总结请求让 LLM 输出最终文本而非继续调工具。
		// 允许一次额外 LLM 调用（step+1），确保有总结性回复；若 LLM 仍要调工具或出错，
		// 回落至 finishMaxIterations。
		if step == iterLimit {
			appendMsg(&msgs, &newMsgs, Message{Role: roleSystem, Content: summaryRequestPrompt})
			if logger != nil {
				logger.Info("injecting summary request at iteration limit", "step", step)
			}
			resp2, _, err2 := callLLMWithDowngrade(ctx, llm, cfg, step+1, msgs, hooks, logger)
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

func buildToolIndex(tools []Tool, logger *slog.Logger) map[string]Tool {
	toolByName := make(map[string]Tool, len(tools))
	for _, t := range tools {
		if _, dup := toolByName[t.Name]; dup && logger != nil {
			// 重名静默覆盖会让前者不可达且无任何线索，路由歧义极难排查。
			logger.Warn("duplicate tool name, last wins", "tool", t.Name)
		}
		toolByName[t.Name] = t
	}
	return toolByName
}

// callLLM 保持原签名供 thinking_test.go 直接调用，委托 callLLMWithDowngrade 并丢弃
// downgraded。Run 改用 callLLMWithDowngrade 以跨步固化 thinking 降级（P2-2）。
func callLLM(ctx context.Context, llm *HTTPClient, cfg LoopConfig, step int, msgs []Message, hooks LoopHooks, logger *slog.Logger) (Response, error) {
	resp, _, err := callLLMWithDowngrade(ctx, llm, cfg, step, msgs, hooks, logger)
	return resp, err
}

// callLLMWithDowngrade：callLLMOnce 之上做单步 thinking 降级重试，回传 downgraded 供 Run
// 跨步固化 cfg；其余（重试一次、日志、截断告警）与原 callLLM 一致。
func callLLMWithDowngrade(ctx context.Context, llm *HTTPClient, cfg LoopConfig, step int, msgs []Message, hooks LoopHooks, logger *slog.Logger) (Response, bool, error) {
	if logger != nil {
		logger.Debug("llm call start", "step", step, "model", cfg.Model, "stream", cfg.Stream, "thinking", cfg.ThinkingLevel)
	}
	resp, err := callLLMOnce(ctx, llm, cfg, step, msgs, hooks)
	downgraded := false
	// thinking 不被支持：去字段重试一次（仅当确实发了 thinking，避免无谓重试，审查 v2 #7）。
	if errors.Is(err, ErrThinkingUnsupported) && cfg.ThinkingLevel != "" && cfg.ThinkingLevel != ThinkingOff {
		if logger != nil {
			logger.Warn("thinking 不被端点支持，去字段重试一次", "step", step)
		}
		down := cfg
		down.ThinkingLevel = ""
		down.Thinking = nil
		resp, err = callLLMOnce(ctx, llm, down, step, msgs, hooks)
		downgraded = true
	}
	if err != nil {
		if logger != nil {
			logger.Warn("llm call failed", "step", step, "error", err)
		}
		return Response{}, downgraded, fmt.Errorf("llm call %d: %w", step, err)
	}
	if logger != nil {
		logger.Info("llm call done", "step", step, "input_tokens", resp.Usage.InputTokens, "output_tokens", resp.Usage.OutputTokens, "tool_calls", len(resp.ToolCalls), "finish_reason", resp.FinishReason)
	}
	// finish_reason 非 stop/tool_calls 表示回答被 max_tokens 或内容过滤截断。
	if logger != nil && resp.FinishReason != "" && resp.FinishReason != "stop" && resp.FinishReason != "tool_calls" {
		logger.Warn("llm response truncated", "step", step, "finish_reason", resp.FinishReason)
	}
	return resp, downgraded, nil
}

func handleToolCalls(ctx context.Context, step int, resp Response, toolByName map[string]Tool, msgs []Message, newMsgs *[]Message, hooks LoopHooks, logger *slog.Logger) ([]Message, error) {
	calls := make([]ToolCall, len(resp.ToolCalls))
	for i, tc := range resp.ToolCalls {
		calls[i] = tc
		if calls[i].ID == "" {
			calls[i].ID = fmt.Sprintf("synth_%d_%d", step, i)
		}
	}
	// 思考链随 assistant 消息入历史（回灌 reasoning 模型所需）。
	appendMsg(&msgs, newMsgs, Message{Role: roleAssistant, Reasoning: resp.Reasoning, ToolCalls: calls})

	// 先按序通知本轮全部 tool_use：消费方尽早看到完整工具计划，且顺序确定。
	// OnToolUse 返回 ErrToolDenied 表示拒绝该工具（如危险命令未确认）：记录后继续
	// 通知其余工具，runToolsParallel 跳过被拒者；其他 error 仍终止循环。
	denied := make(map[string]bool)
	if hooks.OnToolUse != nil {
		for _, tc := range calls {
			if err := hooks.OnToolUse(tc.Name, tc.Args); err != nil {
				if errors.Is(err, ErrToolDenied) {
					denied[tc.ID] = true
					continue
				}
				return msgs, err
			}
		}
	}

	// 同一步内 LLM 一次发起的多个 tool_call 相互独立，串行会让总耗时 = Σ 单工具
	// 耗时（shell 可达数十秒）。并行执行，结果按原 index 回填，保证历史消息
	// 与 assistant.tool_calls 一一对应（OpenAI 要求顺序匹配）。
	results := runToolsParallel(ctx, logger, calls, toolByName, denied)

	for i, tc := range calls {
		tres := results[i]
		if logger != nil {
			logger.Info("tool executed", "step", step, "tool", tc.Name, "is_error", tres.IsError, "output_len", len(tres.Output))
		}
		// 工具执行后通知消费方结果（含 ExitCode/is_error），供实时观察与校验。
		if hooks.OnToolResult != nil {
			if err := hooks.OnToolResult(tc.Name, tc.ID, tres); err != nil {
				// 配对补全：下游不可写，剩余 calls（含当前 i）结果无法提交；但 assistant.tool_calls 已入历史，
				// 须为每个补一条占位 tool 消息，否则 Messages 配对断裂、续跑被端点 400（P2-1）。
				for j := i; j < len(calls); j++ {
					appendMsg(&msgs, newMsgs, Message{Role: roleTool, ToolCallID: calls[j].ID, Content: "工具未提交结果：上游管道错误"})
				}
				return msgs, err
			}
		}
		limit := 0
		if t, ok := toolByName[tc.Name]; ok {
			limit = t.ResultLimit
		}
		appendMsg(&msgs, newMsgs, Message{Role: roleTool, ToolCallID: tc.ID, Content: trimForHistory(tres.Output, limit)})
	}
	return msgs, nil
}

// runToolsParallel 并行执行 calls，返回与 calls 同序的结果。
// 各 goroutine 写入 results 的不同下标，无内存竞争；wg.Wait 提供 happens-before。
// 未知工具在调度前短路，直接回填错误结果。每个 tool 的 panic 由 safeCall 兜底。
// 用 buffered chan 做信号量限制同时在途的工具数（maxParallelTools）。
func runToolsParallel(ctx context.Context, logger *slog.Logger, calls []ToolCall, toolByName map[string]Tool, denied map[string]bool) []ToolResult {
	results := make([]ToolResult, len(calls))
	var wg sync.WaitGroup
	sem := make(chan struct{}, maxParallelTools)
	for i, tc := range calls {
		if denied[tc.ID] {
			results[i] = ToolResult{IsError: true, ExitCode: exitCodeNotSet, Output: "用户拒绝执行"}
			continue
		}
		tool, ok := toolByName[tc.Name]
		if !ok {
			// ExitCode=exitCodeNotSet 与 denied 一致：未知工具从未真正执行，零值 0 会被事件层误读为成功（P3-4）。
			results[i] = ToolResult{IsError: true, ExitCode: exitCodeNotSet, Output: fmt.Sprintf("未知工具 %q", tc.Name)}
			continue
		}
		wg.Add(1)
		go func(i int, tc ToolCall, tool Tool) {
			defer wg.Done()
			// 信号量获取联动 ctx：取消后排队中的调用直接放弃，不再等空位；
			// 否则一个不尊重 ctx 的阻塞工具会永久占位，wg.Wait 不返回、Run 挂死。
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				results[i] = ToolResult{IsError: true, Output: "已取消"}
				return
			}
			defer func() { <-sem }()
			results[i] = safeCall(ctx, logger, tool, tc.Name, tc.Args)
		}(i, tc, tool)
	}
	wg.Wait()
	return results
}

// trimForHistory 把工具结果裁到 limit 字符后入历史；limit<=0 用默认上限。
// read/edit 等代码类工具经 Tool.ResultLimit 传高限，避免截断丢准确性。
// C-2 的 context 降级复用同一裁剪语义（对更早的 tool content 用更小 limit 再裁）。
func trimForHistory(s string, limit int) string {
	if limit <= 0 {
		limit = maxToolResultInHistory
	}
	return truncate(s, limit, "…[tool_result 已截断]")
}
