// budget_tail.go: retainedTail token budget helpers (preserveRecentTokens / jointTailBudget / estimateRoundTokens / summaryTailStart).

package compaction

import (
	"github.com/justphantom/miniagent/miniagent"
	"github.com/justphantom/miniagent/miniagent/policy"
)

// summaryTailStart returns the index in msgs immediately AFTER the first miniagent.KindSummary message (the new summary
// compactWithSummary emits), i.e. the start of the retained tail. Returns -1 when no KindSummary is present. Used by
// FitHistory's summarized branch to apply the steady-state reasoning strip to the tail subslice only — everything
// before the boundary (head + summaryMsg) is left untouched.
func summaryTailStart(msgs []miniagent.Message) int {
	for i, m := range msgs {
		if m.Kind == miniagent.KindSummary {
			return i + 1
		}
	}
	return -1
}

// preserveRecentTokens resolves the upper bound of the retainedTail token budget (ported from opencode preserveRecentBudget, §P1-E):
// budget.PreserveRecentTokens>0 returned directly; otherwise floor(budget.ContextWindow/tailBudgetFraction)
// clamped to [minPreserveRecentTokens, maxPreserveRecentTokens]; budget.ContextWindow<=0 returns 0 (disabled → pure round-count mode).
func preserveRecentTokens(budget ContextBudget) int {
	if budget.PreserveRecentTokens > 0 {
		return budget.PreserveRecentTokens
	}
	if budget.ContextWindow <= 0 {
		return 0
	}
	t := budget.ContextWindow / tailBudgetFraction
	if t < minPreserveRecentTokens {
		return minPreserveRecentTokens
	}
	if t > maxPreserveRecentTokens {
		return maxPreserveRecentTokens
	}
	return t
}

// jointTailBudget returns the joint token budget for retainedTail (§B): deducts incompressible portions from CW×4/5 —
// request-level system/schema/overhead + head round marginal + summary upper bound estimate — so tail proactively
// yields space to the incompressible summary. Takes min with preserveRecentTokens (user's tail will upper bound).
// headAdj refinement: when head is a single old KindSummary it is extracted as prevSummary and doesn't enter out, so headAdj=0.
func jointTailBudget(budget ContextBudget, headRounds []miniagent.Message) int {
	if budget.ContextWindow <= 0 {
		return preserveRecentTokens(budget)
	}
	target := budget.ContextWindow * 4 / 5
	reqOverhead := policy.EstimateTokens(nil, budget.System, budget.Tools)
	headAdj := 0
	if len(headRounds) != 1 || headRounds[0].Kind != miniagent.KindSummary {
		headAdj = estimateRoundTokens(headRounds)
	}
	maxChars := budget.SummaryMaxChars
	if maxChars <= 0 {
		maxChars = summaryMaxChars
	}
	summaryEstimate := maxChars/2 + policy.EnvelopePerMsgTokens
	avail := target - reqOverhead - headAdj - summaryEstimate
	return min(max(avail, 0), preserveRecentTokens(budget))
}

// estimateRoundTokens estimates the marginal tokens of a single round (content+reasoning+args+envelope), excluding
// system/schema global overhead — those two are request-level constants counted only once in the tail total, so per-round
// accumulation must use marginal estimation. Used by selectTailByTokens for accumulation.
func estimateRoundTokens(round []miniagent.Message) int {
	return policy.EstimateTokens(round, "", nil) - policy.SystemOverheadTokens
}
