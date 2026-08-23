package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func writeSessionConfig(t *testing.T, srvURL, sessionDir string) string {
	t.Helper()
	cfgPath := filepath.Join(t.TempDir(), "miniagent.json")
	body := `{"session":{"dir":"` + sessionDir + `"},"providers":[{"name":"p","chat_url":"` + srvURL + `/v1/chat/completions"}],"defaults":{"provider":"p","model":"m","mode":"auto"},"compaction":{"provider":"p","model":"m"}}`
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath
}

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
