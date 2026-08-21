package compaction

import (
	"testing"

	"github.com/justphantom/miniagent/miniagent/policy"
)

// P3-2: budget_tail.go's marginal constants mirror the default estimate contract (policy.EstimateTokens):
// estimateRoundTokens subtracts the request-level systemOverheadTokens, and estimateMessageTokensLocal adds the
// per-message/per-tool-call envelope. If policy adjusts the overheads, this test fails instead of silently
// drifting the tail budget (§B). Keeping the mirror local (rather than importing) preserves the existing
// "replacement callback sharing this contract stays compatible" comment contract; the test pins them together.
func TestMirroredConstantsMatchPolicy(t *testing.T) {
	if systemOverheadTokens != policy.SystemOverheadTokens {
		t.Errorf("systemOverheadTokens = %d, policy.SystemOverheadTokens = %d", systemOverheadTokens, policy.SystemOverheadTokens)
	}
	if envelopePerMsgTokens != policy.EnvelopePerMsgTokens {
		t.Errorf("envelopePerMsgTokens = %d, policy.EnvelopePerMsgTokens = %d", envelopePerMsgTokens, policy.EnvelopePerMsgTokens)
	}
	if envelopePerToolCallTokens != policy.EnvelopePerToolCallTokens {
		t.Errorf("envelopePerToolCallTokens = %d, policy.EnvelopePerToolCallTokens = %d", envelopePerToolCallTokens, policy.EnvelopePerToolCallTokens)
	}
}
