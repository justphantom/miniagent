package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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

// fork-based 测试：通过 MINIAGENT_TEST_ENTRYPOINTS=1 让 test binary 重新进入
// main()，覆盖 os.Exit 路径（这些路径无法在进程内测试）。

const entrypointEnv = "MINIAGENT_TEST_ENTRYPOINTS=1"

func TestMain(m *testing.M) {
	if os.Getenv("MINIAGENT_TEST_ENTRYPOINTS") == "1" {
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// writeConfigFixture 写一份指向 srvURL 的临时 miniagent.json（mode=auto 免 workdir），
// 返回其路径。runJSON 非空时原样作为 "run" 段（支持 max_tokens_total/max_duration 等仅 config 参数）。
func writeConfigFixture(t *testing.T, srvURL, runJSON string) string {
	t.Helper()
	cfgPath := filepath.Join(t.TempDir(), "miniagent.json")
	runField := ""
	if runJSON != "" {
		runField = `,"run":` + runJSON
	}
	body := `{"providers":[{"name":"p","chat_url":"` + srvURL + `/v1/chat/completions","models_url":"` + srvURL + `/v1/models"}],"defaults":{"model":"p/m","mode":"auto"}` + runField + `}`
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath
}

// configArgs 构造 e2e 的公共参数：写临时 config（替代旧裸模式 chatArgs），返回 -config <path> + extra。
func configArgs(t *testing.T, srvURL string, extra ...string) []string {
	t.Helper()
	return append([]string{"-config", writeConfigFixture(t, srvURL, "")}, extra...)
}

// runMainBin fork 出 test binary 自身，注入 env + stdin + args，返回
// (exitCode, combinedOutput)。extraEnv 追加在默认 env 之后（覆盖同名 key）。
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

// 显式 -config 不存在 → 报错（S1 后 config 必须存在，不再退裸模式）。
func TestCLI_ExplicitConfigMissingExits1(t *testing.T) {
	code, out := runMainBin(t, "prompt", []string{"-config", filepath.Join(t.TempDir(), "nope.json")})
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(out, "config") {
		t.Errorf("missing config error: %s", out)
	}
}

// config 无 defaults.model 且未传 -model → Resolve 报错。
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
	if !strings.Contains(out, "model") {
		t.Errorf("missing model error: %s", out)
	}
}

func TestCLI_MissingAPIKeyExits1(t *testing.T) {
	code, out := runMainBin(t, "prompt", configArgs(t, "http://127.0.0.1:1"))
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(out, "API key") {
		t.Errorf("missing API key error: %s", out)
	}
}

// default 模式无 workdir → 报错（需 -workdir 或 -mode auto）。
func TestCLI_DefaultModeRequiresWorkdir(t *testing.T) {
	args := configArgs(t, "http://127.0.0.1:1", "-mode", "default")
	code, out := runMainBin(t, "prompt", args, "MINIAGENT_API_KEY=sk-test")
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(out, "workdir") {
		t.Errorf("missing workdir-required error: %s", out)
	}
}

// -stream 与 -result-only 互斥。
func TestCLI_StreamResultOnlyMutex(t *testing.T) {
	args := configArgs(t, "http://127.0.0.1:1", "-stream", "-result-only")
	code, out := runMainBin(t, "prompt", args, "MINIAGENT_API_KEY=sk-test")
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(out, "互斥") {
		t.Errorf("missing mutex error: %s", out)
	}
}

func TestCLI_EmptyStdinExits1(t *testing.T) {
	code, out := runMainBin(t, "", configArgs(t, "http://127.0.0.1:1"), "MINIAGENT_API_KEY=sk-test")
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(out, "stdin is empty") {
		t.Errorf("missing error: %s", out)
	}
}

func TestCLI_OversizedStdinExits1(t *testing.T) {
	code, out := runMainBin(t, strings.Repeat("x", maxPromptBytes+1), configArgs(t, "http://127.0.0.1:1"), "MINIAGENT_API_KEY=sk-test")
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(out, "超过大小上限") {
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

// e2e：仅靠 -key-file 提供 key，key 进入请求 Authorization。
func TestCLI_KeyFileAuth(t *testing.T) {
	var mu sync.Mutex
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotAuth = r.Header.Get("Authorization")
		mu.Unlock()
		_, _ = fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer srv.Close()

	keyFile := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(keyFile, []byte("sk-from-config-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out := runMainBin(t, "ping", configArgs(t, srv.URL, "-key-file", keyFile))
	if code != 0 {
		t.Fatalf("code = %d, out = %s", code, out)
	}
	mu.Lock()
	defer mu.Unlock()
	if gotAuth != "Bearer sk-from-config-file" {
		t.Errorf("Authorization = %q", gotAuth)
	}
}
