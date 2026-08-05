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

// Run 是极简的 ReAct 核心循环：发 userPrompt → LLM 回工具则执行回灌 → 直到模型给出无 tool_calls 的
// 最终文本或撞 maxIterations。核心不做任何上下文管理（压缩/记忆/溢出/估算全无）——一切上下文策略经
// LoopHooks.BeforeLLM/AfterLLM 外挂（见 NewCompaction）。logger 为 nil 时静默。
//
// 开放缝：
//   - BeforeLLM（每步调 LLM 前）：改写消息视图、收缩 transcript、注入记忆/RAG、提交持久化摘要、累加用量。
//     nil=透传（极简模式：原样发 transcript，零上下文管理）。
//   - AfterLLM（每步响应后）：用量记账、静默溢出判定（压缩插件据此置下步强制压缩）。
//
// Result.Messages 是截至返回的全量 transcript，所有 return 路径均带回；NewMessages 是本轮新增（main 据此
// append-only 落盘）。ErrContextLength 会触发一次收紧重试（核心健壮性，防长会话崩溃），其余错误上抛。
func Run(ctx context.Context, chat *ChatClient, stream *StreamClient, cfg LoopConfig, userPrompt string, hooks LoopHooks, logger *slog.Logger) (result Result, err error) {
	if chat == nil {
		return Result{}, errors.New("miniagent: chat client is nil")
	}
	if cfg.Stream && stream == nil {
		return Result{}, errors.New("miniagent: stream client is nil in stream mode")
	}
	// thinkingDowngraded/compacted 跨循环累积，统一经 defer 写入命名返回 result 的对应字段。
	var thinkingDowngraded, compacted bool
	defer func() {
		result.ThinkingDowngraded = thinkingDowngraded
		result.Compacted = compacted
	}()
	toolByName := buildToolIndex(cfg.Tools, logger)

	// 复制 History：接续对话时调用方可能复用同一 slice，原地 append 会越界写其数据。
	msgs := make([]Message, 0, len(cfg.History)+1)
	msgs = append(msgs, cfg.History...)
	// newMsgs 仅记本轮新增，main 据此 append-only 落盘；与 msgs 分离：裁剪只动 msgs。
	var newMsgs []Message
	appendMsg(&msgs, &newMsgs, Message{Role: roleUser, Content: userPrompt})
	total := Usage{}

	iterLimit := cfg.MaxIterations
	if iterLimit <= 0 {
		iterLimit = maxIterations
	}

	// 工具输出落盘 store（cfg.ToolOutputDir 空=禁用）。仍属核心工具结果成型策略，跨步复用。
	var store *toolOutputStore
	if cfg.ToolOutputDir != "" {
		store = newToolOutputStore(cfg.ToolOutputDir, cfg.ToolOutputRetention, logger)
		store.cleanup()
	}

	for step := 1; step <= iterLimit; step++ {
		if err := ctx.Err(); err != nil {
			return Result{Usage: total, Steps: step - 1, Messages: msgs, NewMessages: newMsgs}, err
		}
		// 开放缝 BeforeLLM：压缩/记忆/RAG/上下文管理。nil=透传（极简模式）。
		toSend, perr := applyBeforeLLM(ctx, hooks, step, &msgs, &newMsgs, &total, &compacted, cfg)
		if perr != nil {
			return Result{Usage: total, Steps: step - 1, Messages: msgs, NewMessages: newMsgs}, perr
		}

		resp, downgraded, err := callLLMWithDowngrade(ctx, chat, stream, cfg, step, toSend, hooks, logger)
		if downgraded {
			cfg.ThinkingLevel, cfg.Thinking = "", nil
			thinkingDowngraded = true
		}
		// 撞 context 上限：收紧历史（清 reasoning + 压 tool content）后重试一次——核心健壮性，仍超则上抛。
		if errors.Is(err, ErrContextLength) {
			msgs = trimHistoryForContext(msgs)
			if logger != nil {
				logger.Warn("context length exceeded; trimmed history; retrying step", "step", step)
			}
			resp, _, err = callLLMWithDowngrade(ctx, chat, stream, cfg, step, msgs, hooks, logger)
		}
		if err != nil {
			return Result{Usage: total, Steps: step - 1, Messages: msgs, NewMessages: newMsgs}, err
		}

		// 开放缝 AfterLLM：用量记账 / 静默溢出判定（压缩插件据此置下步 Force）。
		if hooks.AfterLLM != nil {
			if aerr := hooks.AfterLLM(ctx, step, resp); aerr != nil {
				return Result{Usage: total, Steps: step - 1, Messages: msgs, NewMessages: newMsgs}, aerr
			}
		}

		total.InputTokens += resp.Usage.InputTokens
		total.OutputTokens += resp.Usage.OutputTokens
		// usage 全零（流式端点常不 honor include_usage / 非流式缺失）：用本地估算 fallback，
		// 避免预算熔断静默失效。估算计入请求侧（toSend+system+tools）与响应侧。
		if resp.Usage.InputTokens == 0 && resp.Usage.OutputTokens == 0 {
			if logger != nil {
				logger.Warn("llm returned no usage; using local token estimate for budget enforcement", "step", step)
			}
			total.InputTokens += estimateTokens(toSend, cfg.System, cfg.Tools)
			total.OutputTokens += estimateResponseTokens(resp)
		}
		// 预算熔断：以端点返回的真实 usage（或零 usage 时的本地估算）累计判定，超限即停。
		if cfg.MaxTotalTokens > 0 && total.InputTokens+total.OutputTokens > cfg.MaxTotalTokens {
			return Result{Usage: total, Steps: step, Messages: msgs, NewMessages: newMsgs}, fmt.Errorf(
				"%w: input=%d output=%d（累计超 MaxTotalTokens %d）",
				ErrBudgetExceeded, total.InputTokens, total.OutputTokens, cfg.MaxTotalTokens,
			)
		}

		if len(resp.ToolCalls) == 0 {
			// 最终文本入历史：接续对话需要看到上一轮的回答。附真实 usage 供外挂策略防陈旧估算。
			appendMsg(&msgs, &newMsgs, Message{Role: roleAssistant, Content: resp.Text, Reasoning: resp.Reasoning, Usage: &resp.Usage})
			return Result{Text: resp.Text, Usage: total, Steps: step, Finish: finishStop, Messages: msgs, NewMessages: newMsgs}, nil
		}

		msgs, err = handleToolCalls(ctx, cfg, step, resp, toolByName, msgs, &newMsgs, hooks, store, logger)
		if err != nil {
			return Result{Usage: total, Steps: step, Messages: msgs, NewMessages: newMsgs}, err
		}
		// 撞迭代上限且刚执行完工具：注入总结请求让 LLM 输出最终文本（允许一次额外调用），
		// 落回落至 finishMaxIterations。总结请求是内部引导消息，不持久化（LoadSession 拒绝 roleSystem）。
		if step == iterLimit {
			summaryReq := cfg.SummaryRequest
			if summaryReq == "" {
				summaryReq = summaryRequestPrompt
			}
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
	// 达到迭代上限：返回 nil error，让上层仍能消费已累积的 Usage。Finish=finishMaxIterations 是终止信号。
	return Result{Usage: total, Steps: iterLimit, Finish: finishMaxIterations, Messages: msgs, NewMessages: newMsgs}, nil
}

// applyBeforeLLM 调 hooks.BeforeLLM（nil=透传，极简模式）并把它对 transcript / 持久化 / 用量 / 压缩标记的
// 副作用折叠回循环局部状态。返回本轮实际发给 LLM 的消息视图 toSend。
func applyBeforeLLM(ctx context.Context, hooks LoopHooks, step int, msgs, newMsgs *[]Message, total *Usage, compacted *bool, cfg LoopConfig) ([]Message, error) {
	if hooks.BeforeLLM == nil {
		return *msgs, nil
	}
	out, err := hooks.BeforeLLM(ctx, StepInput{Step: step, Msgs: *msgs, System: cfg.System, Tools: cfg.Tools})
	if err != nil {
		return nil, err
	}
	toSend := out.View
	if toSend == nil {
		toSend = *msgs
	}
	if out.Commit {
		// 压缩场景：收缩后的 View 即新的运行 transcript。
		*msgs = out.View
	}
	if len(out.Persist) > 0 {
		mergePersisted(newMsgs, out.Persist)
	}
	if out.ExtraUsage != nil {
		total.InputTokens += out.ExtraUsage.InputTokens
		total.OutputTokens += out.ExtraUsage.OutputTokens
	}
	if out.Compacted {
		*compacted = true
	}
	return toSend, nil
}

// mergePersisted 把 persisted 批量并入 newMsgs：带非空 Kind 的条目替换 newMsgs 中同 Kind 旧条目
// （如多次压缩只留最新 summary），随后把批次前插到 newMsgs 首部——保跨轮 barrier 命中最新 summary 的语义
// （与原 insertSummaryIntoNewMsgs 行为一致，泛化到任意带 Kind 的持久化条目）。
func mergePersisted(newMsgs *[]Message, persisted []Message) {
	kinds := make(map[string]bool, len(persisted))
	for _, m := range persisted {
		if m.Kind != "" {
			kinds[m.Kind] = true
		}
	}
	filtered := make([]Message, 0, len(*newMsgs)+len(persisted))
	for _, m := range *newMsgs {
		if m.Kind != "" && kinds[m.Kind] {
			continue
		}
		filtered = append(filtered, m)
	}
	merged := make([]Message, 0, len(filtered)+len(persisted))
	merged = append(merged, persisted...)
	merged = append(merged, filtered...)
	*newMsgs = merged
}

// appendMsg 同时追加到 msgs（LLM context）与 newMsgs（持久化）。所有产生点统一经此，
// 保证 session 落盘与上下文一致。对 Ts==0 的消息统一打 Unix 毫秒戳（供外挂策略的「真实 usage 防陈旧」判定）；
// 已显式设 Ts 的（如压缩 summaryMsg）不覆盖——0 可靠表示「未承载」。
func appendMsg(msgs, newMsgs *[]Message, m Message) {
	if m.Ts == 0 {
		m.Ts = nowMs()
	}
	*msgs = append(*msgs, m)
	*newMsgs = append(*newMsgs, m)
}
