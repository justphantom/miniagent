package main

// web_bus.go is the turn event bus: turns run decoupled from the HTTP request that started
// them (D1), so a client disconnect, tab close or view switch no longer cancels the agent.
// The engine streams NDJSON into a turnEntry; the entry buffers every line (late subscribers
// get a full replay) and fans out to live subscribers. Write always returns nil — an emit
// error would abort the turn (OnToolUse errors terminate the loop), so a dead subscriber must
// never be allowed to kill the engine. Lifecycle transitions are broadcast to the global
// /api/events stream so every browser can sync its session list and running state.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
)

// maxTurnBufferEvents caps the per-turn replay buffer (lines). Beyond it the oldest lines are
// dropped and late subscribers see a live_truncated marker with the drop count — honest
// truncation instead of unbounded memory on pathological turns.
const maxTurnBufferEvents = 20000

// turnSubchanBuffer is the per-subscriber channel capacity. A subscriber that falls this far
// behind (slow client) is closed by the fan-out; its handler writes the stream_cut marker and
// the client rebuilds its view via /live or session replay (D3). Sized to absorb delta bursts
// (the terminal result batch in particular) so a momentarily slow client self-drains instead
// of tripping the cut on every turn.
const turnSubchanBuffer = 256

// streamCutLine is the non-terminal cut marker written to a lag-closed subscriber: the turn
// keeps running (or has finished) server-side; the client must rebuild via /live or replay.
// NOT a terminal event — consumers must not treat it as result/error/stop.
const streamCutLine = "{\"type\":\"stream_cut\"}\n"

// turnEntry is one reserved session slot: a running turn (kind "running") or a delete in
// progress (kind "deleting" — blocks new turns while the file is removed).
type turnEntry struct {
	kind   string
	id     string // session id (logging/observability)
	cancel context.CancelFunc
	warn   func(msg string, args ...any) // nil-safe lag-cut logger (engine's slog)

	mu      sync.Mutex
	buf     []string // every NDJSON line of this turn, in order
	dropped int      // lines evicted from buf head (over maxTurnBufferEvents)
	partial []byte   // Write payload not yet terminated by '\n'
	subs    map[chan string]struct{}
	done    chan struct{}
	err     error // read-only after done closes
}

// turnRegistry maps session id → in-flight entry, plus the global lifecycle subscriber set.
type turnRegistry struct {
	mu      sync.Mutex
	m       map[string]*turnEntry
	lifeMu  sync.Mutex
	lifeSub map[chan string]struct{}
}

func newTurnRegistry() *turnRegistry {
	return &turnRegistry{m: map[string]*turnEntry{}, lifeSub: map[chan string]struct{}{}}
}

// register reserves the session for a running turn. busy=true when any entry (running or
// deleting) exists — the caller answers 409, preserving the N7 same-session semantics.
// warn is the engine logger's Warn (nil-safe in tests).
func (r *turnRegistry) register(id string, cancel context.CancelFunc, warn func(string, ...any)) (*turnEntry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.m[id]; ok {
		return nil, true
	}
	e := &turnEntry{kind: "running", id: id, cancel: cancel, warn: warn, subs: map[chan string]struct{}{}, done: make(chan struct{})}
	r.m[id] = e
	return e, false
}

// beginDelete reserves the session for deletion: new turns are rejected while the remove is
// in flight, exactly like the old TryLock on the per-session mutex.
func (r *turnRegistry) beginDelete(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.m[id]; ok {
		return true
	}
	r.m[id] = &turnEntry{kind: "deleting", done: make(chan struct{})}
	return false
}

// finish releases the slot: stores the terminal error, wakes done watchers and closes every
// subscriber channel (buffered lines remain readable — subscribers drain, then end). The
// entry stays reserved until runTurn has returned (saveSession included), so a follow-up turn
// on the same session cannot race the final RewriteMessages.
func (r *turnRegistry) finish(id string, err error) {
	r.mu.Lock()
	e := r.m[id]
	delete(r.m, id)
	r.mu.Unlock()
	if e == nil {
		return
	}
	e.mu.Lock()
	e.err = err
	for ch := range e.subs {
		close(ch)
		delete(e.subs, ch)
	}
	close(e.done)
	e.mu.Unlock()
}

