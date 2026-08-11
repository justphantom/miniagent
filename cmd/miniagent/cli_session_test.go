package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/justphantom/miniagent/internal/miniagent/session"
)

// writeSessionConfig writes a temporary config pointing at srvURL, with session.dir=sessionDir and mode=auto (workdir is passed explicitly per caller; required in all modes).
func writeSessionConfig(t *testing.T, srvURL, sessionDir string) string {
	t.Helper()
	cfgPath := filepath.Join(t.TempDir(), "miniagent.json")
	body := `{"session":{"dir":"` + sessionDir + `"},"providers":[{"name":"p","chat_url":"` + srvURL + `/v1/chat/completions","models_url":"` + srvURL + `/v1/models"}],"defaults":{"provider":"p","model":"m","mode":"auto"},"compaction":{"provider":"p","model":"m"}}`
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath
}

// Two-turn resume e2e: turn1 -save-session creates (id generated internally), turn2 -session <id> resumes.
// Verifies the second turn's request body contains the first turn's answer, and the session jsonl persists both turns.
func TestCLI_SessionTwoTurns(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		_, _ = fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"answerX"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer srv.Close()

	sessionDir := t.TempDir()
	cfgPath := writeSessionConfig(t, srv.URL, sessionDir)
	code, out := runMainBin(t, "turn1-question", []string{"-config", cfgPath, "-workdir", t.TempDir(), "-save-session"}, "MINIAGENT_API_KEY=sk-test")
	if code != 0 {
		t.Fatalf("turn1 code = %d, out = %s", code, out)
	}
	matches, err := filepath.Glob(filepath.Join(sessionDir, "*.jsonl"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("expected 1 session file, got %v (err=%v) out=%s", matches, err, out)
	}
	sess := matches[0]
	id := strings.TrimSuffix(filepath.Base(sess), ".jsonl")

	code, out = runMainBin(t, "turn2-question", []string{"-config", cfgPath, "-workdir", t.TempDir(), "-session", id}, "MINIAGENT_API_KEY=sk-test")
	if code != 0 {
		t.Fatalf("turn2 code = %d, out = %s", code, out)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("server got %d requests, want 2", len(bodies))
	}
	if !strings.Contains(bodies[1], "turn1-question") || !strings.Contains(bodies[1], "answerX") || !strings.Contains(bodies[1], "turn2-question") {
		t.Errorf("turn2 request missing turn1 context: %s", bodies[1])
	}
	data, err := os.ReadFile(sess)
	if err != nil {
		t.Fatalf("session file not written: %v", err)
	}
	for _, want := range []string{"turn1-question", "answerX", "turn2-question"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("session file missing %q: %s", want, data)
		}
	}
}

// Corrupt session → resume errors out with exit 1.
func TestCLI_CorruptSessionExits1(t *testing.T) {
	sessionDir := t.TempDir()
	sess := filepath.Join(sessionDir, "bad.jsonl")
	// Corrupted in the middle (a legal line sandwiching an illegal one): LoadSession errors out strictly (only a half-written tail line is tolerated), CLI exit code 1.
	body := "{\"type\":\"session\",\"id\":\"bad\"}\n{oops}\n{\"type\":\"message\",\"role\":\"user\",\"content\":\"q\"}\n"
	if err := os.WriteFile(sess, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgPath := writeSessionConfig(t, "http://127.0.0.1:1", sessionDir)
	code, out := runMainBin(t, "prompt", []string{"-config", cfgPath, "-workdir", t.TempDir(), "-session", "bad"}, "MINIAGENT_API_KEY=sk-test")
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(out, "load session") {
		t.Errorf("missing error: %s", out)
	}
}

// -session pointing to a non-existent session → errors out with exit 1 (prevents creating a garbage session on a typo; use -save-session to create a new one).
func TestCLI_ResumeMissingSessionExits1(t *testing.T) {
	sessionDir := t.TempDir()
	cfgPath := writeSessionConfig(t, "http://127.0.0.1:1", sessionDir)
	code, out := runMainBin(t, "prompt", []string{"-config", cfgPath, "-workdir", t.TempDir(), "-session", "nope"}, "MINIAGENT_API_KEY=sk-test")
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(out, "not found") {
		t.Errorf("missing not-exist error: %s", out)
	}
}

// P3 SIGINT exit code: SIGINT takes code 130 (128+SIGINT POSIX), does not emit an error event (clean exit).
// Non-streaming Do returns context.Canceled after ctx cancel, and main os.Exit(130) accordingly.
func TestCLI_SIGINTExits130(t *testing.T) {
	var hitOnce sync.Once
	serverHit := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitOnce.Do(func() { close(serverHit) }) // the subprocess has entered Run and sent the HTTP request
		time.Sleep(5 * time.Second)             // slow response so SIGINT arrives before the response
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], configArgs(t, srv.URL, "-workdir", t.TempDir())...)
	cmd.Stdin = strings.NewReader("prompt")
	env := []string{entrypointEnv}
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "MINIAGENT_API_KEY=") || strings.HasPrefix(kv, "MINIAGENT_TEST_ENTRYPOINTS=") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "MINIAGENT_API_KEY=sk-test")
	cmd.Env = env
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Wait until srv receives the request (the subprocess has entered Run's HTTP block) before sending SIGINT: a fixed sleep is unreliable under -race
	// load — the subprocess may not have registered its signal handler yet, and SIGINT gets killed by the default disposition (exit -1).
	select {
	case <-serverHit:
	case <-ctx.Done():
		t.Fatalf("subprocess did not hit srv within the timeout: %s", out.String())
	}
	_ = cmd.Process.Signal(syscall.SIGINT)
	err := cmd.Wait()
	var ee *exec.ExitError
	code := 0
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("cmd.Wait err=%v out=%s", err, out.String())
	}
	if code != 130 {
		t.Errorf("SIGINT should exit 130, got %d (out=%s)", code, out.String())
	}
	// SIGINT should not emit an error NDJSON event (clean exit, distinct from a real failure).
	if strings.Contains(out.String(), `"type":"error"`) {
		t.Errorf("SIGINT should not emit an error event: %s", out.String())
	}
}

