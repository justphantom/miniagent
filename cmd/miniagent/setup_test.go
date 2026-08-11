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
	"github.com/justphantom/miniagent/internal/provider/anthropic"
	"github.com/justphantom/miniagent/internal/provider/openai"
)

func TestHTTPTimeoutFromConfig_RejectsNegative(t *testing.T) {
	neg := "-1s"
	cfg := &config.Config{Run: config.RunConfig{HTTPTimeout: &neg}}
	_, err := httpTimeoutFromConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "negative value is invalid") {
		t.Fatalf("expected negative duration error, got %v", err)
	}
}

// P4: buildLLM (default openai kind) returns a Provider composing a ChatClient (120s overall Timeout,
// non-streaming Do fallback preventing hangs #3) + a StreamClient (no Timeout, body not cut P2-5); both
// share the same *http.Transport (proxy/dial/TLS timeout, #2).
func TestBuildLLM_ChatTimeoutStreamNoTimeoutSharedTransport(t *testing.T) {
	llm := buildLLM("sk", config.ProviderConfig{ChatURL: "http://localhost:1234/v1/chat/completions"}, nil, 0)
	p, ok := llm.(*openai.Provider)
	if !ok {
		t.Fatalf("buildLLM default kind returned %T, want *openai.Provider", llm)
	}
	chat, stream := p.Chat, p.Stream
	if chat.HTTP == nil {
		t.Fatal("ChatClient.HTTP is nil, want injected client")
	}
	if chat.HTTP.Timeout != 120*time.Second {
		t.Errorf("ChatClient Timeout = %v, want 120s (non-streaming Do overall timeout fallback, #3)", chat.HTTP.Timeout)
	}
	if stream.HTTP == nil || stream.HTTP.Timeout != 0 {
		t.Errorf("StreamClient Timeout = %v, want 0 (streaming body not cut, P2-5)", stream.HTTP.Timeout)
	}
	ctr, ok := chat.HTTP.Transport.(*http.Transport)
	str, ok2 := stream.HTTP.Transport.(*http.Transport)
	if !ok || !ok2 {
		t.Fatalf("Transport = %T / %T, want *http.Transport", chat.HTTP.Transport, stream.HTTP.Transport)
	}
	if ctr != str {
		t.Error("ChatClient and StreamClient should share the same *http.Transport")
	}
	if ctr.Proxy == nil {
		t.Error("Transport.Proxy = nil, want http.ProxyFromEnvironment (#2)")
	}
	if ctr.DialContext == nil {
		t.Error("Transport.DialContext not set (dial timeout)")
	}
	if ctr.ResponseHeaderTimeout == 0 {
		t.Error("Transport.ResponseHeaderTimeout = 0, want >0")
	}
}

// buildAnthropicLLM must wire StreamAllowUnterminated from config just like the openai path (buildOpenAILLM),
// otherwise the opt-in is silently a no-op for kind=anthropic despite StreamClient honoring the field.
func TestBuildLLM_AnthropicStreamAllowUnterminated(t *testing.T) {
	t1 := true
	llm := buildLLM("sk", config.ProviderConfig{Kind: "anthropic", ChatURL: "http://localhost:1234/v1/messages", StreamAllowUnterminated: &t1}, nil, 0)
	p, ok := llm.(*anthropic.Provider)
	if !ok {
		t.Fatalf("buildLLM anthropic kind returned %T, want *anthropic.Provider", llm)
	}
	if !p.Stream.StreamAllowUnterminated {
		t.Error("anthropic StreamClient.StreamAllowUnterminated = false, want true (must be wired from config)")
	}
	// Default (nil) stays false.
	llm0 := buildLLM("sk", config.ProviderConfig{Kind: "anthropic", ChatURL: "http://localhost:1234/v1/messages"}, nil, 0)
	if p0, ok := llm0.(*anthropic.Provider); !ok || p0.Stream.StreamAllowUnterminated {
		t.Error("anthropic StreamAllowUnterminated should default to false when config is nil")
	}
}

// http (non-loopback) endpoints should warn about plaintext key transmission.
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

// requireConfig: when there is no -config and ~/.miniagent/miniagent.json does not exist, it should return an error.
// Explicit -config pointing to a non-existent path is a hard error (covered by TestCLI_ExplicitConfigMissingExits1).
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
	// Use the test directory in place of HOME to avoid loading the real ~/.miniagent/miniagent.json
	t.Setenv("HOME", dir)
	_, err = requireConfig("")
	if err == nil {
		t.Fatal("requireConfig should error when no config found")
	}
	if !strings.Contains(err.Error(), "config not found") {
		t.Errorf("error should mention config missing: %v", err)
	}
}

// requireConfig: when the default config's Stat error is not fs.ErrNotExist (e.g. a self-referential symlink triggering
// ELOOP, or permission denied) it is returned as a hard error (review P2-6).
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
	// Use the test directory in place of HOME to avoid loading the real ~/.miniagent/miniagent.json
	t.Setenv("HOME", dir)
	maDir := filepath.Join(dir, ".miniagent")
	if err := os.MkdirAll(maDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(maDir, "miniagent.json")
	// Self-referential symlink: Stat follows it → ELOOP (not fs.ErrNotExist) → hard error.
	if err := os.Symlink(cfgPath, cfgPath); err != nil {
		t.Skipf("cannot create self-referential symlink: %v", err)
	}
	_, err = requireConfig("")
	if err == nil {
		t.Fatal("expected hard error for non-ErrNotExist Stat failure, got nil")
	}
	// The error must propagate the original stat failure, not be swallowed into "config not found".
	if !strings.Contains(err.Error(), "stat config") && !strings.Contains(err.Error(), "too many levels of symbolic links") {
		t.Errorf("expected hard stat error, got: %v", err)
	}
}
