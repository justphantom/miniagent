package main

import (
	"bytes"
	"context"
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
	"testing"
	"time"
)

// fork-based tests: via MINIAGENT_TEST_ENTRYPOINTS=1 the test binary re-enters
// main(), covering os.Exit paths (these paths cannot be tested in-process).

const entrypointEnv = "MINIAGENT_TEST_ENTRYPOINTS=1"

func TestMain(m *testing.M) {
	if os.Getenv("MINIAGENT_TEST_ENTRYPOINTS") == "1" {
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// writeConfigFixture writes a temporary miniagent.json pointing at srvURL (mode=auto; -workdir is supplied explicitly per e2e caller — workdir is required in all modes),
// and returns its path. When runJSON is non-empty it is used verbatim as the "run" section (supporting config-only params like max_tokens_total/max_duration).
func writeConfigFixture(t *testing.T, srvURL, runJSON string) string {
	t.Helper()
	cfgPath := filepath.Join(t.TempDir(), "miniagent.json")
	runField := ""
	if runJSON != "" {
		runField = `,"run":` + runJSON
	}
	body := `{"providers":[{"name":"p","chat_url":"` + srvURL + `/v1/chat/completions","models_url":"` + srvURL + `/v1/models"}],"defaults":{"provider":"p","model":"m","mode":"auto"},"compaction":{"provider":"p","model":"m"}` + runField + `}`
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath
}

// configArgs builds the common args for e2e: writes a temporary config (replacing the old bare-mode chatArgs), returns -config <path> + extra.
func configArgs(t *testing.T, srvURL string, extra ...string) []string {
	t.Helper()
	return append([]string{"-config", writeConfigFixture(t, srvURL, "")}, extra...)
}

// runMainBin forks the test binary itself, injecting env + stdin + args, returning
// (exitCode, combinedOutput). extraEnv is appended after the default env (overriding same-named keys).
func runMainBin(t *testing.T, stdin string, args []string, extraEnv ...string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], args...)
	cmd.Stdin = strings.NewReader(stdin)
	env := []string{entrypointEnv}
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "MINIAGENT_API_KEY=") ||
			strings.HasPrefix(kv, "MINIAGENT_TEST_ENTRYPOINTS=") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, extraEnv...)
	cmd.Env = env
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("exec test binary: %v", err)
	}
	return code, out.String()
}

func TestCLI_VersionExitsZero(t *testing.T) {
	code, out := runMainBin(t, "", []string{"-version"})
	if code != 0 {
		t.Errorf("code = %d, want 0; out=%s", code, out)
	}
	if !strings.Contains(out, "miniagent ") {
		t.Errorf("missing version banner: %s", out)
	}
}

// Explicit -config that does not exist → error (after S1 config must exist; bare mode is no longer a fallback).
func TestCLI_ExplicitConfigMissingExits1(t *testing.T) {
	code, out := runMainBin(t, "prompt", []string{"-config", filepath.Join(t.TempDir(), "nope.json")})
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(out, "config") {
		t.Errorf("missing config error: %s", out)
	}
}

// config missing defaults.provider/model (required after the split) → validateConfig errors out.
func TestCLI_MissingModelExits1(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "miniagent.json")
	body := `{"providers":[{"name":"p","chat_url":"http://127.0.0.1:1/v1/chat/completions"}]}`
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out := runMainBin(t, "prompt", []string{"-config", cfgPath}, "MINIAGENT_API_KEY=sk-test")
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(out, "is required") {
		t.Errorf("missing required-field error: %s", out)
	}
}

func TestCLI_MissingAPIKeyExits1(t *testing.T) {
	code, out := runMainBin(t, "prompt", configArgs(t, "http://127.0.0.1:1", "-workdir", t.TempDir()))
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(out, "API key") {
		t.Errorf("missing API key error: %s", out)
	}
}

// missing -workdir → error (workdir is unconditionally required and must be absolute).
func TestCLI_RequiresWorkdir(t *testing.T) {
	args := configArgs(t, "http://127.0.0.1:1")
	code, out := runMainBin(t, "prompt", args, "MINIAGENT_API_KEY=sk-test")
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(out, "workdir") {
		t.Errorf("missing workdir-required error: %s", out)
	}
}

// -stream and -result-only are mutually exclusive.
func TestCLI_StreamResultOnlyMutex(t *testing.T) {
	args := configArgs(t, "http://127.0.0.1:1", "-stream", "-result-only")
	code, out := runMainBin(t, "prompt", args, "MINIAGENT_API_KEY=sk-test")
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(out, "mutually exclusive") {
		t.Errorf("missing mutex error: %s", out)
	}
}

func TestCLI_EmptyStdinExits1(t *testing.T) {
	code, out := runMainBin(t, "", configArgs(t, "http://127.0.0.1:1", "-workdir", t.TempDir()), "MINIAGENT_API_KEY=sk-test")
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(out, "stdin is empty") {
		t.Errorf("missing error: %s", out)
	}
}

