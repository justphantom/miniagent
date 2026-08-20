package session

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/justphantom/miniagent/miniagent"
)

// TestMain preserves os.Exit semantics; the session upper limit is now injected per-test via the LoadSession/AppendMessages maxBytes parameter.
func TestMain(m *testing.M) {
	os.Exit(m.Run())
}

func sampleTranscript() []miniagent.Message {
	return []miniagent.Message{
		{Role: "user", Content: "look at a.txt"},
		{Role: "assistant", ToolCalls: []miniagent.ToolCall{{ID: "c1", Name: "read", Args: `{"path":"a.txt"}`}}},
		{Role: "tool", ToolCallID: "c1", Content: "1 │ hello"},
		{Role: "assistant", Content: "a.txt contains hello"},
	}
}

func writeLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Append→Load round-trip: messages are equal one by one, metadata is reproduced.
func TestSession_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	want := sampleTranscript()
	meta := SessionMeta{ID: "s", Model: "main/glm", Workdir: "/abs", Provider: "main"}
	if err := AppendMessages(path, meta, want); err != nil {
		t.Fatalf("AppendMessages: %v", err)
	}
	gotMeta, got, err := LoadSession(path)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round-trip mismatch:\n got %+v\nwant %+v", got, want)
	}
	if gotMeta.ID != "s" || gotMeta.Model != "main/glm" {
		t.Errorf("meta mismatch: %+v", gotMeta)
	}
}

// Missing file → (zero meta, nil, nil), equivalent to a new session.
func TestLoadSession_MissingFileReturnsNil(t *testing.T) {
	meta, msgs, err := LoadSession(filepath.Join(t.TempDir(), "nope.jsonl"))
	if err != nil || msgs != nil || meta.Type != "" {
		t.Errorf("got (%+v, %v, %v), want (zero meta, nil, nil)", meta, msgs, err)
	}
}

// A corrupt JSON line in the middle (valid lines sandwiched before and after) must error.
func TestLoadSession_CorruptJSONFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.jsonl")
	writeLines(t, path, `{"type":"session","id":"s"}`, `{not json`, `{"type":"message","role":"user","content":"after"}`)
	if _, _, err := LoadSession(path); err == nil {
		t.Fatal("expected error for corrupt JSON line in the middle")
	}
}

// A partial trailing line left by an append-only crash should be tolerated: the partial line is discarded and the preceding valid history is loaded.
func TestLoadSession_ToleratesCorruptTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tail.jsonl")
	writeLines(t, path,
		`{"type":"session","id":"s"}`,
		`{"type":"message","role":"user","content":"ok"}`,
		`{broken half line`)
	_, msgs, err := LoadSession(path)
	if err != nil {
		t.Fatalf("corrupt tail should be tolerated: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Content != "ok" {
		t.Errorf("expected 1 valid message before corrupt tail, got %+v", msgs)
	}
}

func TestLoadSession_UnknownRoleFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "role.jsonl")
	writeLines(t, path, `{"type":"message","role":"system","content":"x"}`)
	if _, _, err := LoadSession(path); err == nil {
		t.Fatal("expected error for unknown role")
	}
}

func TestLoadSession_ToolMissingCallIDFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tool.jsonl")
	writeLines(t, path, `{"type":"message","role":"tool","content":"x"}`)
	if _, _, err := LoadSession(path); err == nil {
		t.Fatal("expected error for tool message without tool_call_id")
	}
}

func TestLoadSession_OversizedFails(t *testing.T) {
	const maxSz = int64(1 << 20)
	path := filepath.Join(t.TempDir(), "big.jsonl")
	if err := os.WriteFile(path, make([]byte, maxSz+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadSession(path, maxSz); err == nil {
		t.Fatal("expected error for oversized session file")
	}
}

// tool_calls and tool pairing breakage must be rejected.
func TestLoadSession_DanglingToolCallFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dangling.jsonl")
	writeLines(t, path,
		`{"type":"message","role":"user","content":"q"}`,
		`{"type":"message","role":"assistant","content":"","tool_calls":[{"id":"c1","name":"read","args":"{}"}]}`)
	if _, _, err := LoadSession(path); err == nil {
		t.Fatal("expected error for dangling tool_call")
	}
}

func TestLoadSession_OrphanToolMessageFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "orphan.jsonl")
	writeLines(t, path,
		`{"type":"message","role":"user","content":"q"}`,
		`{"type":"message","role":"tool","tool_call_id":"cX","content":"x"}`)
	if _, _, err := LoadSession(path); err == nil {
		t.Fatal("expected error for orphan tool message")
	}
}

