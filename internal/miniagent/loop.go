package miniagent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/justphantom/miniagent/internal/text"
)

// maxIterations: default upper bound on LLM calls per turn, to prevent tool loops burning tokens; overridable via MaxIterations.
const maxIterations = 20

// summaryRequestPrompt is injected after the tool call one step before the iteration limit, guiding the LLM to emit a summary reply rather than continuing to call tools.
const summaryRequestPrompt = "All tool calls have completed. Please provide a summary reply describing what was accomplished and the key results. Do not continue calling tools after this."

// Run is the minimalist ReAct core loop: send userPrompt → if the LLM replies with tools, execute and feed results back → repeat until the model emits final text without tool_calls or hits maxIterations. The core does only five things: register tools, assemble context, call the LLM, execute tools as the LLM requests, exit the loop when the LLM returns no tool_calls. All context management (compaction/memory/overflow), usage estimation, budget assessment, tool result shaping, and LLM failure recovery — are plugged in via LoopHooks (OnBudget/OnLLMError/ShapeToolResult/BeforeLLM/AfterLLM); the core is policy-free.
// Default behavior is reused via NewDefault* factories (assembled at the cmd layer). When logger is nil, it is silent.
//
// Result.Messages is the full transcript up to return, carried back on all return paths; NewMessages is this turn's additions (main persists them append-only). On LLM failure the default is to propagate the error directly (when OnLLMError is nil); the default hook NewDefaultOnLLMError carries ErrContextLength-tightened retry (preventing long-session crashes).
func Run(ctx context.Context, llm LLM, cfg LoopConfig, userPrompt string, hooks LoopHooks, logger *slog.Logger) (result Result, err error) {
	if llm == nil {
		return Result{}, errors.New("miniagent: llm provider is nil")
	}
	// thinkingDowngraded/compacted accumulate across the loop (written into the named return result via defer after total/msgs/newMsgs are declared).
	var thinkingDowngraded, compacted bool
	// llmReqs counts actual requests sent to the LLM endpoint this turn (including downgrade/error retries/summary step), written to result.LLMRequests via defer.
	var llmReqs int
	toolByName := buildToolIndex(cfg.Tools, logger)

	// Copy History: when resuming a conversation the caller may reuse the same slice, and an in-place append would overwrite its data.
	msgs := make([]Message, 0, len(cfg.History)+1)
	msgs = append(msgs, cfg.History...)
	// newMsgs records only this turn's new entries, which main persists append-only; kept separate from msgs: trimming only touches msgs.
	var newMsgs []Message
	appendMsg(&msgs, &newMsgs, Message{Role: RoleUser, Content: userPrompt})
	total := Usage{}

	// Usage/Messages/NewMessages/thinkingDowngraded/compacted are written into the named return result uniformly via defer — eliminating the three-piece duplication across 12 returns, and new returns need not write it by hand (preventing omitted fields that would drop messages during session persistence). Each return sets only the differing fields (Steps/Text/Finish). The early return for llm==nil happens before this defer is registered and is unaffected (result is zero-valued).
	defer func() {
		// Top-level panic backstop: safeCall (loop_tools.go) and callLLMOnce (loop_extra.go) each recover their own
		// panics, but Run had none — so a panic in any of the 7 user hooks (BeforeLLM/AfterLLM/OnBudget/OnLLMError/
		// OnToolUse/OnToolResult/ShapeToolResult) or in the applyBeforeLLM/recordStepUsage helpers would crash the
		// process and lose the turn's un-persisted NewMessages. recover() runs first so the field assignments below
		// still execute (the transcript survives for session persistence); the named err converts the panic into a
		// normal error return. Folded into this existing defer rather than wrapping each hook call site — strictly
		// more protective (also catches core-logic panics).
		if r := recover(); r != nil {
			if logger != nil {
				logger.Error("run panicked", "panic", r)
			}
			err = fmt.Errorf("run panicked: %v", r)
		}
		result.Usage = total
		result.Messages = msgs
		result.NewMessages = newMsgs
		result.ThinkingDowngraded = thinkingDowngraded
		result.Compacted = compacted
		result.LLMRequests = llmReqs
	}()

	iterLimit := cfg.MaxIterations
	if iterLimit <= 0 {
		iterLimit = maxIterations
	}

	// captureDowngrade persists the thinking downgrade: clears cfg's thinking fields + sets thinkingDowngraded.
	// Shared by the main path / OnLLMError retry / summary closure — the unified handling point for callLLMWithDowngrade's downgraded return value.
	captureDowngrade := func(down bool) {
		if down {
			cfg.ThinkingLevel, cfg.Thinking = "", nil
			thinkingDowngraded = true
		}
	}

	// summarizeAtLimit injects a RoleSystem summary request and makes one extra LLM call for final text when the iteration limit is hit. Factored out as a closure,
	// so the main for body stays focused on the five things; it captures all of Run's local state, taking only step as a parameter (the closure is defined before for, so loop variables cannot be captured directly).
	// The summary request is sent via a temporary reqMsgs — it does not pollute the transcript (internal bootstrapping messages do not enter Result.Messages / newMsgs).
	// ok=true means a terminating Result has been produced (res+err), and the caller returns directly; ok=false means the summary failed and falls back to FinishMaxIterations.
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
		resp2, down2, reqs2, err2 := callLLMWithDowngrade(ctx, llm, cfg, s+1, reqMsgs, hooks, logger)
		// Capture downgrade in the summary step (consolidated via captureDowngrade).
		captureDowngrade(down2)
		llmReqs += reqs2 // summary step: count actual requests (2 when it downgraded), not a flat 1 — reqs2 is returned even on the err path
		if err2 != nil {
			if logger != nil {
				logger.Warn("summary LLM call failed", "step", s+1, "error", err2)
			}
			return Result{}, false, nil
		}
		if len(resp2.ToolCalls) != 0 {
			// Fallback: the model still wants tools, but the iteration limit is already hit so nothing is executed. This call's tokens are still consumed — accumulated via
			// recordStepUsage and circuit-broken through OnBudget (aligned with the main path), so the degraded path cannot bypass budget enforcement and silently exceed MaxTotalTokens.
			if berr := recordStepUsage(ctx, hooks, s+1, resp2, reqMsgs, cfg, &total); berr != nil {
				return Result{Steps: s + 1}, true, berr
			}
			// Exception (already documented, see TestRun_SummaryInjectionFallsBack): the summary call is an internal bootstrap that "persuades the model to wrap up after the limit is hit";
			// tokens count toward total (to prevent budget bypass) but **Steps are not accumulated** — falls back ok=false so :179 returns Steps=iterLimit.
			// Steps denote "active ReAct steps"; the summary bootstrap is not an active step, so usage and Steps are intentionally out of sync on this path.
			return Result{}, false, nil
		}
		appendMsg(&msgs, &newMsgs, Message{Role: RoleAssistant, Content: resp2.Text, Reasoning: resp2.Reasoning, Usage: &resp2.Usage})
		if hooks.AfterLLM != nil {
			if aerr := hooks.AfterLLM(ctx, s+1, resp2); aerr != nil {
				// recordStepUsage has not run yet (it is after this); compute s per the Steps=recorded usage calls semantics (=(s+1)-1),
				// aligned with the main path's AfterLLM err step-1.
				return Result{Steps: s}, true, aerr
			}
		}
		if berr := recordStepUsage(ctx, hooks, s+1, resp2, reqMsgs, cfg, &total); berr != nil {
			return Result{Steps: s + 1}, true, berr // budget error: Finish left empty (error-return invariant), aligned with :107 and the main loop
		}
		return Result{Text: resp2.Text, Steps: s + 1, Finish: FinishStop}, true, nil
	}

	for step := 1; step <= iterLimit; step++ {
		// OnStep observability seam: fires once at the top of each iteration (before any branching) so it covers every exit path.
		// Observe-only; built from state already in hand (no extra scans). nil = zero overhead.
		if hooks.OnStep != nil {
			hooks.OnStep(ctx, StepSnapshot{
				Step:          step,
				TranscriptLen: len(msgs),
				InputTokens:   total.InputTokens,
				OutputTokens:  total.OutputTokens,
				Compacted:     compacted,
				LLMRequests:   llmReqs,
				NewMessages:   len(newMsgs),
			})
		}
		if err := ctx.Err(); err != nil {
			return Result{Steps: step - 1}, err
		}
		// Open seam BeforeLLM: compaction / memory / RAG / context management. nil=pass-through (minimal mode).
		toSend, perr := applyBeforeLLM(ctx, hooks, step, &msgs, &newMsgs, &total, &compacted, cfg)
		if perr != nil {
			return Result{Steps: step - 1}, perr
		}

		resp, downgraded, reqs, err := callLLMWithDowngrade(ctx, llm, cfg, step, toSend, hooks, logger)
		captureDowngrade(downgraded)
		llmReqs += reqs // main-path call: count actual requests (2 when a thinking downgrade retried), not a flat 1
		// Open seam OnLLMError: LLM failure recovery (typically ErrContextLength triggers trim-and-retry). nil=error is re-raised directly.
		// The core does no error-recovery policy; the default implementation NewDefaultOnLLMError carries the former trimHistoryForContext.
		// Pass msgs (persistent transcript) rather than toSend (BeforeLLM's transient View, Commit=false never enters the transcript):
		// recovery must act on the persisted transcript, and retries are re-sent against the trimmed msgs (discarding injections is exactly the context-recovery direction).
		if err != nil && hooks.OnLLMError != nil {
			recovered, retry, rerr := hooks.OnLLMError(ctx, step, msgs, err)
			if rerr != nil {
				return Result{Steps: step - 1}, rerr
			}
			if retry {
				if recovered != nil {
					msgs = recovered
				}
				// Capture downgrade on the retry path (unified solidification via captureDowngrade).
				var down2 bool
				var reqs2 int
				resp, down2, reqs2, err = callLLMWithDowngrade(ctx, llm, cfg, step, msgs, hooks, logger)
				captureDowngrade(down2)
				llmReqs += reqs2 // OnLLMError retry: count actual requests (2 when the retry itself downgraded)
				// The retry actually sends the trimmed msgs (recovered); refresh toSend so downstream recordStepUsage/OnBudget
				// estimation aligns with what was actually sent (otherwise the pre-trim toSend would systematically overestimate, breaking the contract).
				toSend = msgs
			}
		}
		if err != nil {
			return Result{Steps: step - 1}, err
		}

		// Open seam AfterLLM: usage accounting / silent-overflow detection (compaction plugins set next-step Force based on this).
		if hooks.AfterLLM != nil {
			if aerr := hooks.AfterLLM(ctx, step, resp); aerr != nil {
				return Result{Steps: step - 1}, aerr
			}
		}

		// Open seam OnBudget: accumulate real usage (zero-usage estimation fallback carried by NewDefaultOnBudget) + budget judgment.
		// nil=core neither estimates nor circuit-breaks (minimal). AfterLLM and OnBudget differ in Steps semantics (former step-1, latter step),
		// so AfterLLM stays in the caller while only accumulation + judgment sink into recordStepUsage for reuse.
		if berr := recordStepUsage(ctx, hooks, step, resp, toSend, cfg, &total); berr != nil {
			return Result{Steps: step}, berr
		}

		if len(resp.ToolCalls) == 0 {
			// Final text enters history: subsequent turns need to see the previous answer. Attach real usage for external strategies to prevent stale estimation.
			appendMsg(&msgs, &newMsgs, Message{Role: RoleAssistant, Content: resp.Text, Reasoning: resp.Reasoning, Usage: &resp.Usage})
			return Result{Text: resp.Text, Steps: step, Finish: FinishStop}, nil
		}

		msgs, err = handleToolCalls(ctx, cfg, step, resp, toolByName, msgs, &newMsgs, hooks, logger)
		if err != nil {
			return Result{Steps: step}, err
		}
		// Hit iteration limit and just finished executing tools: inject a summary request so the LLM emits final text (allowing one extra call);
		// if it fails, fall back to FinishMaxIterations (summary logic extracted into summarizeAtLimit to keep the main loop concise).
		if step == iterLimit {
			if res, ok, err := summarizeAtLimit(step); ok {
				return res, err
			}
		}
	}
	// ctx cancelled during the last tool/summary step also lands here: the tool returns "cancelled" (not an error) → handleToolCalls returns nil →
	// summarizeAtLimit fails with the cancelled ctx and returns ok=false → loop exits. ctx must be checked explicitly, otherwise the cancellation
	// is swallowed into FinishMaxIterations + nil error (exit code 0 instead of 130, violating the "Run must return Canceled promptly" contract).
	if err := ctx.Err(); err != nil {
		return Result{Steps: iterLimit}, err
	}
	// Iteration limit reached: return nil error so the caller can still consume the accumulated Usage. Finish=FinishMaxIterations is the termination signal.
	return Result{Steps: iterLimit, Finish: FinishMaxIterations}, nil
}