func TestCLI_OversizedStdinExits1(t *testing.T) {
	code, out := runMainBin(t, strings.Repeat("x", maxPromptBytes+1), configArgs(t, "http://127.0.0.1:1", "-workdir", t.TempDir()), "MINIAGENT_API_KEY=sk-test")
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(out, "exceeds the size limit") {
		t.Errorf("missing error: %s", out)
	}
}

func TestCLI_InvalidLogLevelExits1(t *testing.T) {
	code, out := runMainBin(t, "prompt", configArgs(t, "http://127.0.0.1:1", "-log-level", "bogus"), "MINIAGENT_API_KEY=sk-test")
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(out, "invalid -log-level") {
		t.Errorf("missing error: %s", out)
	}
}

// e2e: config 未设 context_window/max_tokens 且配了 models_url → 启动时 GET /v1/models 自动填充
// （模型 ID 命中 limits）；config 已显式设 context_window → 不发 models GET（显式配置优先，绝不覆盖）。
func TestCLI_AutoFillModelLimits(t *testing.T) {
	var modelsCalls int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			mu.Lock()
			modelsCalls++
			mu.Unlock()
			fmt.Fprint(w, `{"data":[{"id":"m","context_window":524288,"max_output_tokens":65536}]}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer srv.Close()

	// auto-fill path: no context_window/max_tokens in config → GET models, run succeeds.
	code, out := runMainBin(t, "hi", configArgs(t, srv.URL, "-workdir", t.TempDir()), "MINIAGENT_API_KEY=sk-test")
	if code != 0 {
		t.Fatalf("auto-fill code = %d, out = %s", code, out)
	}
	mu.Lock()
	got := modelsCalls
	mu.Unlock()
	if got != 1 {
		t.Errorf("models GET = %d, want 1 (auto-fill should fetch once when limits unset)", got)
	}
	if !strings.Contains(out, `"type":"result"`) {
		t.Errorf("missing result event: %s", out)
	}
}

// e2e: -save-session creates a new session, persisting the conversation to a jsonl under session.dir with an internally generated id.
func TestCLI_SingleTurnSessionAppend(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer srv.Close()

	sessionDir := t.TempDir()
	cfgPath := writeSessionConfig(t, srv.URL, sessionDir)
	code, out := runMainBin(t, "hi", []string{"-config", cfgPath, "-workdir", t.TempDir(), "-save-session"}, "MINIAGENT_API_KEY=sk-test")
	if code != 0 {
		t.Fatalf("code = %d, out = %s", code, out)
	}
	if !strings.Contains(out, `"type":"result"`) || !strings.Contains(out, `"text":"ok"`) {
		t.Errorf("missing result event: %s", out)
	}
	matches, err := filepath.Glob(filepath.Join(sessionDir, "*.jsonl"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("expected 1 session file, got %v (err=%v)", matches, err)
	}
	data, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatalf("read session: %v", err)
	}
	if !strings.Contains(string(data), `"role":"user"`) || !strings.Contains(string(data), `"role":"assistant"`) {
		t.Errorf("session missing messages: %s", data)
	}
}

// -save-session and -session are mutually exclusive.
func TestCLI_SaveSessionMutex(t *testing.T) {
	args := configArgs(t, "http://127.0.0.1:1", "-save-session", "-session", "x")
	code, out := runMainBin(t, "prompt", args, "MINIAGENT_API_KEY=sk-test")
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(out, "mutually exclusive") {
		t.Errorf("missing mutex error: %s", out)
	}
}

// e2e: the entire stdin content is treated as a single turn's complete prompt (multi-line is not split), one LLM call returns one result.
func TestCLI_MultiLineStdinSingleTurn(t *testing.T) {
	var calls int
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			// auto-fill models GET (writeConfigFixture sets models_url): report no limits, run proceeds unset.
			fmt.Fprint(w, `{"data":[{"id":"m"}]}`)
			return
		}
		calls++
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer srv.Close()

	code, out := runMainBin(t, "hi\nhello\n", configArgs(t, srv.URL, "-workdir", t.TempDir()), "MINIAGENT_API_KEY=sk-test")
	if code != 0 {
		t.Fatalf("code = %d, out = %s", code, out)
	}
	if calls != 1 {
		t.Errorf("server calls = %d, want 1 (multi-line stdin should be composed into a single turn)", calls)
	}
	if strings.Count(out, `"type":"result"`) != 1 {
		t.Errorf("want 1 result event, got: %s", out)
	}
	if !strings.Contains(gotBody, `hi\nhello`) {
		t.Errorf("prompt should preserve the multi-line original: %s", gotBody)
	}
}

// e2e: when run.max_duration expires the non-interactive path returns 1.
func TestCLI_MaxDurationExits1(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			fmt.Fprint(w, `{"data":[{"id":"m"}]}`)
			return
		}
		time.Sleep(50 * time.Millisecond)
		_, _ = fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"late"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer srv.Close()

	cfgPath := writeConfigFixture(t, srv.URL, `{"max_duration":"1ns"}`)
	code, out := runMainBin(t, "hi", []string{"-config", cfgPath, "-workdir", t.TempDir()}, "MINIAGENT_API_KEY=sk-test")
	if code != 1 {
		t.Errorf("code = %d, want 1; out=%s", code, out)
	}
}
