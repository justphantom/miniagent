package main

import (
	"log/slog"

	"github.com/justphantom/miniagent/config"
	"github.com/justphantom/miniagent/miniagent"
	"github.com/justphantom/miniagent/miniagent/compaction"
	"github.com/justphantom/miniagent/miniagent/policy"
	"github.com/justphantom/miniagent/miniagent/session"
)

// loopCfg overrides flag defaults per resolved (cli>config) to construct LoopConfig (loop body + policy-carrier fields;
// compaction policies are attached via NewCompaction, other policies via NewDefault* hook factories, core Run has zero policies).
// resolved.System must already be non-empty (the production path guarantees this via assembleSystemPrompt at main.go:147);
// loopCfg does not re-default it — a missing System here is a caller bug, surfaced as an empty prompt rather than silently papered over.
func loopCfg(resolved *config.Resolved, maxIterDef int, streamDef bool, history []miniagent.Message, tools []miniagent.Tool) miniagent.LoopConfig {
	return miniagent.LoopConfig{
		Model:              resolved.ModelID,
		System:             resolved.System,
		SummaryRequest:     resolved.SummaryRequest,
		MaxTokens:          into(resolved.MaxTokens, 0),
		Tools:              tools,
		History:            history,
		MaxIterations:      into(resolved.Run.MaxIterations, maxIterDef),
		MaxTotalTokens:     into(resolved.RunConfig.MaxTotalTokens, 0),
		Stream:             intoBool(resolved.Run.Stream, streamDef),
		ThinkingLevel:      resolved.Thinking,
		Thinking:           resolved.Provider.Thinking,
		MaxToolResultChars: into(resolved.RunConfig.MaxToolResultChars, 0),
		MaxParallelTools:   into(resolved.RunConfig.MaxParallelTools, 0),
	}
}

// warnNoBudgetFuse (C3): run.max_tokens_total defaults to nil → OnBudget skips the cumulative-spend check entirely,
// so a long Run can burn unbounded tokens until repeated context-window compaction. The default-config single turn is
// already bounded by soft fuses (max_iterations=20, CompactionAuto default-on, optional max-duration); the real footgun
// is cross-turn -session resume accumulation and single Runs with iterations raised above 20. Warn (do NOT auto-pick a
// budget — that would decide for the user) only when the fuse is unset AND the run is long-session-prone.
func warnNoBudgetFuse(resolved *config.Resolved, sessionArg string, maxIterDef int, logger *slog.Logger) {
	iter := into(resolved.Run.MaxIterations, maxIterDef)
	if !shouldWarnBudgetFuse(resolved.RunConfig.MaxTotalTokens != nil, sessionArg, iter) {
		return
	}
	logger.Warn("run.max_tokens_total is not set: no cumulative token budget fuse",
		"resume", sessionArg != "", "max_iterations", iter,
		"hint", "set run.max_tokens_total to cap cumulative spend (especially across -session resumes)")
}

// shouldWarnBudgetFuse reports whether the C3 no-fuse warning should fire: the cumulative budget fuse is unset AND the
// run is long-session-prone — a cross-turn -session resume (the real cumulative-spend footgun) or a single Run with
// iterations raised above the default 20. Default single turns are left alone (soft fuses already bound them). Pure
// predicate for testability; 20 mirrors the maxIterations default.
func shouldWarnBudgetFuse(maxTotalTokensSet bool, session string, maxIterations int) bool {
	if maxTotalTokensSet {
		return false
	}
	return session != "" || maxIterations > 20
}

// compactionOptions assembles the resolved compaction policies into CompactionOptions. chat is the summarization client
// (use compChat if non-nil, otherwise fall back to main chat).
// compactionOptions assembles the resolved compaction policies into CompactionOptions. mainDoer is the
// summarization client fallback (used when compDoer is nil, i.e. compaction shares the main provider).
func compactionOptions(resolved *config.Resolved, meta session.SessionMeta, mainDoer, compDoer miniagent.Doer, system string, tools []miniagent.Tool, logger *slog.Logger) compaction.CompactionOptions {
	compClient := compDoer
	if compClient == nil {
		compClient = mainDoer
	}
	return compaction.CompactionOptions{
		Chat:                     compClient,
		ContextWindow:            into(resolved.ContextWindow, 0),
		Model:                    resolved.ModelID,
		CompactionModel:          resolved.CompactionModelID,
		System:                   system,
		Tools:                    tools,
		KeepRecent:               into(resolved.RunConfig.ContextKeepRecent, 0),
		KeepReasoning:            into(resolved.RunConfig.ContextKeepReasoning, 0),
		KeepToolArgs:             into(resolved.RunConfig.ContextKeepToolArgs, 0),
		KeepReasoningChars:       into(resolved.RunConfig.ContextKeepReasoningChars, 0),
		SummarizerPrompt:         resolved.SummarizerPrompt,
		SummaryCreateInstruction: resolved.SummaryCreateInstruction,
		SummaryUpdateInstruction: resolved.SummaryUpdateInstruction,
		SummaryTemplate:          resolved.SummaryTemplate,
		SummaryMaxChars:          into(resolved.RunConfig.SummaryMaxChars, 0),
		SummaryMaxTokens:         into(resolved.RunConfig.SummaryMaxTokens, 0),
		PreserveRecentTokens:     into(resolved.RunConfig.PreserveRecentTokens, 0),
		UseRealUsage:             intoBool(resolved.RunConfig.ContextUseRealUsage, true),
		Auto:                     resolved.CompactionAuto,
		Reserved:                 resolved.CompactionReserved,
		SessionID:                meta.ID,
		EstimateTokens:           policy.EstimateTokens,
		Logger:                   logger,
	}
}

func intoBool(ov *bool, def bool) bool {
	if ov != nil {
		return *ov
	}
	return def
}

// into parses *int overrides: if ov is non-nil use *ov, otherwise use def. Shared by loopCfg and main's buildTools.
func into(ov *int, def int) int {
	if ov != nil {
		return *ov
	}
	return def
}
