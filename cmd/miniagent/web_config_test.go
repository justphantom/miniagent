package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/justphantom/miniagent/config"
)

// cfgTestServer builds a webServer with write-back enabled: seeds a temp config file,
// loads it, and sets cfgPath so PUT /api/config can write back.
func cfgTestServer(t *testing.T) (*webServer, string) {
	t.Helper()
	dir := t.TempDir()
	path := dir + "/miniagent.json"
	seed := &config.Config{
		Providers: []config.ProviderConfig{{Name: "p", ChatURL: "http://127.0.0.1:1/v1/chat/completions", Key: "sk-plain", Models: []config.ModelConfig{{Name: "m"}}}},
		Defaults:  config.DefaultsConfig{Provider: "p", Model: "m"},
	}
	if err := config.SaveConfig(path, seed); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	loaded, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("load seeded config: %v", err)
	}
	s := newWebServer(context.Background(), loaded, &turnEngine{cfg: loaded, logger: testLogger()}, "", testLogger())
	s.setCfgPath(path)
	return s, path
}

func configReq(method, body string) *http.Request {
	r := httptest.NewRequestWithContext(context.Background(), method, "/api/config", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	return r
}

func TestConfigGet_MasksSecrets(t *testing.T) {
	s, _ := cfgTestServer(t)
	rec := httptest.NewRecorder()
	s.mux().ServeHTTP(rec, configReq(http.MethodGet, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp configGetResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Writable {
		t.Error("expected writable=true when cfgPath set")
	}
	for _, p := range resp.Config.Providers {
		if p.Key == "sk-plain" {
			t.Errorf("provider.key leaked in GET response")
		}
		if p.Key != maskedSecret {
			t.Errorf("provider.key = %q, want masked sentinel", p.Key)
		}
	}
}

func TestConfigPut_PreservesMaskedSecret(t *testing.T) {
	s, path := cfgTestServer(t)
	edited := *s.cfg
	edited.Providers = make([]config.ProviderConfig, len(s.cfg.Providers))
	copy(edited.Providers, s.cfg.Providers)
	edited.Providers[0].Key = maskedSecret
	edited.Run.MaxTokens = intPtr(9999)
	body, _ := json.Marshal(edited)
	rec := httptest.NewRecorder()
	s.mux().ServeHTTP(rec, configReq(http.MethodPut, string(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", rec.Code, rec.Body.String())
	}
	reloaded, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.Providers[0].Key != "sk-plain" {
		t.Errorf("provider.key = %q, want preserved sk-plain", reloaded.Providers[0].Key)
	}
	if reloaded.Run.MaxTokens == nil || *reloaded.Run.MaxTokens != 9999 {
		t.Errorf("max_tokens = %+v, want 9999", reloaded.Run.MaxTokens)
	}
}

func TestConfigPut_InvalidReturnsBadRequest(t *testing.T) {
	s, _ := cfgTestServer(t)
	edited := config.Config{Defaults: config.DefaultsConfig{Provider: "p", Model: "m"}}
	body, _ := json.Marshal(edited)
	rec := httptest.NewRecorder()
	s.mux().ServeHTTP(rec, configReq(http.MethodPut, string(body)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400", rec.Code)
	}
}

func TestConfigPut_NotWritableReturns404(t *testing.T) {
	s, _ := cfgTestServer(t)
	s.cfgPath = ""
	body, _ := json.Marshal(s.cfg)
	rec := httptest.NewRecorder()
	s.mux().ServeHTTP(rec, configReq(http.MethodPut, string(body)))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404", rec.Code)
	}
}

func TestConfigPut_NeedRestartDetection(t *testing.T) {
	s, _ := cfgTestServer(t)
	body, _ := json.Marshal(s.cfg)
	rec := httptest.NewRecorder()
	s.mux().ServeHTTP(rec, configReq(http.MethodPut, string(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	var resp configPutResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.NeedRestart {
		t.Error("identical config should not require restart")
	}
	edited := *s.cfg
	edited.Run.MaxTokens = intPtr(12345)
	body, _ = json.Marshal(edited)
	rec = httptest.NewRecorder()
	s.mux().ServeHTTP(rec, configReq(http.MethodPut, string(body)))
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if !resp.NeedRestart {
		t.Error("changed config should require restart")
	}
}

func intPtr(n int) *int { return &n }
