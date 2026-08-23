package main

// web_bus_test.go covers the turn event bus in isolation: fan-out ordering, late-subscriber
// replay, lagging-subscriber eviction, buffer truncation and lifecycle broadcast. The
// end-to-end decoupling behavior (disconnect/stop) lives in web_turn_test.go.

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"
)

func newTestBus() (*turnRegistry, *turnEntry) {
	r := newTurnRegistry()
	e, busy := r.register("sess-1", func() {}, nil)
	if busy {
		panic("fresh registry busy")
	}
	return r, e
}

// Every subscriber sees the exact same full line sequence, in order, no loss. Subscribers
// drain concurrently with the writer. Line count stays under turnSubchanBuffer so no
// subscriber can be evicted mid-test — eviction pressure has its own test below.
func TestTurnBus_FanOutIdentical(t *testing.T) {
	r, e := newTestBus()
	_, sub1, _ := e.subscribe()
	_, sub2, _ := e.subscribe()
	collect := func(ch chan string) []string {
		var out []string
		for line := range ch {
			out = append(out, strings.TrimRight(line, "\n"))
		}
		return out
	}
	done1, done2 := make(chan []string, 1), make(chan []string, 1)
	go func() { done1 <- collect(sub1) }()
	go func() { done2 <- collect(sub2) }()
	const n = turnSubchanBuffer - 10
	for i := range n {
		_, _ = e.Write([]byte(`{"type":"tool_use","n":` + strconv.Itoa(i) + "}\n"))
	}
	// newline-split payload: the bus must stitch partial writes into one line
	_, _ = e.Write([]byte(`{"type":"re`))
	_, _ = e.Write([]byte("sult\"}\n"))
	r.finish("sess-1", nil)

	a, b := <-done1, <-done2
	if len(a) != n+1 || len(b) != n+1 {
		t.Fatalf("subscriber line counts = %d / %d, want %d", len(a), len(b), n+1)
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("divergence at %d: %q vs %q", i, a[i], b[i])
		}
	}
	if a[n] != `{"type":"result"}` {
		t.Errorf("last line = %q, want stitched result line", a[n])
	}
	// Subscribe after finish must report not-ok: the replay lives on in the session file.
	if _, _, ok := e.subscribe(); ok {
		t.Error("subscribe after finish should report not-ok")
	}
}

// A mid-stream subscriber gets the full replay then continues live: no gap, no duplicate.
func TestTurnBus_MidSubscriberReplayPlusLive(t *testing.T) {
	r, e := newTestBus()
	for i := range 5 {
		_, _ = e.Write([]byte(`{"n":` + strconv.Itoa(i) + "}\n"))
	}
	replay, ch, ok := e.subscribe()
	if !ok {
		t.Fatal("subscribe should succeed while running")
	}
	if len(replay) != 5 {
		t.Fatalf("replay = %d lines, want 5", len(replay))
	}
	_, _ = e.Write([]byte(`{"n":5}` + "\n"))
	r.finish("sess-1", nil)
	var live []string
	for line := range ch {
		live = append(live, strings.TrimRight(line, "\n"))
	}
	if len(live) != 1 || live[0] != `{"n":5}` {
		t.Fatalf("live = %v, want the single post-subscribe line", live)
	}
}

// A subscriber that never drains is closed by the fan-out; the engine keeps writing.
func TestTurnBus_LaggingSubscriberEvicted(t *testing.T) {
	r, e := newTestBus()
	_, ch, ok := e.subscribe()
	if !ok {
		t.Fatal("subscribe failed")
	}
	for i := range turnSubchanBuffer + 50 {
		if _, err := e.Write([]byte(`{"n":` + strconv.Itoa(i) + "}\n")); err != nil {
			t.Fatalf("engine write failed on lagging subscriber: %v", err)
		}
	}
	r.finish("sess-1", nil)
	// The lagging channel is closed (drains its buffer then ends) — no deadlock, engine survived.
	seen := 0
	deadline := time.After(2 * time.Second)
	for range ch {
		seen++
		if seen > turnSubchanBuffer {
			break
		}
		select {
		case <-deadline:
			t.Fatal("lagging channel never closed")
		default:
		}
	}
}

