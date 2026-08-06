package miniagent

import (
	"context"
	"errors"
	"log/slog"

	"github.com/justphantom/miniagent/internal/text"
)

// maxIterations：单轮 LLM 调用上限默认值，防工具循环烧 token；可经 MaxIterations 覆盖。
const maxIterations = 20

// summaryRequestPrompt 在迭代上限前一步工具调用后注入，引导 LLM 输出总结性回复而非继续调用工具。
const summaryRequestPrompt = "所有工具调用已完成。请输出一段总结性回复，说明完成了什么、关键结果是什么。不要在此之后继续调用工具。"

// Run 是极简的 ReAct 核心循环：发 userPrompt → LLM 回工具则执行回灌 → 直到模型给出无 tool_calls 的
// 最终文本或撞 maxIterations。核心只做五件事：注册工具、拼接上下文、调 LLM、按 LLM 要求执行工具、
// LLM 无 tool_calls 则退出循环。一切上下文管理（压缩/记忆/溢出）、用量估算、预算判定、工具结果成型、
// LLM 失败恢复——经 LoopHooks 外挂（OnBudget/OnLLMError/ShapeToolResult/BeforeLLM/AfterLLM），核心零策略。
// 默认行为经 NewDefault* 工厂复用（cmd 层组装）。logger 为 nil 时静默。
//
// Result.Messages 是截至返回的全量 transcript，所有 return 路径均带回；NewMessages 是本轮新增（main 据此
// append-only 落盘）。LLM 失败默认直接上抛（OnLLMError nil 时）；默认钩子 NewDefaultOnLLMError 承载
// ErrContextLength 收紧重试（防长会话崩溃）。
func Run(ctx context.Context, llm LLM, cfg LoopConfig, userPrompt string, hooks LoopHooks, logger *slog.Logger) (result Result, err error) {
	if llm == nil {
		return Result{}, errors.New("miniagent: llm provider is nil")
	}
	// thinkingDowngraded/compacted 跨循环累积（defer 在 total/msgs/newMsgs 声明后统一写入命名返回 result）。
	var thinkingDowngraded, compacted bool
	toolByName := buildToolIndex(cfg.Tools, logger)

	// 复制 History：接续对话时调用方可能复用同一 slice，原地 append 会越界写其数据。
	msgs := make([]Message, 0, len(cfg.History)+1)
	msgs = append(msgs, cfg.History...)
	// newMsgs 仅记本轮新增，main 据此 append-only 落盘；与 msgs 分离：裁剪只动 msgs。
	var newMsgs []Message
	appendMsg(&msgs, &newMsgs, Message{Role: RoleUser, Content: userPrompt})
	total := Usage{}

	// Usage/Messages/NewMessages/thinkingDowngraded/compacted 经 defer 统一写入命名返回 result——消除 12 处
	// return 的三件套重复，且新增 return 不必手写（防漏填致 session 持久化丢消息）。各 return 只设差异字段
	// （Steps/Text/Finish）。llm==nil 的早期 return 在此 defer 注册前，不受影响（result 零值）。
	defer func() {
		result.Usage = total
		result.Messages = msgs
		result.NewMessages = newMsgs
		result.ThinkingDowngraded = thinkingDowngraded
		result.Compacted = compacted
	}()

	iterLimit := cfg.MaxIterations
	if iterLimit <= 0 {
		iterLimit = maxIterations
	}

	// captureDowngrade 固化 thinking 降级：清 cfg 的 thinking 字段 + 置位 thinkingDowngraded。
	// 主路径 / OnLLMError 重试 / 总结闭包三处共用——callLLMWithDowngrade 的 downgraded 返回值统一处理点。
	captureDowngrade := func(down bool) {
		if down {
			cfg.ThinkingLevel, cfg.Thinking = "", nil
			thinkingDowngraded = true
		}
	}

	// summarizeAtLimit 撞迭代上限时注入 RoleSystem 总结请求并额外调一次 LLM 求最终文本。逻辑外提为闭包，
	// 使主循环 for 体聚焦五件事；捕获 Run 全部局部状态，仅 step 作参数（闭包定义先于 for，循环变量不可直接捕获）。
	// 总结请求经临时 reqMsgs 发送——不污染 transcript（内部引导消息不进 Result.Messages / newMsgs）。
	// ok=true 表示已得出终止 Result（res+err），调用方直接 return；ok=false 表示总结未成，回落 finishMaxIterations。
	summarizeAtLimit := func(s int) (Result, bool, error) {
		summaryReq := cfg.SummaryRequest
		if summaryReq == "" {
			summaryReq = summaryRequestPrompt
		}
		if logger != nil {
			logger.Info("injecting summary request at iteration limit", "step", s)
		}
		reqMsgs := make([]Message, 0, len(msgs)+1)
		reqMsgs = append(reqMsgs, msgs...)
		reqMsgs = append(reqMsgs, Message{Role: RoleSystem, Content: summaryReq})
		resp2, down2, err2 := callLLMWithDowngrade(ctx, llm, cfg, s+1, reqMsgs, hooks, logger)
		// 捕获总结步 downgraded（经 captureDowngrade 统一固化）。
		captureDowngrade(down2)
		if err2 != nil {
			if logger != nil {
				logger.Warn("summary LLM call failed", "step", s+1, "error", err2)
			}
			return Result{}, false, nil
		}
		if len(resp2.ToolCalls) != 0 {
			// 回落：模型仍要调工具，但已撞上限不再执行。这次调用 token 仍消耗，记账与主路径对齐
			// （主路径 tool_calls 也累加 usage），但不执行工具、回落 finishMaxIterations。
			total.InputTokens += resp2.Usage.InputTokens
			total.OutputTokens += resp2.Usage.OutputTokens
			return Result{}, false, nil
		}
		appendMsg(&msgs, &newMsgs, Message{Role: RoleAssistant, Content: resp2.Text, Reasoning: resp2.Reasoning, Usage: &resp2.Usage})
		if hooks.AfterLLM != nil {
			if aerr := hooks.AfterLLM(ctx, s+1, resp2); aerr != nil {
				// recordStepUsage 未执行（在其后），按 Steps=已记账 usage 调用数 的语义计 s（=(s+1)-1），
				// 与主路径 AfterLLM err 的 step-1 对齐。
				return Result{Steps: s}, true, aerr
			}
		}
		if berr := recordStepUsage(ctx, hooks, s+1, resp2, reqMsgs, cfg, &total); berr != nil {
			return Result{Steps: s + 1, Finish: finishStop}, true, berr
		}
		return Result{Text: resp2.Text, Steps: s + 1, Finish: finishStop}, true, nil
	}

	for step := 1; step <= iterLimit; step++ {
		if err := ctx.Err(); err != nil {
			return Result{Steps: step - 1}, err
		}
		// 开放缝 BeforeLLM：压缩/记忆/RAG/上下文管理。nil=透传（极简模式）。
		toSend, perr := applyBeforeLLM(ctx, hooks, step, &msgs, &newMsgs, &total, &compacted, cfg)
		if perr != nil {
			return Result{Steps: step - 1}, perr
		}

		resp, downgraded, err := callLLMWithDowngrade(ctx, llm, cfg, step, toSend, hooks, logger)
		captureDowngrade(downgraded)
		// 开放缝 OnLLMError：LLM 失败恢复（典型 ErrContextLength 收紧重试）。nil=error 直接上抛。
		// 核心不做任何错误恢复策略；默认实现 NewDefaultOnLLMError 承载原 trimHistoryForContext。
		if err != nil && hooks.OnLLMError != nil {
			recovered, retry, rerr := hooks.OnLLMError(ctx, step, msgs, err)
			if rerr != nil {
				return Result{Steps: step - 1}, rerr
			}
			if retry {
				if recovered != nil {
					msgs = recovered
				}
				// 捕获重试路径的 downgraded（经 captureDowngrade 统一固化）。
				var down2 bool
				resp, down2, err = callLLMWithDowngrade(ctx, llm, cfg, step, msgs, hooks, logger)
				captureDowngrade(down2)
			}
		}
		if err != nil {
			return Result{Steps: step - 1}, err
		}

		// 开放缝 AfterLLM：用量记账 / 静默溢出判定（压缩插件据此置下步 Force）。
		if hooks.AfterLLM != nil {
			if aerr := hooks.AfterLLM(ctx, step, resp); aerr != nil {
				return Result{Steps: step - 1}, aerr
			}
		}

		// 开放缝 OnBudget：累加真实 usage（零 usage 估算 fallback 由 NewDefaultOnBudget 承载）+ 预算判定。
		// nil=核心不估算不熔断（极简）。AfterLLM 与 OnBudget 的 Steps 语义不同（前者 step-1、后者 step），
		// 故 AfterLLM 留在调用方、仅累加+判定下沉到 recordStepUsage 复用。
		if berr := recordStepUsage(ctx, hooks, step, resp, toSend, cfg, &total); berr != nil {
			return Result{Steps: step}, berr
		}

		if len(resp.ToolCalls) == 0 {
			// 最终文本入历史：接续对话需要看到上一轮的回答。附真实 usage 供外挂策略防陈旧估算。
			appendMsg(&msgs, &newMsgs, Message{Role: RoleAssistant, Content: resp.Text, Reasoning: resp.Reasoning, Usage: &resp.Usage})
			return Result{Text: resp.Text, Steps: step, Finish: finishStop}, nil
		}

		msgs, err = handleToolCalls(ctx, cfg, step, resp, toolByName, msgs, &newMsgs, hooks, logger)
		if err != nil {
			return Result{Steps: step}, err
		}
		// 撞迭代上限且刚执行完工具：注入总结请求让 LLM 输出最终文本（允许一次额外调用）；
		// 未成则回落 finishMaxIterations（总结逻辑外提为 summarizeAtLimit，主循环保持简洁）。
		if step == iterLimit {
			if res, ok, err := summarizeAtLimit(step); ok {
				return res, err
			}
		}
	}
	// 达到迭代上限：返回 nil error，让上层仍能消费已累积的 Usage。Finish=finishMaxIterations 是终止信号。
	return Result{Steps: iterLimit, Finish: finishMaxIterations}, nil
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
		// 压缩场景：收缩后的 View 即新的运行 transcript。用 toSend（已 nil 补救）而非裸 View——
		// 防 StepOutput{Commit:true, View:nil} 误用静默清空 transcript（View 文档必填，但核心不强制）。
		*msgs = toSend
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
		m.Ts = text.NowMs()
	}
	*msgs = append(*msgs, m)
	*newMsgs = append(*newMsgs, m)
}

// recordStepUsage 把单步真实 usage 累加进 total 并经 OnBudget 钩子做零 usage 估算 fallback + 预算判定。
// 主路径与总结路径共用，消除两处三段式重复（AfterLLM 因 Steps 语义与 Result 构造耦合，仍留在各调用方）。
// toSend 是本轮实际发给 LLM 的消息视图，供 OnBudget 估算。
func recordStepUsage(ctx context.Context, hooks LoopHooks, step int, resp Response, toSend []Message, cfg LoopConfig, total *Usage) error {
	total.InputTokens += resp.Usage.InputTokens
	total.OutputTokens += resp.Usage.OutputTokens
	if hooks.OnBudget != nil {
		if berr := hooks.OnBudget(ctx, step, BudgetInput{ToSend: toSend, System: cfg.System, Tools: cfg.Tools, Resp: resp}, total); berr != nil {
			return berr
		}
	}
	return nil
}
