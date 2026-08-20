// split.go: middle-segment splitting and lossy compaction.

package compaction

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/justphantom/miniagent/miniagent"
	"github.com/justphantom/miniagent/miniagent/session"
	"github.com/justphantom/miniagent/text"
)

// applyCompactionBarrier locates the most recent Kind=="summary" message and returns it and all messages after it;
// the older history before it (including older summaries) does not enter the context but remains in the session
// file. When there is no summary it returns the input as-is.
func applyCompactionBarrier(msgs []miniagent.Message) []miniagent.Message {
	for i := range slices.Backward(msgs) {
		if msgs[i].Kind == miniagent.KindSummary {
			return msgs[i:]
		}
	}
	return msgs
}

// selectTailByTokens accumulates the tail from the most recent round by token budget (ported from opencode select, §P1-E).
// maxTurns=keepRecent (round-count upper bound); tokenBudget=preserveRecentTokens(...). Flow: accumulate
// estimateRoundTokens(round) backward from the most recent round (marginal, excluding system/schema); rounds that
// fit entirely go into the tail; the first boundary round that does not fit is passed to splitRoundByTokens to find
// a safe split point (split off the suffix into the tail); if it cannot be split (a tool-call round), fall back to
// shrinkRoundToolContents to compress the tool content to fit the remaining budget and include it in the tail; if
// that still fails the whole round goes to middle; everything before the boundary round goes to middle.
// tokenBudget<=0 degrades to a pure "most recent maxTurns rounds" round-count mode (backward compatible). The
// returned tail and middle are both flat []miniagent.Message (original order).
func selectTailByTokens(rounds [][]miniagent.Message, maxTurns, tokenBudget int) (tail, middle []miniagent.Message) {
	n := len(rounds)
	if n == 0 {
		return nil, nil
	}
	if tokenBudget <= 0 {
		cnt := min(maxTurns, n)
		return flatten(rounds[n-cnt:]), flatten(rounds[:n-cnt])
	}
	total := 0
	// tailStart defaults to 0: if all rounds fit (n<=maxTurns and the token limit is not hit, the loop ends normally)
	// then tail=all and middle=empty. On break inside the loop it is overwritten to i+1 (tail=rounds[i+1..]).
	tailStart := 0
	boundary := -1 // index of the token boundary round (needs a split/shrink decision); -1=none (all fit, or only maxTurns truncation)
	for i := n - 1; i >= 0; i-- {
		if n-i > maxTurns {
			tailStart = i + 1 // maxTurns truncation: tail=rounds[i+1..], older goes to middle
			break
		}
		size := estimateRoundTokens(rounds[i])
		// The most recent round (i==n-1) is force-included in the tail even if it alone exceeds tokenBudget: the
		// recent context cannot be dropped; pushing it into middle would get it summarized away, making the model
		// lose precise recent context. So only when i<n-1 do we take the boundary round at the budget overflow.
		if i < n-1 && total+size > tokenBudget {
			boundary = i
			tailStart = i + 1
			break
		}
		total += size
	}
	tail = flatten(rounds[tailStart:n])
	middle = flatten(rounds[:tailStart])
	if boundary >= 0 {
		// The boundary round tries to split/shrink and prepend to the tail; on success that round (post-compression) does not enter middle.
		remaining := tokenBudget - total
		if fitted, ok := splitOrShrinkToRound(rounds[boundary], remaining); ok {
			tail = append(append([]miniagent.Message{}, fitted...), tail...)
			middle = flatten(rounds[:boundary]) // boundary round already included in tail, removed from middle
		}
		// split/shrink failure → the boundary round stays wholly in middle (rounds[:tailStart]=rounds[:boundary+1] already includes it), as expected.
	}
	// Invariant: the tail contains at least the most recent round. This fallback covers two degenerate paths that
	// once produced an empty tail — the most recent round alone exceeds tokenBudget and boundary split/shrink fails
	// (already guarded out by the i<n-1 check above, double-protected here), or maxTurns<=0 hits the maxTurns
	// truncation at i==n-1. Letting the most recent round enter middle to be summarized is always a semantic error;
	// a tail slightly over budget is preferable.
	if len(tail) == 0 && n > 0 {
		tail = flatten(rounds[n-1:])
		middle = flatten(rounds[:n-1])
	}
	return tail, middle
}

