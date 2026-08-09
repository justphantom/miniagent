// Package metrics provides the default consumer of the miniagent.LoopHooks.OnStep observability seam: a best-effort NDJSON
// emitter that writes one line per step, turning the long-session behavior the docs could previously only argue (transcript
// growth, token-spend slope, compaction count, per-turn LLM request count) into collectible curves.
package metrics //nolint:revive // var-naming: name is deliberate; no stdlib import collision

import (
	"context"
	"encoding/json"
	"io"
	"sync"
	"time"

	"github.com/justphantom/miniagent/internal/miniagent"
)

// StepEmitter writes one NDJSON line per OnStep snapshot to w. Best-effort: a write error (e.g. a closed sink) is swallowed,
// so the metrics sink can never terminate the agent loop (OnStep is observe-only). Lines are JSON objects carrying the
// snapshot fields plus a wall-clock ts (UTC ms).
type StepEmitter struct {
	mu  sync.Mutex
	enc *json.Encoder
	now func() int64 // UTC ms
}

// NewStepEmitter returns an emitter writing to w. The returned Emit method matches miniagent.LoopHooks.OnStep, so it can be
// assigned directly: hooks.OnStep = metrics.NewStepEmitter(w).Emit.
func NewStepEmitter(w io.Writer) *StepEmitter {
	return &StepEmitter{enc: json.NewEncoder(w), now: func() int64 { return time.Now().UnixMilli() }}
}

// Emit writes one NDJSON line for snap. It is the OnStep hook body (observe-only; write errors swallowed).
func (e *StepEmitter) Emit(_ context.Context, snap miniagent.StepSnapshot) {
	e.mu.Lock()
	defer e.mu.Unlock()
	_ = e.enc.Encode(struct { //nolint:musttag // StepSnapshot is serialized via field-name promotion (contract: capitalized keys)
		Type string `json:"type"`
		Ts   int64  `json:"ts"`
		miniagent.StepSnapshot
	}{Type: "step", Ts: e.now(), StepSnapshot: snap})
}
