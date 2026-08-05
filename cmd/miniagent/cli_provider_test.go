package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// e2e：config provider.key 提供 key，进入请求 Authorization。
func TestCLI_ProviderKeyAuth(t *testing.T) {
	var mu sync.Mutex
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotAuth = r.Header.Get("Authorization")
		mu.Unlock()
		_, _ = fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer srv.Close()

	cfgPath := filepath.Join(t.TempDir(), "miniagent.json")
	body := `{"providers":[{"name":"p","chat_url":"` + srv.URL + `/v1/chat/completions","key":"sk-from-config"}],"defaults":{"provider":"p","model":"m","mode":"auto"},"compaction":{"provider":"p","model":"m"},"memory":{"provider":"p","model":"m"}}`
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out := runMainBin(t, "ping", []string{"-config", cfgPath})
	if code != 0 {
		t.Fatalf("code = %d, out = %s", code, out)
	}
	mu.Lock()
	defer mu.Unlock()
	if gotAuth != "Bearer sk-from-config" {
		t.Errorf("Authorization = %q", gotAuth)
	}
}