// splitOrShrinkToRound is the adaptation entry for the boundary round: shrinkRoundToolContents compresses the tool
// content to fit remaining. It returns (fitted, ok); ok=false means the boundary round cannot be included in the
// tail and should go wholly to middle.
// (The former splitRoundByTokens is deleted: miniagent splitRounds makes text rounds single-message and tool-call
// rounds [A(tc)+tools]; within a single round there is no safe non-tool message boundary to split on, so that
// function always returned nil for production rounds — deleted as YAGNI, which also removes the hidden danger of
// its success path dropping the boundary round's prefix.)
func splitOrShrinkToRound(round []miniagent.Message, remaining int) ([]miniagent.Message, bool) {
	if remaining <= 0 {
		return nil, false
	}
	shrunk := shrinkRoundToolContents(round, remaining)
	if estimateRoundTokens(shrunk) <= remaining {
		return shrunk, true
	}
	return nil, false
}

// shrinkRoundToolContents is the semantic equivalent of opencode splitTurn under the miniagent flat model (§P1-E,
// the required compensation after REFUTED): when a tool-call round cannot be split, it truncates the tool result
// content in-place within the round to fit tokenBudget (deep copy, never touches the input, pairing unchanged).
// It scales each tool content's char count by the ratio of the round's current estimateRoundTokens to tokenBudget
// (reusing text.TruncateHeadTail head 1/4 + tail 3/4). tokenBudget<=0 returns a copy as-is; with no tool messages
// compression is pointless but a copy is still returned (the caller judges fit).
func shrinkRoundToolContents(round []miniagent.Message, tokenBudget int) []miniagent.Message {
	out := make([]miniagent.Message, len(round))
	copy(out, round)
	if tokenBudget <= 0 {
		return out
	}
	cur := estimateRoundTokens(out)
	if cur <= tokenBudget {
		return out
	}
	ratio := float64(tokenBudget) / float64(cur)
	if ratio > 1 {
		ratio = 1
	}
	for i, m := range out {
		if m.Role == miniagent.RoleTool && len(m.Content) > 0 {
			newLen := int(float64(len([]rune(m.Content))) * ratio)
			newLen = max(1, newLen)
			out[i].Content = text.TruncateHeadTail(m.Content, newLen, "…[tool result compressed]")
		}
	}
	return out
}