// running returns the entry when a turn is in flight on id.
func (r *turnRegistry) running(id string) (*turnEntry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.m[id]
	if !ok || e.kind != "running" {
		return nil, false
	}
	return e, true
}

// Write implements io.Writer for the engine's NDJSON sink. Payloads are split on newlines so
// partial writes are stitched; each complete line is buffered and fanned out under one lock,
// which makes subscribe's snapshot+register atomic (no gap, no duplicate).
func (e *turnEntry) Write(p []byte) (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.partial = append(e.partial, p...)
	for {
		i := bytes.IndexByte(e.partial, '\n')
		if i < 0 {
			break
		}
		line := string(e.partial[:i+1])
		e.partial = e.partial[i+1:]
		if len(line) <= 1 {
			continue // stray empty line — nothing to show
		}
		e.buf = append(e.buf, line)
		if len(e.buf) > maxTurnBufferEvents {
			e.buf = e.buf[1:]
			e.dropped++
		}
		for ch := range e.subs {
			select {
			case ch <- line:
			default:
				// lagging subscriber: close it — the handler writes stream_cut and the client
				// rebuilds via /live or replay (D3). Observable via warn: this was the
				// invisible cause of "连接中断" reports (server fine, stream silently cut).
				if e.warn != nil {
					e.warn("subscriber lag-closed", "session", e.id, "buf", len(e.buf))
				}
				close(ch)
				delete(e.subs, ch)
			}
		}
	}
	return len(p), nil
}

// subscribe returns the replay snapshot plus a live channel. Called under e.mu together with
// the registration, so the snapshot ends exactly where live events begin.
func (e *turnEntry) subscribe() (replay []string, ch chan string, ok bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	select {
	case <-e.done:
		return nil, nil, false
	default:
	}
	ch = make(chan string, turnSubchanBuffer)
	e.subs[ch] = struct{}{}
	if e.dropped > 0 {
		replay = append(replay, `{"type":"live_truncated","dropped":`+strconv.Itoa(e.dropped)+"}\n")
	}
	replay = append(replay, e.buf...)
	return replay, ch, true
}

func (e *turnEntry) unsubscribe(ch chan string) {
	e.mu.Lock()
	// not closed here: only the fan-out (single writer) closes subscriber channels
	delete(e.subs, ch)
	e.mu.Unlock()
}

// result returns the terminal error after done closes (nil on success).
func (e *turnEntry) result() error {
	<-e.done
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.err
}

// subscribeLife registers a global lifecycle listener (/api/events). Same lagging policy as
// turn subscribers: a stalled client is closed and reconnects.
func (r *turnRegistry) subscribeLife() chan string {
	ch := make(chan string, turnSubchanBuffer)
	r.lifeMu.Lock()
	r.lifeSub[ch] = struct{}{}
	r.lifeMu.Unlock()
	return ch
}

func (r *turnRegistry) unsubscribeLife(ch chan string) {
	r.lifeMu.Lock()
	delete(r.lifeSub, ch)
	r.lifeMu.Unlock()
}

// broadcastLife fans a pre-serialized NDJSON lifecycle line to every global subscriber.
func (r *turnRegistry) broadcastLife(line string) {
	r.lifeMu.Lock()
	defer r.lifeMu.Unlock()
	for ch := range r.lifeSub {
		select {
		case ch <- line:
		default:
			close(ch)
			delete(r.lifeSub, ch)
		}
	}
}

// lifeEvent serializes a lifecycle event line (called on every turn start/finish).
func lifeEvent(typ, id string, extra string) string {
	if extra == "" {
		return fmt.Sprintf("{\"type\":%q,\"session\":%q}\n", typ, id)
	}
	return fmt.Sprintf("{\"type\":%q,\"session\":%q,%s}\n", typ, id, extra)
}

// turnFinishedEvent carries ok/error so list-refreshing clients can surface failed turns.
func turnFinishedEvent(id string, err error) string {
	if err == nil {
		return lifeEvent("turn_finished", id, `"ok":true`)
	}
	msg, _ := json.Marshal(err.Error())
	return lifeEvent("turn_finished", id, `"ok":false,"error":`+string(msg))
}
