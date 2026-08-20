package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/justphantom/miniagent/miniagent"
	"github.com/justphantom/miniagent/miniagent/session"
)

// parseNDJSON parses the combined output line-by-line into a list of events, skipping empty lines
// and non-JSON (such as slog log lines).
func parseNDJSON(t *testing.T, out string) []map[string]any {
	t.Helper()
	var events []map[string]any
	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.HasPrefix(line, "{") {
			continue
		}
		var e map[string]any
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue
		}
		events = append(events, e)
	}
	return events
}

func equalSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// replaySession unit test: known message sequence → exact event sequence. Covers:
//   - assistant with tool_calls+reasoning (step1: tool_use + reasoning_delta, no text_delta);
//   - tool messages resolve the tool name via the callID→name map and emit tool_result;
//   - terminal assistant plain text (step2: text_delta);
//   - multi-step usage accumulation, result aggregation (steps/text/finish/model/usage).
func TestReplaySession(t *testing.T) {
	meta := session.SessionMeta{
		Type: "session", ID: "s1", Model: "p/m", Provider: "p", Workdir: "/wd", Created: "2026-01-01T00:00:00Z",
	}
	msgs := []miniagent.Message{
		{Role: miniagent.RoleUser, Content: "question"},
		{Role: miniagent.RoleAssistant, Reasoning: "let me think", ToolCalls: []miniagent.ToolCall{{ID: "c1", Name: "read", Args: `{"path":"/x"}`}}, Usage: &miniagent.Usage{InputTokens: 10, OutputTokens: 5}},
		{Role: miniagent.RoleTool, ToolCallID: "c1", Content: "file content"},
		{Role: miniagent.RoleAssistant, Content: "final answer", Usage: &miniagent.Usage{InputTokens: 20, OutputTokens: 8}},
	}
	var buf bytes.Buffer
	if err := replaySession(&buf, meta, msgs); err != nil {
		t.Fatalf("replaySession: %v", err)
	}
	ev := parseNDJSON(t, buf.String())

	wantTypes := []string{"session", "tool_use", "reasoning_delta", "tool_result", "text_delta", "result"}
	if !equalSlice(eventTypes(ev), wantTypes) {
		t.Fatalf("event types = %v, want %v", eventTypes(ev), wantTypes)
	}
	if ev[0]["id"] != "s1" || ev[0]["model"] != "p/m" {
		t.Errorf("session id/model = %v/%v", ev[0]["id"], ev[0]["model"])
	}
	if ev[1]["name"] != "read" || ev[1]["input"] != `{"path":"/x"}` {
		t.Errorf("tool_use name/input = %v/%v", ev[1]["name"], ev[1]["input"])
	}
	if ev[2]["step"] != float64(1) || ev[2]["text"] != "let me think" {
		t.Errorf("reasoning_delta = %+v", ev[2])
	}
	// the tool_result name is resolved from c1 to read; output goes through EmitToolResult (short string not truncated).
	if ev[3]["name"] != "read" || ev[3]["call_id"] != "c1" || ev[3]["output"] != "file content" {
		t.Errorf("tool_result = %+v", ev[3])
	}
	if ev[4]["step"] != float64(2) || ev[4]["text"] != "final answer" {
		t.Errorf("text_delta = %+v", ev[4])
	}
	if ev[5]["text"] != "final answer" || ev[5]["steps"] != float64(2) || ev[5]["finish"] != "stop" ||
		ev[5]["model"] != "p/m" || ev[5]["input_tokens"] != float64(30) || ev[5]["output_tokens"] != float64(13) {
		t.Errorf("result = %+v", ev[5])
	}
}

// orphan tool message (tool_call_id has no match in assistant.ToolCalls) → name falls back to empty string, no crash; steps=0.
func TestReplaySession_OrphanTool(t *testing.T) {
	msgs := []miniagent.Message{
		{Role: miniagent.RoleTool, ToolCallID: "ghost", Content: "fragment"},
	}
	var buf bytes.Buffer
	if err := replaySession(&buf, session.SessionMeta{Type: "session", ID: "s", Model: "m"}, msgs); err != nil {
		t.Fatalf("replaySession: %v", err)
	}
	ev := parseNDJSON(t, buf.String())
	if !equalSlice(eventTypes(ev), []string{"session", "tool_result", "result"}) {
		t.Fatalf("event types = %v", eventTypes(ev))
	}
	if ev[1]["name"] != "" || ev[1]["call_id"] != "ghost" {
		t.Errorf("orphan tool_result name should be empty: %+v", ev[1])
	}
}