// compactWithSummary retains the earliest 1 round + the most recent keepRecent rounds, and summarizes the middle
// segment into a single miniagent.KindSummary message. It returns (out, summary, usage, err): out contains the new
// summary; summary is that message (.Kind=="" means no middle / failure); on a broken middle-segment pairing or
// summarization failure it returns an error (the caller FitHistory falls back to compactHistory). When there is no
// middle to summarize it returns (msgs, miniagent.Message{}, miniagent.Usage{}, nil). It no longer takes newMsgs —
// persistence insertion is done by Run.
//
// Cross-turn inheritance (P2-1 + §P0-A UPDATE): the old summary brought in by the previous LoadSession lands at the
// head of msgs via applyCompactionBarrier, and splitRounds makes it a standalone rounds[0]. Both the default and
// override paths extract the old summary text as previousSummary (stripSummaryPrefix) and pass it down through the
// Summarize callback (UPDATE mode); head is set to nil and the old summary is never merged into middle — this
// halves tokens (the old summary is no longer re-read and re-written as history) and the explicit preserve
// instruction lowers the chance of dropping details. The override path's previousSummary is consumed by
// buildSummarizerSystem (the {previous_summary} placeholder is substituted if present, otherwise the same
// <previous-summary> block the default UPDATE path uses is appended), so a custom prompt is a strict superset of
// default rather than regressing to the old dilutive re-read/re-write.
//
// On the next turn, after the barrier hits the new summary, the old summary is dropped; a non-summary first round
// (a normal user round) keeps the original behavior.
func compactWithSummary(ctx context.Context, budget ContextBudget, msgs []miniagent.Message, keepRecent int) (out []miniagent.Message, summary miniagent.Message, usage miniagent.Usage, err error) {
	// FitHistory is an exported function; a direct caller may forget to set Summarize — when nil, summarization is
	// impossible, return an error so FitHistory falls back to lossy compaction (NewCompaction always sets Summarize
	// internally, the production path never hits this branch).
	if budget.Summarize == nil {
		return msgs, miniagent.Message{}, miniagent.Usage{}, errors.New("ContextBudget.Summarize is not configured, cannot summarize")
	}
	rounds := splitRounds(msgs)
	if len(rounds) <= 1+keepRecent {
		return msgs, miniagent.Message{}, miniagent.Usage{}, nil // no middle to summarize
	}
	head := rounds[0]
	// §B: the tail budget is now a joint budget (jointTailBudget) — after deducting summary+head+request overhead
	// from CW×4/5, whatever remains goes to the tail, so the non-compressible summary is prioritized and the tail
	// proactively yields, reducing post-compaction over-window trim/abort on medium CW. tokenBudget<=0 falls back to
	// pure round-count mode (backward compatible with old sessions / no window). §P1-E boundary-round fine-split
	// (split/shrink) semantics unchanged.
	tokenBudget := jointTailBudget(budget, head)
	tail, middleCore := selectTailByTokens(rounds[1:], keepRecent, tokenBudget)
	prevSummary := ""
	if len(head) == 1 && head[0].Kind == miniagent.KindSummary {
		// Both default and override paths extract the old summary as the UPDATE anchor (previousSummary) and do NOT
		// merge it into middle for re-read/re-write — this halves tokens (the old summary is no longer re-read and
		// re-written as history) and the explicit preserve instruction lowers the chance of dropping details. The
		// override path's previousSummary is consumed by buildSummarizerSystem (placeholder substitution or an
		// appended <previous-summary> block), making override a strict superset of default rather than regressing to
		// the old dilutive re-read/re-write.
		prevSummary = stripSummaryPrefix(head[0].Content)
		head = nil
	}
	middle := middleCore
	if len(middle) == 0 {
		return msgs, miniagent.Message{}, miniagent.Usage{}, nil
	}
	// The middle segment must be self-contained pairing-wise: otherwise substituting it with a summary would leave
	// orphaned tool_call/tool messages, and continuation would be rejected by the endpoint with a 400.
	if err := session.ValidateToolPairing(middle); err != nil {
		return msgs, miniagent.Message{}, miniagent.Usage{}, fmt.Errorf("middle segment pairing is broken, cannot summarize safely: %w", err)
	}
	// Before summarizing, apply full strip to middle (keepN=0): middle is all non-recent objects about to be
	// lossily summarized — clear redundant reasoning / fold superseded reads / compress write-edit args — saving
	// summary input tokens (measured ~56%) and preventing middle+summaryMaxTokens from exceeding the summary
	// model's CW. applyContextStrips only mutates fields, never deletes messages, never touches ToolCallID, so
	// pairing is unchanged (ValidateToolPairing above already passed). logger=nil → dbg=false strips at zero
	// overhead. The UPDATE old summary goes via prevSummary and never enters middle (both default and override).
	middle = applyContextStrips(ctx, middle, 0, 0, 0, nil, budget.System, budget.Tools)
	compModel := budget.CompactionModel
	if compModel == "" {
		compModel = budget.Model
	}
	// §P2: trigger the compaction hook before the summary LLM call (inject context / one-shot replace summarizerPrompt).
	// It must run after session.ValidateToolPairing(middle) passes: context injection only appends user messages
	// without tool_calls, so it does not break pairing.
	effPrompt, effMiddle, herr := applyCompactingHook(ctx, budget.Compacting, budget.SessionID, compModel, budget.SummarizerPrompt, middle)
	if herr != nil {
		return msgs, miniagent.Message{}, miniagent.Usage{}, herr // implementation A: a hook error is propagated upward to abort compaction
	}
	sumText, sumUsage, err := budget.Summarize(ctx, compModel, effPrompt, prevSummary, effMiddle)
	if err != nil {
		return msgs, miniagent.Message{}, miniagent.Usage{}, err
	}
	// §P0-B: summaryMsg is explicitly stamped with a new Ts (text.NowMs(), not via appendMsg) — the key trigger point
	// for anti-staleness: the new Ts invalidates the real usage of preceding assistants in the next round's
	// estimateTokensFromUsage (lastApplicableUsageIndex's latestSummaryTs is raised), forcing a fallback to local
	// estimation to recompute the small post-compaction history, avoiding an immediate second compaction driven by
	// the stale large usage.
	summaryMsg := miniagent.Message{Role: miniagent.RoleUser, Kind: miniagent.KindSummary, Content: summaryPrefix + sumText, Ts: text.NowMs()}
	out = append([]miniagent.Message{}, head...)
	out = append(out, summaryMsg)
	out = append(out, tail...)
	return out, summaryMsg, sumUsage, nil
}
