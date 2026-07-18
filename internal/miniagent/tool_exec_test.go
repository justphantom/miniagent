package miniagent

import (
	"context"
	"net/http"
	"net/http/httptest"
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
	t.Setenv("MY_CUSTOM_SECRET", "sec-LEAK")
	t.Setenv("SHELL_TEST_MARKER", "kept")
	s := ShellTool(t.TempDir(), false, nil)
	res := s.Call(context.Background(), `{"command":"env"}`)
	if res.IsError {
		t.Fatalf("shell env failed: %s", res.Output)
	}
	if strings.Contains(res.Output, "sk-LEAK") || strings.Contains(res.Output, "sec-LEAK") {
		t.Errorf("secret leaked: %q", res.Output)
	}
	if !strings.Contains(res.Output, "SHELL_TEST_MARKER=kept") {
		t.Errorf("marker missing: %q", res.Output)
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

func TestWebFetch_ParsesHTML(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><head><title>T</title><style>x{}</style></head><body><p>hello <b>world</b></p><script>alert(1)</script></body></html>`))
	}))
	defer srv.Close()
	res := WebFetchTool(nil).Call(context.Background(), `{"url":"`+srv.URL+`"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if !strings.Contains(res.Output, "hello world") {
		t.Errorf("Output = %q", res.Output)
	}
	if strings.Contains(res.Output, "alert") || strings.Contains(res.Output, "x{}") {
		t.Errorf("script/style not stripped: %q", res.Output)
	}
}

func TestWebFetch_NonHTTPSchemeRejected(t *testing.T) {
	for _, bad := range []string{"file:///etc/passwd", "ftp://x/y", "javascript:alert(1)"} {
		res := WebFetchTool(nil).Call(context.Background(), `{"url":"`+bad+`"}`)
		if !res.IsError {
			t.Errorf("expected error for %q", bad)
		}
	}
}

func TestWebFetch_Non2xxIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	res := WebFetchTool(nil).Call(context.Background(), `{"url":"`+srv.URL+`"}`)
	if !res.IsError {
		t.Fatal("expected error")
	}
	if !strings.Contains(res.Output, "404") {
		t.Errorf("Output = %q", res.Output)
	}
}

func TestWebFetch_PlainTextReturned(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("plain body"))
	}))
	defer srv.Close()
	res := WebFetchTool(nil).Call(context.Background(), `{"url":"`+srv.URL+`"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if res.Output != "plain body" {
		t.Errorf("Output = %q", res.Output)
	}
}

func TestWebFetch_ConnectionError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close()
	res := WebFetchTool(&http.Client{Timeout: 2 * time.Second}).Call(context.Background(), `{"url":"`+srv.URL+`"}`)
	if !res.IsError {
		t.Fatal("expected error")
	}
}
