package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/justphantom/miniagent/miniagent/session"
)

func TestCLI_SaveSessionAndResultOnlyExits1(t *testing.T) {
	sessionDir := t.TempDir()
	cfgPath := writeSessionConfig(t, "http://127.0.0.1:1", sessionDir)
	code, out := runMainBin(t, "prompt", []string{"-config", cfgPath, "-save-session", "-result-only"}, "MINIAGENT_API_KEY=sk-test")
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(out, "-save-session is mutually exclusive with -result-only") {
		t.Errorf("missing mutex error: %s", out)
	}
}

func TestCLI_SaveSessionEmitsSessionEventFirst(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer srv.Close()
	sessionDir := t.TempDir()
	cfgPath := writeSessionConfig(t, srv.URL, sessionDir)
	code, out := runMainBin(t, "question", []string{"-config", cfgPath, "-workdir", t.TempDir(), "-save-session"}, "MINIAGENT_API_KEY=sk-test")
	if code != 0 {
		t.Fatalf("code = %d, out = %s", code, out)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) == 0 {
		t.Fatal("stdout has no output")
	}
	var first map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("first line is not JSON: %s err=%v", lines[0], err)
	}
	if first["type"] != "session" {
		t.Errorf("first line type = %v, want session (the session event must be emitted before Run)", first["type"])
	}
	if first["provider"] != "p" {
		t.Errorf("provider = %v, want p (the config provider name)", first["provider"])
	}
	if first["id"] == nil || first["id"] == "" {
		t.Errorf("id missing or empty: %v", first["id"])
	}
	// The session file should already be persisted; the first metadata line has the same provider as the stdout session event.
	matches, _ := filepath.Glob(filepath.Join(sessionDir, "*.jsonl"))
	if len(matches) != 1 {
		t.Fatalf("session file count = %d, want 1 (out=%s)", len(matches), out)
	}
	data, _ := os.ReadFile(matches[0])
	firstFileLine := strings.SplitN(strings.TrimSpace(string(data)), "\n", 2)[0]
	var meta map[string]any
	if err := json.Unmarshal([]byte(firstFileLine), &meta); err != nil {
		t.Fatalf("jsonl first line is not JSON: %s", firstFileLine)
	}
	if meta["provider"] != first["provider"] {
		t.Errorf("jsonl first line provider = %v, does not match the stdout session event %v", meta["provider"], first["provider"])
	}
	if meta["id"] != first["id"] {
		t.Errorf("jsonl first line id = %v, does not match the stdout session event %v", meta["id"], first["id"])
	}
}

func TestCLI_ErrorRunSavesPartialSession(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			// 1st call: returns a tool_call (pointing to a non-existent tool) → handleToolCalls backfills an "unknown tool" result and appends it to history.
			_, _ = fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"search","tool_calls":[{"id":"c1","type":"function","function":{"name":"no_such_tool","arguments":"{}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
			return
		}
		// From the 2nd call on, 500: not context-length, OnLLMError does not retry, surfaces directly → Run returns err → main exit 1 (but already persisted).
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = fmt.Fprint(w, `{"error":{"message":"upstream boom"}}`)
	}))
	defer srv.Close()

	sessionDir := t.TempDir()
	cfgPath := writeSessionConfig(t, srv.URL, sessionDir)
	code, out := runMainBin(t, "help-question", []string{"-config", cfgPath, "-workdir", t.TempDir(), "-save-session"}, "MINIAGENT_API_KEY=sk-test")
	if code != 1 {
		t.Fatalf("code = %d, want 1 (LLM 500 should exit 1); out=%s", code, out)
	}
	matches, err := filepath.Glob(filepath.Join(sessionDir, "*.jsonl"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("expected 1 session file, got %v (err=%v) out=%s", matches, err, out)
	}
	data, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatalf("read session: %v", err)
	}
	s := string(data)
	for _, want := range []string{`"role":"user"`, `"role":"assistant"`, `"role":"tool"`, "no_such_tool", "help-question"} {
		if !strings.Contains(s, want) {
			t.Errorf("session missing %q (the error turn should recover the executed part): %s", want, s)
		}
	}
	// The persisted session must have complete pairing and be reloadable by LoadSession — otherwise the next resume errors out, equivalent to no recovery.
	if _, _, err := session.LoadSession(matches[0]); err != nil {
		t.Errorf("the session persisted on the error path cannot be reloaded by LoadSession (broken pairing?): %v\n%s", err, s)
	}
}

func TestCLI_ErrorRunNoSessionSkipsSave(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = fmt.Fprint(w, `{"error":{"message":"boom"}}`)
	}))
	defer srv.Close()
	sessionDir := t.TempDir()
	cfgPath := writeSessionConfig(t, srv.URL, sessionDir)
	code, _ := runMainBin(t, "question", []string{"-config", cfgPath, "-workdir", t.TempDir()}, "MINIAGENT_API_KEY=sk-test")
	if code != 1 {
		t.Errorf("code = %d, want 1 (500 error)", code)
	}
	matches, _ := filepath.Glob(filepath.Join(sessionDir, "*.jsonl"))
	if len(matches) != 0 {
		t.Errorf("should not persist when neither -save-session/-session is active, got %v", matches)
	}
}