// kind=summary structured marker is readable (review v3 #2); role=user is persisted legally.
func TestLoadSession_KindSummaryRecognized(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sum.jsonl")
	if err := AppendMessages(path, SessionMeta{ID: "s"}, []miniagent.Message{
		{Role: "user", Kind: miniagent.KindSummary, Content: "[Previous Conversation Summary] xxx"},
	}); err != nil {
		t.Fatal(err)
	}
	_, msgs, err := LoadSession(path)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Kind != miniagent.KindSummary || msgs[0].Role != "user" {
		t.Errorf("summary kind not recognized: %+v", msgs)
	}
}

// P2-7: a single large message (>1MiB old single-line limit, <maxSessionBytes total limit) loads fine,
// no longer making the whole session unreadable due to scanner ErrTooLong with no append-only way to repair it.
func TestLoadSession_LargeSingleLineOK(t *testing.T) {
	const maxSz = int64(1 << 20)
	path := filepath.Join(t.TempDir(), "s.jsonl")
	big := strings.Repeat("x", int(maxSz/2))
	if err := AppendMessages(path, SessionMeta{ID: "s"}, []miniagent.Message{{Role: "user", Content: big}}, maxSz); err != nil {
		t.Fatalf("AppendMessages: %v", err)
	}
	_, msgs, err := LoadSession(path, maxSz)
	if err != nil {
		t.Fatalf("P2-7: large single-line load failed (scanner single-line limit insufficient): %v", err)
	}
	if len(msgs) != 1 || len(msgs[0].Content) != len(big) {
		t.Errorf("large single-line round-trip mismatch: %+v", msgs)
	}
}

// H3-1: a partial trailing line left by a crash (no trailing newline) must be truncated by AppendMessages; otherwise a blind O_APPEND appends the new
// message onto the partial line, and the next LoadSession tolerates the merged illegal line as the trailing line and loses the new message (the time after
// that it becomes mid-file corruption and fails permanently).
func TestAppendMessages_HealsPartialTrailingLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	// A valid line + a half line with no trailing newline (simulating a crash-interrupted append, last line has no \n).
	content := "{\"type\":\"session\",\"id\":\"s\"}\n" +
		"{\"type\":\"message\",\"role\":\"user\",\"content\":\"ok\"}\n" +
		"{\"type\":\"message\",\"role\":\"assistant\",\"content\":\"half"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := AppendMessages(path, SessionMeta{ID: "s"}, []miniagent.Message{{Role: "user", Content: "after"}}); err != nil {
		t.Fatalf("AppendMessages: %v", err)
	}
	_, msgs, err := LoadSession(path)
	if err != nil {
		t.Fatalf("LoadSession after heal: %v", err)
	}
	// The partial line "half" is truncated; ok + new after are kept. Without the fix the partial line and after merge into an illegal line tolerated as the trailing line, msgs only [ok].
	if len(msgs) != 2 || msgs[0].Content != "ok" || msgs[1].Content != "after" {
		t.Errorf("after heal msgs = %+v, want [ok, after] (partial line truncated, new message kept)", msgs)
	}
}

// R4-1: a file already exceeding maxBytes (including a crash partial line) must be rejected directly with zero modification — the ensureTrailingNewline
// slow path's LimitReader(mb+1) reads incompletely when size>mb and would mis-truncate and lose valid lines, so a size>mb guard is put in front (consistent with LoadSession).
func TestAppendMessages_OversizedFileRejectedUntouched(t *testing.T) {
	const mb = int64(1024)
	path := filepath.Join(t.TempDir(), "s.jsonl")
	line := "{\"type\":\"message\",\"role\":\"user\",\"content\":\"ok\"}\n"
	content := "{\"type\":\"session\",\"id\":\"s\"}\n" + strings.Repeat(line, 40) + "{half"
	if int64(len(content)) <= mb {
		t.Fatalf("fixture too small: %d bytes, need > %d", len(content), mb)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := AppendMessages(path, SessionMeta{ID: "s"}, []miniagent.Message{{Role: "user", Content: "after"}}, mb); err == nil {
		t.Fatal("file exceeding mb should be rejected")
	}
	// Key point: the file is not truncated (R4-1 regression — the old implementation would silently truncate and lose valid lines).
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != content {
		t.Errorf("file exceeding mb was modified (should have zero side effects): orig=%d bytes got=%d bytes", len(content), len(got))
	}
}
