package metrics //nolint:revive // var-naming: package name is deliberate; stdlib collision is benign

import (
	"context"
	"strings"
	"testing"

	"github.com/justphantom/miniagent/miniagent"
)

// StepEmitter writes one NDJSON line per snapshot (best-effort), each carrying type=step, a ts, and the snapshot fields.
func TestStepEmitter_EmitsNDJSON(t *testing.T) {
	var sb strings.Builder
	e := NewStepEmitter(&sb)
	e.Emit(context.Background(), miniagent.StepSnapshot{Step: 1, TranscriptLen: 5, InputTokens: 100, OutputTokens: 20})
	e.Emit(context.Background(), miniagent.StepSnapshot{Step: 2, TranscriptLen: 8, InputTokens: 250, OutputTokens: 45, LLMRequests: 1, Compacted: true})
	out := sb.String()
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines = %d, want 2: %q", len(lines), out)
	}
	if !strings.Contains(lines[0], `"type":"step"`) || !strings.Contains(lines[0], `"Step":1`) || !strings.Contains(lines[0], `"TranscriptLen":5`) {
		t.Errorf("line1 = %q", lines[0])
	}
	if !strings.Contains(lines[1], `"Compacted":true`) || !strings.Contains(lines[1], `"LLMRequests":1`) || !strings.Contains(lines[1], `"ts":`) {
		t.Errorf("line2 = %q", lines[1])
	}
}
