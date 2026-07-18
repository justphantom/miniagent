package miniagent

import (
	"context"
	"net"
	"net/http"
	"os/exec"
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

// 超时后整组清理：sh 的孙进程不应残留。
// 跗 short 模式跳过：测试需等 shellTimeout(60s) 触发，耗时过长。
func TestShell_KillsGrandchildOnTimeout(t *testing.T) {
	if testing.Short() {
		t.Skip("requires shellTimeout to elapse")
	}
	if _, err := exec.LookPath("pgrep"); err != nil {
		t.Skip("pgrep not available")
	}
	marker := "miniagent_uniq_sleep_marker_9f3k2"
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = exec.CommandContext(ctx, "pkill", "-9", "-f", marker).Run()
	})
	s := ShellTool(t.TempDir(), false, nil)
	// exec -a 让 sleep 进程名带 marker，pgrep -f 才能精确匹配。
	start := time.Now()
	res := s.Call(context.Background(), `{"command":"exec -a `+marker+` sleep 600"}`)
	elapsed := time.Since(start)
	if !res.IsError {
		t.Error("expected timeout error")
	}
	if elapsed > 75*time.Second {
		t.Errorf("timeout not enforced: elapsed=%v", elapsed)
	}
	time.Sleep(time.Second)
	pgrepCtx, pgrepCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer pgrepCancel()
	out, err := exec.CommandContext(pgrepCtx, "pgrep", "-f", marker).Output()
	if err == nil && len(strings.TrimSpace(string(out))) > 0 {
		t.Errorf("grandchild still alive after kill: %s", out)
	}
}

func TestShell_EmptyWorkspaceRoot(t *testing.T) {
	s := ShellTool("", false, nil)
	res := s.Call(context.Background(), `{"command":"echo x"}`)
	if !res.IsError {
		t.Fatal("expected error")
	}
}

// 用户传空 JSON 数组必须回落默认黑名单，不能静默放开破坏性命令。
func TestShell_EmptyBlockedPatternsFallsBack(t *testing.T) {
	s := ShellTool(t.TempDir(), false, []string{})
	res := s.Call(context.Background(), `{"command":"rm -rf /tmp/x"}`)
	if !res.IsError {
		t.Fatal("expected rm -rf to be blocked even with empty blockedPatterns")
	}
	if !strings.Contains(res.Output, "黑名单") {
		t.Errorf("error = %q", res.Output)
	}
}

// free 模式 + 空 workdir：cmd.Dir="" 由 exec 解释为进程 cwd，不应被
// ensureWorkspaceDir 拒绝（受限模式才需要工作目录约束）。
func TestShell_FreeModeEmptyWorkdir(t *testing.T) {
	s := ShellTool("", true, nil)
	res := s.Call(context.Background(), `{"command":"echo ok-free"}`)
	if res.IsError {
		t.Fatalf("free mode should not require workdir: %s", res.Output)
	}
	if !strings.Contains(res.Output, "ok-free") {
		t.Errorf("Output = %q", res.Output)
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

func TestWebFetch_StripsCommentsAndCDATA(t *testing.T) {
	// 注释里含 >：旧实现会提前关闭 tag，泄漏后续内容。
	body := []byte(`<p>a <!-- x > y & <z> --> b</p><p>c <![CDATA[<raw>]]> d</p>`)
	got := htmlToText(body)
	if strings.Contains(got, "x > y") || strings.Contains(got, "<z>") {
		t.Errorf("comment leaked: %q", got)
	}
	if !strings.Contains(got, "<raw>") {
		t.Errorf("CDATA content lost: %q", got)
	}
	if !strings.Contains(got, "a") || !strings.Contains(got, "b") || !strings.Contains(got, "c") || !strings.Contains(got, "d") {
		t.Errorf("text body dropped: %q", got)
	}
}

func TestWebFetch_UnterminatedCommentEOF(t *testing.T) {
	got := htmlToText([]byte(`<p>ok</p><!-- never closed`))
	if !strings.Contains(got, "ok") {
		t.Errorf("body lost: %q", got)
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

// IPv6 封装段可作 SSRF 跳板，必须拒绝。
func TestIsPublicIP_BlocksIPv6Tunnels(t *testing.T) {
	cases := map[string]bool{
		"2002:7f00::1":         false, // 6to4 封装 127.0.0.1
		"2001::ce49:7f00:1":    false, // Teredo
		"64:ff9b::7f00:1":      false, // NAT64 WK
		"::1":                  false, // loopback
		"fe80::1":              false, // link-local
		"fc00::1":              false, // ULA
		"2606:4700:4700::1111": true,  // Cloudflare 公网
	}
	for ipStr, want := range cases {
		ip := net.ParseIP(ipStr)
		if ip == nil {
			t.Errorf("ParseIP(%q) failed", ipStr)
			continue
		}
		if got := isPublicIP(ip); got != want {
			t.Errorf("isPublicIP(%s) = %v, want %v", ipStr, got, want)
		}
	}
}

// 自定义非 *http.Transport 的 RoundTripper 必须回退到带 SSRF 防护的默认 Transport。
func TestWebFetch_NonTransportRTFallsBack(t *testing.T) {
	bypass := &fakeTransport{}
	c := &http.Client{Transport: bypass}
	got := webfetchClient(c)
	tr, ok := got.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("expected *http.Transport after fallback, got %T", got.Transport)
	}
	if tr.DialContext == nil {
		t.Error("DialContext not injected; SSRF bypass possible")
	}
}

func TestWebFetch_TransportRTKeepsSSRF(t *testing.T) {
	src := &http.Client{Transport: &http.Transport{}}
	got := webfetchClient(src)
	tr, ok := got.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("expected *http.Transport, got %T", got.Transport)
	}
	if tr.DialContext == nil {
		t.Error("DialContext not injected")
	}
}

// 用户传的 client 无 Timeout 时必须兜底，避免挂死。
func TestWebFetch_ClientMissingTimeoutFallback(t *testing.T) {
	src := &http.Client{Transport: &http.Transport{}}
	got := webfetchClient(src)
	if got.Timeout != webfetchTimeout {
		t.Errorf("Timeout = %v, want %v", got.Timeout, webfetchTimeout)
	}
}

// 用户显式设了 Timeout，应被保留不被覆盖。
func TestWebFetch_ClientKeepsUserTimeout(t *testing.T) {
	custom := 99 * time.Second
	src := &http.Client{Transport: &http.Transport{}, Timeout: custom}
	got := webfetchClient(src)
	if got.Timeout != custom {
		t.Errorf("Timeout = %v, want user's %v", got.Timeout, custom)
	}
}