// e2e: -save-session creates a session containing a read tool call → -replay replays it. Verifies replay
// translates the persisted messages into an NDJSON event stream: session/tool_use/tool_result/text_delta/result,
// with key fields aligned.
// Known differences: the save pass is non-streaming and does not emit text_delta, while replay always emits
// it (as a whole string); result.model comes from the session metadata "p/m", differing from the runtime result
// (which uses ModelID "m") — both are accepted precision boundaries.
func TestCLI_Replay(t *testing.T) {
	target := filepath.Join(t.TempDir(), "t.txt")
	if err := os.WriteFile(target, []byte("hello-target"), 0o600); err != nil {
		t.Fatal(err)
	}
	// read args goes through a second Marshal into a valid JSON string embedded in the wire's arguments field.
	argsVal, _ := json.Marshal(map[string]string{"path": target})
	argsStr, _ := json.Marshal(string(argsVal))

	var mu sync.Mutex
	count := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		count++
		n := count
		mu.Unlock()
		if n == 1 {
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"read","arguments":`+string(argsStr)+`}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
			return
		}
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":2}}`)
	}))
	defer srv.Close()

	sessionDir := t.TempDir()
	cfgPath := writeSessionConfig(t, srv.URL, sessionDir)
	code, out := runMainBin(t, "read that file", []string{"-config", cfgPath, "-workdir", t.TempDir(), "-save-session"}, "MINIAGENT_API_KEY=sk-test")
	if code != 0 {
		t.Fatalf("save-session code=%d out=%s", code, out)
	}
	matches, _ := filepath.Glob(filepath.Join(sessionDir, "*.jsonl"))
	if len(matches) != 1 {
		t.Fatalf("session file count=%d out=%s", len(matches), out)
	}
	id := strings.TrimSuffix(filepath.Base(matches[0]), ".jsonl")

	// replay: empty stdin (replay does not read stdin, does not call the LLM).
	code, out2 := runMainBin(t, "", []string{"-config", cfgPath, "-replay", id}, "MINIAGENT_API_KEY=sk-test")
	if code != 0 {
		t.Fatalf("replay code=%d out=%s", code, out2)
	}
	ev := parseNDJSON(t, out2)

	wantTypes := []string{"session", "tool_use", "tool_result", "text_delta", "result"}
	if !equalSlice(eventTypes(ev), wantTypes) {
		t.Fatalf("replay event types = %v, want %v (out=%s)", eventTypes(ev), wantTypes, out2)
	}
	if ev[0]["id"] != id {
		t.Errorf("session id = %v, want %q", ev[0]["id"], id)
	}
	if ev[1]["name"] != "read" {
		t.Errorf("tool_use name = %v, want read", ev[1]["name"])
	}
	if ev[2]["call_id"] != "c1" || !strings.Contains(fmt.Sprint(ev[2]["output"]), "hello-target") {
		t.Errorf("tool_result = %+v", ev[2])
	}
	if ev[3]["text"] != "done" {
		t.Errorf("text_delta = %+v", ev[3])
	}
	if ev[4]["text"] != "done" || ev[4]["steps"] != float64(2) || ev[4]["finish"] != "stop" || ev[4]["model"] != "p/m" {
		t.Errorf("result = %+v", ev[4])
	}
}

// -replay and -session are mutually exclusive → error and exit 1.
func TestCLI_ReplayMutex(t *testing.T) {
	sessionDir := t.TempDir()
	cfgPath := writeSessionConfig(t, "http://127.0.0.1:1", sessionDir)
	code, out := runMainBin(t, "", []string{"-config", cfgPath, "-replay", "x", "-session", "y"}, "MINIAGENT_API_KEY=sk-test")
	if code != 1 {
		t.Errorf("code=%d want 1", code)
	}
	if !strings.Contains(out, "-replay") || !strings.Contains(out, "mutually exclusive") {
		t.Errorf("missing mutex error: %s", out)
	}
}

// -replay pointing to a non-existent session → error and exit 1 (prevents silent success on typo).
func TestCLI_ReplayMissingExits1(t *testing.T) {
	sessionDir := t.TempDir()
	cfgPath := writeSessionConfig(t, "http://127.0.0.1:1", sessionDir)
	code, out := runMainBin(t, "", []string{"-config", cfgPath, "-replay", "nope"}, "MINIAGENT_API_KEY=sk-test")
	if code != 1 {
		t.Errorf("code=%d want 1", code)
	}
	if !strings.Contains(out, "not found") {
		t.Errorf("missing not-exist error: %s", out)
	}
}

// eventTypes extracts the type field of each event (for assertions).
func eventTypes(events []map[string]any) []string {
	types := make([]string, 0, len(events))
	for _, e := range events {
		if t, ok := e["type"].(string); ok {
			types = append(types, t)
		}
	}
	return types
}
