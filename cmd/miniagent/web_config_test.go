package main

// web_config_test.go covers GET/PUT /api/config (masking, write-back, divergence); the reload
// endpoint's tests live in web_config_reload_test.go (split to stay under the 300-line ceiling).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
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
	before := *s.cfg
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
	// C1: the running config must NOT be swapped — s.cfg stays the pointer other handlers read
	// without a lock; the file is the source of truth and changes take effect on restart.
	if !configEqual(&before, s.cfg) {
		t.Error("s.cfg was mutated by PUT — runtime swap must not happen")
	}
	// The saved config is echoed back masked, so the UI can refill the form.
	if resp.Config == nil {
		t.Error("expected saved config echoed back for form refill")
	}
	for _, p := range resp.Config.Providers {
		if p.Key == "sk-plain" {
			t.Error("saved config must mask provider.key")
		}
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
	if s.cfg.Run.MaxTokens != nil {
		t.Error("s.cfg.Run.MaxTokens must stay nil after PUT — no runtime swap")
	}
}

// C2: renaming a provider while its key shows the mask must fail the save — the name-keyed
// lookup cannot resolve the old key, and silently persisting the literal mask would break
// that provider's auth with no error anywhere.
func TestConfigPut_RenamedProviderWithMaskedKeyRejected(t *testing.T) {
	s, _ := cfgTestServer(t)
	edited := *s.cfg
	edited.Providers = make([]config.ProviderConfig, len(s.cfg.Providers))
	copy(edited.Providers, s.cfg.Providers)
	edited.Providers[0].Name = "renamed"
	edited.Providers[0].Key = maskedSecret // client kept the masked display value
	body, _ := json.Marshal(edited)
	rec := httptest.NewRecorder()
	s.mux().ServeHTTP(rec, configReq(http.MethodPut, string(body)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400 (masked key + rename cannot ride one save)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "renamed") {
		t.Errorf("error should name the offending provider: %s", rec.Body.String())
	}
}

func intPtr(n int) *int { return &n }

func TestConfigGet_DivergenceDetection(t *testing.T) {
	s, path := cfgTestServer(t)

	// Identical file and running config → no divergence.
	rec := httptest.NewRecorder()
	s.mux().ServeHTTP(rec, configReq(http.MethodGet, ""))
	var resp configGetResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Diverged || len(resp.Diff) != 0 {
		t.Errorf("identical configs: diverged=%v diff=%v, want none", resp.Diverged, resp.Diff)
	}

	// External edit of the file → GET must report drift with the field path only.
	edited, err := config.LoadConfig(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	edited.Run.MaxTokens = intPtr(4242)
	if err := config.SaveConfig(path, edited); err != nil {
		t.Fatalf("save edited: %v", err)
	}
	rec = httptest.NewRecorder()
	s.mux().ServeHTTP(rec, configReq(http.MethodGet, ""))
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if !resp.Diverged {
		t.Error("external file edit must set diverged=true")
	}
	if len(resp.Diff) != 1 || resp.Diff[0] != "run.max_tokens" {
		t.Errorf("diff = %v, want [run.max_tokens]", resp.Diff)
	}
	// The edit basis is the FILE config (4242), not the running value.
	if resp.Config == nil || resp.Config.Run.MaxTokens == nil || *resp.Config.Run.MaxTokens != 4242 {
		t.Errorf("edit basis must be the file config")
	}
}

func TestConfigGet_CorruptFileFallsBackToRunning(t *testing.T) {
	s, path := cfgTestServer(t)
	if err := os.WriteFile(path, []byte("{oops"), 0o600); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	s.mux().ServeHTTP(rec, configReq(http.MethodGet, ""))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET must not 500 on a corrupt file: code=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp configGetResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.FileError == "" {
		t.Error("file_error must be set for a corrupt config file")
	}
	if resp.Diverged {
		t.Error("unreadable file must not report divergence")
	}
	if resp.Config == nil || resp.Config.Defaults.Provider != "p" {
		t.Error("fallback edit basis must be the running config")
	}
}