// applyBeforeLLM invokes hooks.BeforeLLM (nil=pass-through, minimal mode) and folds its side effects on
// transcript / persistence / usage / compaction flags back into the loop's local state. Returns the message view (toSend) actually sent to the LLM this turn.
func applyBeforeLLM(ctx context.Context, hooks LoopHooks, step int, msgs, newMsgs *[]Message, total *Usage, compacted *bool, cfg LoopConfig) ([]Message, error) {
	if hooks.BeforeLLM == nil {
		return *msgs, nil
	}
	out, err := hooks.BeforeLLM(ctx, StepInput{Step: step, Msgs: *msgs, System: cfg.System, Tools: cfg.Tools})
	if err != nil {
		return nil, err
	}
	toSend := out.View
	if len(toSend) == 0 {
		// Both nil and empty slice fall back to *msgs: under Commit=true an empty View (including []Message{}) would silently clear the transcript;
		// the core does not require View to be non-empty, so guard here (a legitimate compaction keeps at least the recent turn + user prompt; an empty View is always misuse).
		toSend = *msgs
	}
	if out.Commit {
		// Compaction scenario: the shrunk View is the new running transcript (len guard already applied, will not clear).
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

// mergePersisted bulk-merges persisted into newMsgs: entries with a non-empty Kind replace the old entry of the same Kind in newMsgs
// (e.g. multiple compactions keep only the latest summary), then prepends the batch to the front of newMsgs — preserving the semantics
// of the cross-turn barrier hitting the latest summary (generalized to any persisted entry carrying a Kind).
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

// appendMsg appends to both msgs (LLM context) and newMsgs (persisted). All producers go through this uniformly,
// keeping session persistence consistent with the context. Messages with Ts==0 get a Unix-millisecond timestamp (for external strategies'
// "real usage anti-staleness" check); an explicitly set Ts (e.g. a compaction summaryMsg) is not overwritten — 0 reliably means "not carried".
func appendMsg(msgs, newMsgs *[]Message, m Message) {
	if m.Ts == 0 {
		m.Ts = text.NowMs()
	}
	*msgs = append(*msgs, m)
	*newMsgs = append(*newMsgs, m)
}

// recordStepUsage accumulates a single step's real usage into total and runs the zero-usage estimation fallback + budget check via the OnBudget hook.
// Shared by the main path and the summary path, eliminating the duplicated three-part logic in two places (AfterLLM stays at each caller because its Steps
// semantics and Result construction are coupled). toSend is the message view actually sent to the LLM this turn, used by OnBudget for estimation.
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
