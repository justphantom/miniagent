package miniagent

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestShell_RunsCommand(t *testing.T) {
	s := ShellTool(t.TempDir(), false, nil)
	res := s.Call(context.Background(), `{"command":"echo hello"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if !strings.Contains(res.Output, "hello") {
		t.Errorf("Output = %q", res.Output)
	}
}

func TestShell_CwdIsWorkspaceRoot(t *testing.T) {
	dir := t.TempDir()
	s := ShellTool(dir, false, nil)
	res := s.Call(context.Background(), `{"command":"pwd"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	cleaned := filepath.Clean(dir)
	if !strings.Contains(res.Output, cleaned) {
		t.Errorf("Output = %q, want contains %q", res.Output, cleaned)
	}
}

func TestShell_BlockedPattern(t *testing.T) {
	s := ShellTool(t.TempDir(), false, nil)
	res := s.Call(context.Background(), `{"command":"rm -rf /"}`)
	if !res.IsError {
		t.Fatal("expected error")
	}
	if !strings.Contains(res.Output, "黑名单") {
		t.Errorf("error = %q", res.Output)
	}
}

func TestShell_BlockedPatternCaseInsensitive(t *testing.T) {
	s := ShellTool(t.TempDir(), false, nil)
	res := s.Call(context.Background(), `{"command":"RM -RF /tmp/x"}`)
	if !res.IsError {
		t.Fatal("expected error")
	}
}

func TestShell_StripsSecretEnv(t *testing.T) {
	t.Setenv("MINIAGENT_API_KEY", "sk-LEAK")
	t.Setenv("DATABASE_URL", "postgres://LEAK")
	t.Setenv("GOPATH", "/test/gopath") // GOPATH 在白名单中
	s := ShellTool(t.TempDir(), false, nil)
	res := s.Call(context.Background(), `{"command":"env"}`)
	if res.IsError {
		t.Fatalf("shell env failed: %s", res.Output)
	}
	if strings.Contains(res.Output, "sk-LEAK") || strings.Contains(res.Output, "postgres://LEAK") {
		t.Errorf("secret leaked: %q", res.Output)
	}
	if !strings.Contains(res.Output, "GOPATH=/test/gopath") {
		t.Errorf("whitelist env missing: %q", res.Output)
	}
}

func TestShell_NonZeroExitIsError(t *testing.T) {
	s := ShellTool(t.TempDir(), false, nil)
	res := s.Call(context.Background(), `{"command":"echo out; exit 3"}`)
	if !res.IsError {
		t.Fatal("expected error")
	}
	if !strings.Contains(res.Output, "退出码") {
		t.Errorf("Output = %q", res.Output)
	}
}

func TestShell_EmptyWorkspaceRoot(t *testing.T) {
	s := ShellTool("", false, nil)
	res := s.Call(context.Background(), `{"command":"echo x"}`)
	if !res.IsError {
		t.Fatal("expected error")
	}
}

func TestShell_BlockedPatternSpacedFlags(t *testing.T) {
	s := ShellTool(t.TempDir(), false, nil)
	res := s.Call(context.Background(), `{"command":"rm -r -f /tmp/x"}`)
	if !res.IsError {
		t.Fatal("expected error")
	}
}

func TestShell_BypassBase64PipeShRejected(t *testing.T) {
	s := ShellTool(t.TempDir(), false, nil)
	res := s.Call(context.Background(), `{"command":"echo cm0gLXJmIC8= | base64 -d | sh"}`)
	if !res.IsError {
		t.Fatal("expected base64 pipe-sh to be rejected")
	}
}

func TestShell_MissingWorkspaceRootRejected(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "missing")
	s := ShellTool(dir, false, nil)
	res := s.Call(context.Background(), `{"command":"echo x"}`)
	if !res.IsError {
		t.Fatal("expected missing workspace root to be rejected")
	}
}

func TestWebFetch_ParsesHTML(t *testing.T) {
	body := []byte(`<html><head><title>T</title><style>x{}</style></head><body><p>hello <b>world</b></p><script>alert(1)</script></body></html>`)
	got := htmlToText(body)
	if !strings.Contains(got, "hello world") {
		t.Errorf("Output = %q", got)
	}
	if strings.Contains(got, "alert") || strings.Contains(got, "x{}") {
		t.Errorf("script/style not stripped: %q", got)
	}
}

func TestWebFetch_Non2xxIsError(t *testing.T) {
	// 本地地址现在被 SSRF 拦截；验证拒绝行为即可。
	res := WebFetchTool(nil).Call(context.Background(), `{"url":"http://127.0.0.1:8080/x"}`)
	if !res.IsError {
		t.Fatal("expected error")
	}
}

func TestWebFetch_PlainTextReturned(t *testing.T) {
	got := htmlToText([]byte("plain body"))
	if got != "plain body" {
		t.Errorf("Output = %q", got)
	}
}

func TestWebFetch_ConnectionError(t *testing.T) {
	res := WebFetchTool(nil).Call(context.Background(), `{"url":"http://10.0.0.1:12345/x"}`)
	if !res.IsError {
		t.Fatal("expected error")
	}
}

func TestSafeDialContext_BlocksPrivateIP(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:80", "192.168.1.1:80", "10.0.0.1:80", "172.16.0.1:80", "[::1]:80"} {
		_, err := safeDialContext(context.Background(), "tcp", addr)
		if err == nil {
			t.Errorf("expected error for %q", addr)
		}
	}
}

func TestSafeDialContext_AllowsPublicIP(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err := safeDialContext(ctx, "tcp", "1.1.1.1:80")
	if err != nil && strings.Contains(err.Error(), "refused") {
		t.Fatalf("unexpected SSRF block for public IP: %v", err)
	}
}