// Buffer over cap drops the oldest lines and late subscribers see an honest marker.
func TestTurnBus_TruncatedReplay(t *testing.T) {
	r, e := newTestBus()
	for i := range maxTurnBufferEvents + 10 {
		_, _ = e.Write([]byte(`{"n":` + strconv.Itoa(i) + "}\n"))
	}
	replay, ch, _ := e.subscribe()
	r.finish("sess-1", nil)
	if len(replay) != maxTurnBufferEvents+1 { // marker + capped buffer
		t.Fatalf("replay = %d lines, want cap+marker", len(replay))
	}
	var m struct {
		Type    string `json:"type"`
		Dropped int    `json:"dropped"`
	}
	if err := json.Unmarshal([]byte(replay[0]), &m); err != nil {
		t.Fatalf("marker line %q: %v", replay[0], err)
	}
	if m.Type != "live_truncated" || m.Dropped != 10 {
		t.Errorf("marker = %+v, want dropped=10", m)
	}
	for range ch { // drain
	}
}

// Write never returns an error — a dead subscriber must not abort the turn via OnToolUse.
func TestTurnBus_WriteNeverErrors(t *testing.T) {
	_, e := newTestBus()
	if _, err := e.Write([]byte("garbage without newline")); err != nil {
		t.Fatalf("partial write errored: %v", err)
	}
	if _, err := e.Write([]byte(" more garbage")); err != nil {
		t.Fatalf("second partial write errored: %v", err)
	}
}

// Lifecycle events reach global subscribers; a lagging one is evicted without panic.
func TestTurnBus_LifecycleBroadcast(t *testing.T) {
	r, _ := newTestBus()
	ch := r.subscribeLife()
	r.broadcastLife(lifeEvent("turn_started", "sess-1", ""))
	r.broadcastLife(turnFinishedEvent("sess-1", nil))
	r.broadcastLife(turnFinishedEvent("sess-2", context.Canceled))
	var lines []string
	for range 3 {
		select {
		case l := <-ch:
			lines = append(lines, strings.TrimRight(l, "\n"))
		case <-time.After(time.Second):
			t.Fatal("lifecycle event lost")
		}
	}
	var ev struct {
		Type    string `json:"type"`
		Session string `json:"session"`
		OK      bool   `json:"ok"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal([]byte(lines[1]), &ev); err != nil {
		t.Fatalf("turn_finished: %v", err)
	}
	if ev.Type != "turn_finished" || ev.Session != "sess-1" || !ev.OK {
		t.Errorf("ok event = %+v", ev)
	}
	ev = struct {
		Type    string `json:"type"`
		Session string `json:"session"`
		OK      bool   `json:"ok"`
		Error   string `json:"error"`
	}{}
	if err := json.Unmarshal([]byte(lines[2]), &ev); err != nil {
		t.Fatalf("failed turn_finished: %v", err)
	}
	if ev.OK || ev.Error == "" {
		t.Errorf("error event = %+v, want ok=false with message", ev)
	}
	r.unsubscribeLife(ch)
}

// delete reservation blocks turns; finish releases the slot for a new turn.
func TestTurnBus_DeleteReservation(t *testing.T) {
	r, _ := newTestBus()
	r.finish("sess-1", nil)
	if r.beginDelete("sess-1") {
		t.Fatal("beginDelete busy after finish")
	}
	if _, busy := r.register("sess-1", func() {}, nil); !busy {
		t.Fatal("register should be busy during delete")
	}
	r.finish("sess-1", nil)
	if _, busy := r.register("sess-1", func() {}, nil); busy {
		t.Fatal("register busy after delete finished")
	}
}
