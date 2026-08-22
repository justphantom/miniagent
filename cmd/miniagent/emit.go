package main

// emit.go assembles the NDJSON event-output hooks and terminal emitters, parameterized by
// an io.Writer (CLI passes os.Stdout; the -serve web path passes the per-request response
// writer so the identical NDJSON contract streams over HTTP). Split out of setup.go to
// respect the per-file line cap.

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/justphantom/miniagent/miniagent"
	"github.com/justphantom/miniagent/miniagent/event"
	"github.com/justphantom/miniagent/miniagent/policy"
)

// buildHooks returns the event-emitting subset of LoopHooks, writing to w.
// resultOnly (subagent fork) yields empty hooks: stdout is the plain-text result, no NDJSON.
func buildHooks(w io.Writer, resultOnly bool) miniagent.LoopHooks {
	if resultOnly {
		return miniagent.LoopHooks{}
	}
	emit := event.ToolUseWriter(w)
	return miniagent.LoopHooks{
		OnToolUse: func(name, callID, input string) error { return emit(name, callID, input) },
		OnToolResult: func(name, callID string, r miniagent.ToolResult) error {
			return event.EmitToolResult(w, name, callID, r)
		},
		OnDelta: func(step int, kind miniagent.DeltaKind, text string) error {
			return event.EmitDelta(w, step, kind, text)
		},
	}
}

func emitRunError(w io.Writer, err error, resultOnly bool, logger *slog.Logger) {
	if resultOnly {
		fmt.Printf("error: %s\n", err.Error())
		return
	}
	if eerr := event.EmitError(w, err.Error()); eerr != nil {
		logger.Warn("emit error failed", "error", eerr)
		fmt.Fprintf(os.Stderr, "miniagent: %v\n", err)
	}
}

func emitRunResult(w io.Writer, result miniagent.Result, model string, resultOnly bool, logger *slog.Logger) error {
	if resultOnly {
		fmt.Println(result.Text)
		return nil
	}
	if err := event.EmitResult(w, result, model); err != nil {
		logger.Warn("emit result failed", "error", err)
		return fmt.Errorf("emit result failed: %w", err)
	}
	return nil
}

// assembleHooks assembles LoopHooks: event output (buildHooks) + compaction before/after + three default policy attachments
// (OnLLMError history-tightening retry / OnBudget estimation+circuit-break / ShapeToolResult truncation+persist). The core Run has zero policies; this function assembles them to restore full capability.
func assembleHooks(
	compBefore func(context.Context, miniagent.StepInput) (miniagent.StepOutput, error),
	compAfter func(context.Context, int, miniagent.Response) error,
	out io.Writer, resultOnly, confirmDestructive bool, baseCfg miniagent.LoopConfig, tools []miniagent.Tool, limits miniagent.Limits, logger *slog.Logger,
) miniagent.LoopHooks {
	hooks := buildHooks(out, resultOnly)
	hooks.BeforeLLM = compBefore
	hooks.AfterLLM = compAfter
	hooks.OnLLMError = policy.NewDefaultOnLLMError(logger, limits.ContextTrimToolChars)
	hooks.OnBudget = policy.NewDefaultOnBudget(baseCfg.MaxTotalTokens, logger)
	hooks.ShapeToolResult = policy.NewDefaultShapeToolResult(tools, baseCfg.ToolOutputDir, baseCfg.ToolOutputRetention, baseCfg.MaxToolResultChars, logger)
	// S-2 (opt-in): wrap OnToolUse with a destructive-tool confirmation gate. buildHooks returns empty hooks (no
	// OnToolUse) for -result-only/subagent mode, so this wraps AFTER buildHooks in both paths — when enabled the gate
	// is active even for subagents (deny-by-default, since they have no TTY), closing the otherwise-uncovered
	// autonomous path; when disabled ConfirmOnToolUse is the identity, leaving behavior unchanged.
	hooks.OnToolUse = policy.ConfirmOnToolUse(hooks.OnToolUse, policy.ConfirmCfg{
		Enabled:     confirmDestructive,
		AutoApprove: os.Getenv("MINIAGENT_AUTO_APPROVE") == "1",
	})
	return hooks
}
