package main

import (
	"net/http"
	"testing"
	"time"

	"github.com/justphantom/miniagent/internal/miniagent"
)

// P1-A：buildLLM 注入带总 Timeout 的 client（非流式 Do 需总超时兜底防挂死，#3）+ Transport
// 级超时（连接/首字节/TLS）+ Proxy（与 fetch 对齐，#2）。流式 DoStream 经 streamHTTPClient
// 改用其 Transport 另造无 Timeout client（body 不被砍）。
func TestBuildLLM_HTTPClientTimeoutAndProxy(t *testing.T) {
	llm := buildLLM("sk", miniagent.ProviderConfig{ChatURL: "http://localhost:1234/v1/chat/completions"}, nil)
	if llm.HTTP == nil {
		t.Fatal("HTTPClient.HTTP is nil, want injected client")
	}
	if llm.HTTP.Timeout != 120*time.Second {
		t.Errorf("injected client Timeout = %v, want 120s（非流式 Do 总超时兜底，#3）", llm.HTTP.Timeout)
	}
	tr, ok := llm.HTTP.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport = %T, want *http.Transport", llm.HTTP.Transport)
	}
	if tr.Proxy == nil {
		t.Error("Transport.Proxy = nil, want http.ProxyFromEnvironment（#2，与 fetch 对齐）")
	}
	if tr.DialContext == nil {
		t.Error("Transport.DialContext not set（拨号超时）")
	}
	if tr.ResponseHeaderTimeout == 0 {
		t.Error("Transport.ResponseHeaderTimeout = 0, want >0")
	}
}
