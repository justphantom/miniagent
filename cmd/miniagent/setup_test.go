package main

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/justphantom/miniagent/internal/miniagent/config"
)

func TestHTTPTimeoutFromConfig_RejectsNegative(t *testing.T) {
	neg := "-1s"
	cfg := &config.Config{Run: config.RunConfig{HTTPTimeout: &neg}}
	_, err := httpTimeoutFromConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "负值") {
		t.Fatalf("expected negative duration error, got %v", err)
	}
}

// P4：buildLLM 返回 ChatClient（带 120s 总 Timeout，非流式 Do 兜底防挂死 #3）+ StreamClient
// （无 Timeout，body 不被砍 P2-5），两者共享同一 *http.Transport（代理/dial/TLS 超时，#2）。
func TestBuildLLM_ChatTimeoutStreamNoTimeoutSharedTransport(t *testing.T) {
	chat, stream := buildLLM("sk", config.ProviderConfig{ChatURL: "http://localhost:1234/v1/chat/completions"}, nil, 0)
	if chat.HTTP == nil {
		t.Fatal("ChatClient.HTTP is nil, want injected client")
	}
	if chat.HTTP.Timeout != 120*time.Second {
		t.Errorf("ChatClient Timeout = %v, want 120s（非流式 Do 总超时兜底，#3）", chat.HTTP.Timeout)
	}
	if stream.HTTP == nil || stream.HTTP.Timeout != 0 {
		t.Errorf("StreamClient Timeout = %v, want 0（流式 body 不被砍，P2-5）", stream.HTTP.Timeout)
	}
	ctr, ok := chat.HTTP.Transport.(*http.Transport)
	str, ok2 := stream.HTTP.Transport.(*http.Transport)
	if !ok || !ok2 {
		t.Fatalf("Transport = %T / %T, want *http.Transport", chat.HTTP.Transport, stream.HTTP.Transport)
	}
	if ctr != str {
		t.Error("ChatClient 与 StreamClient 应共享同一 *http.Transport")
	}
	if ctr.Proxy == nil {
		t.Error("Transport.Proxy = nil, want http.ProxyFromEnvironment（#2）")
	}
	if ctr.DialContext == nil {
		t.Error("Transport.DialContext not set（拨号超时）")
	}
	if ctr.ResponseHeaderTimeout == 0 {
		t.Error("Transport.ResponseHeaderTimeout = 0, want >0")
	}
}

// http（非 loopback）端点应警告明文传 key。
func TestWarnInsecureURL(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	warnInsecureURL("http://llm.internal:8000/v1/chat/completions")
	warnInsecureURL("http://localhost:8080/v1/chat/completions")
	warnInsecureURL("http://127.0.0.1:11434/v1/chat/completions")
	warnInsecureURL("https://api.openai.com/v1/chat/completions")
	_ = w.Close()
	os.Stderr = old
	out, _ := io.ReadAll(r)
	if got := strings.Count(string(out), "warning"); got != 1 {
		t.Errorf("warnings = %d, want 1: %s", got, out)
	}
}

// requireConfig：无 -config 且 ~/.miniagent/miniagent.json 不存在时，应返回 error。
// 显式 -config 不存在=硬错误（通过 TestCLI_ExplicitConfigMissingExits1 覆盖）。
func TestRequireConfig_NoConfigExits(t *testing.T) {
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldWD)
	dir := t.TempDir()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	// 用测试目录替代 HOME，避免加载真实的 ~/.miniagent/miniagent.json
	t.Setenv("HOME", dir)
	_, err = requireConfig("")
	if err == nil {
		t.Fatal("requireConfig should error when no config found")
	}
	if !strings.Contains(err.Error(), "config 不存在") {
		t.Errorf("error should mention config missing: %v", err)
	}
}

// requireConfig：默认 config 的 Stat 错误若非 fs.ErrNotExist（如自指符号链接触发
// ELOOP、permission denied）按硬错误返回（审查 P2-6）。
func TestRequireConfig_DefaultStatErrorIsHardError(t *testing.T) {
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldWD)
	dir := t.TempDir()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	// 用测试目录替代 HOME，避免加载真实的 ~/.miniagent/miniagent.json
	t.Setenv("HOME", dir)
	maDir := filepath.Join(dir, ".miniagent")
	if err := os.MkdirAll(maDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(maDir, "miniagent.json")
	// 自指符号链接：Stat 跟随 → ELOOP（非 fs.ErrNotExist）→ 硬错误。
	if err := os.Symlink(cfgPath, cfgPath); err != nil {
		t.Skipf("cannot create self-referential symlink: %v", err)
	}
	_, err = requireConfig("")
	if err == nil {
		t.Fatal("expected hard error for non-ErrNotExist Stat failure, got nil")
	}
	// 错误必须透传原始 stat 失败，而不是被吞成“config 不存在”。
	if !strings.Contains(err.Error(), "stat config") && !strings.Contains(err.Error(), "too many levels of symbolic links") {
		t.Errorf("expected hard stat error, got: %v", err)
	}
}