// -save-session and -result-only passed together → stderr errors out with exit 1 (subagent fork is stateless, does not persist a session).
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

// -save-session creating a new session: the first stdout event is session (type/provider/id),
// and the provider in the jsonl's first metadata line matches it (the output contract is isomorphic with the persisted file).
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

// When Run errors (LLM 500) it still persists the executed part of this turn: recovers the user prompt + the executed assistant/tool sequence,
// and the pairing is complete and reloadable by LoadSession (resume is not interrupted). Verifies saveSession takes effect on the error path —
// the old behavior was for err to directly os.Exit(1) skipping persistence, leaving the orphan inconsistency of "tool already executed, jsonl not appended".
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

// Canceled (SIGINT→exit 130) also persists: recovers the user prompt for the next continuation.
// Run via defer still returns NewMessages on the Canceled path (including the user prompt appended before the loop), with complete pairing.
func TestCLI_CanceledRunSavesSession(t *testing.T) {
	var hitOnce sync.Once
	serverHit := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitOnce.Do(func() { close(serverHit) }) // the subprocess has entered Run and sent the HTTP request
		time.Sleep(5 * time.Second)             // slow response so SIGINT arrives before the response
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	sessionDir := t.TempDir()
	cfgPath := writeSessionConfig(t, srv.URL, sessionDir)
	cmd := exec.CommandContext(ctx, os.Args[0], "-config", cfgPath, "-workdir", t.TempDir(), "-save-session")
	cmd.Stdin = strings.NewReader("mid-cancel-question")
	env := []string{entrypointEnv}
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "MINIAGENT_API_KEY=") || strings.HasPrefix(kv, "MINIAGENT_TEST_ENTRYPOINTS=") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "MINIAGENT_API_KEY=sk-test")
	cmd.Env = env
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-serverHit:
	case <-ctx.Done():
		t.Fatalf("subprocess did not hit srv within the timeout: %s", out.String())
	}
	_ = cmd.Process.Signal(syscall.SIGINT)
	err := cmd.Wait()
	var ee *exec.ExitError
	code := 0
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("cmd.Wait err=%v out=%s", err, out.String())
	}
	if code != 130 {
		t.Errorf("SIGINT should exit 130, got %d (out=%s)", code, out.String())
	}
	matches, _ := filepath.Glob(filepath.Join(sessionDir, "*.jsonl"))
	if len(matches) != 1 {
		t.Fatalf("Canceled should persist 1 session file, got %d (out=%s)", len(matches), out.String())
	}
	data, _ := os.ReadFile(matches[0])
	if !strings.Contains(string(data), `"role":"user"`) || !strings.Contains(string(data), "mid-cancel-question") {
		t.Errorf("Canceled should recover the user prompt: %s", data)
	}
}

// When neither -save-session nor -session is active (sessPath=""), an error does not persist: saveSession's sessPath guard —
// the error path does not accidentally create a session file (persistence requires explicitly activating -save-session/-session).
// Note: the len(NewMessages)==0 guard is purely defensive — the user prompt is appended to newMsgs before the Run loop,
// and the defer guarantees it is brought back, so that branch is unreachable under the current architecture and is not unit-tested.
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
