package openai

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/justphantom/miniagent/internal/miniagent/config"
)

func TestListModels_NonOKErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, "err")
	}))
	defer srv.Close()
	llm := &ChatClient{APIKey: "sk", ChatURL: srv.URL, ModelsURL: srv.URL + "/v1/models"}
	if _, err := llm.ListModels(context.Background()); err == nil {
		t.Error("non-200 should error")
	}
}

func TestListModels_EmptyData(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[]}`)
	}))
	defer srv.Close()
	llm := &ChatClient{APIKey: "sk", ChatURL: srv.URL, ModelsURL: srv.URL + "/v1/models"}
	ids, err := llm.ListModels(context.Background())
	if err != nil {
		t.Fatalf("empty data: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("want empty, got %v", ids)
	}
}

func keyForTest(_ config.ProviderConfig) string { return "sk-test" }

func TestListAllModels_StaticNoGET(t *testing.T) {
	// Empty ModelsURL + static Models -> returned directly, never sends HTTP (no server needed proves no GET).
	providers := []config.ProviderConfig{{Name: "p", Models: []config.ModelConfig{{Name: "a"}, {Name: "b"}}}}
	ids, err := ListAllModels(context.Background(), providers, keyForTest, nil, nil)
	if err != nil {
		t.Fatalf("static list: %v", err)
	}
	if len(ids) != 2 || ids[0] != (ProviderModel{Provider: "p", Model: "a"}) {
		t.Errorf("ids = %v", ids)
	}
}

func TestListAllModels_StaticEmptyErrors(t *testing.T) {
	providers := []config.ProviderConfig{{Name: "p"}}
	if _, err := ListAllModels(context.Background(), providers, keyForTest, nil, nil); err == nil {
		t.Error("empty static models should error")
	}
}

func TestListAllModels_MultiProvider(t *testing.T) {
	srv1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"gpt-4o"},{"id":"gpt-3.5"}]}`)
	}))
	defer srv1.Close()
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"deepseek-chat"},{"id":"deepseek-coder"}]}`)
	}))
	defer srv2.Close()

	providers := []config.ProviderConfig{
		{Name: "openai", ChatURL: srv1.URL, ModelsURL: srv1.URL + "/v1/models"},
		{Name: "deepseek", ChatURL: srv2.URL, ModelsURL: srv2.URL + "/v1/models"},
	}
	ids, err := ListAllModels(context.Background(), providers, keyForTest, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := map[ProviderModel]bool{
		{Provider: "openai", Model: "gpt-4o"}:           true,
		{Provider: "openai", Model: "gpt-3.5"}:          true,
		{Provider: "deepseek", Model: "deepseek-chat"}:  true,
		{Provider: "deepseek", Model: "deepseek-coder"}: true,
	}
	if len(ids) != 4 {
		t.Fatalf("want 4 ids, got %d: %v", len(ids), ids)
	}
	for _, id := range ids {
		if !want[id] {
			t.Errorf("unexpected ref: %+v", id)
		}
	}
}

func TestListAllModels_MixedStaticAndDynamic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"dynamic-model"}]}`)
	}))
	defer srv.Close()

	providers := []config.ProviderConfig{
		{Name: "static", Models: []config.ModelConfig{{Name: "static-1"}, {Name: "static-2"}}},
		{Name: "dynamic", ChatURL: srv.URL, ModelsURL: srv.URL + "/v1/models"},
	}
	ids, err := ListAllModels(context.Background(), providers, keyForTest, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := map[ProviderModel]bool{
		{Provider: "static", Model: "static-1"}:       true,
		{Provider: "static", Model: "static-2"}:       true,
		{Provider: "dynamic", Model: "dynamic-model"}: true,
	}
	if len(ids) != 3 {
		t.Fatalf("want 3 ids, got %d: %v", len(ids), ids)
	}
	for _, id := range ids {
		if !want[id] {
			t.Errorf("unexpected ref: %+v", id)
		}
	}
}

func TestListAllModels_PartialFailure(t *testing.T) {
	srv1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"model-a"}]}`)
	}))
	defer srv1.Close()
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unreachable", http.StatusInternalServerError)
	}))
	defer srv2.Close()

	providers := []config.ProviderConfig{
		{Name: "ok", ChatURL: srv1.URL, ModelsURL: srv1.URL + "/v1/models"},
		{Name: "fail", ChatURL: srv2.URL, ModelsURL: srv2.URL + "/v1/models"},
	}
	ids, err := ListAllModels(context.Background(), providers, keyForTest, nil, nil)
	if err == nil {
		t.Fatal("expected error from failed provider")
	}
	if len(ids) != 1 || ids[0] != (ProviderModel{Provider: "ok", Model: "model-a"}) {
		t.Errorf("want [{ok model-a}], got %v", ids)
	}
	if !strings.Contains(err.Error(), "fail") {
		t.Errorf("error should mention failing provider: %v", err)
	}
}

func TestListAllModels_EmptyProviders(t *testing.T) {
	ids, err := ListAllModels(context.Background(), []config.ProviderConfig{}, keyForTest, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("want empty, got %v", ids)
	}
}

func TestListAllModels_PerProviderKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		fmt.Fprintf(w, `{"data":[{"id":"%s"}]}`, auth)
	}))
	defer srv.Close()

	providers := []config.ProviderConfig{
		{Name: "a", ChatURL: srv.URL, ModelsURL: srv.URL + "/v1/models", Key: "key-a"},
		{Name: "b", ChatURL: srv.URL, ModelsURL: srv.URL + "/v1/models", Key: "key-b"},
	}
	keys := map[string]string{"a": "shared"}
	ids, err := ListAllModels(context.Background(), providers, func(p config.ProviderConfig) string {
		if k, ok := keys[p.Name]; ok {
			return k
		}
		return p.Key
	}, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ids[0].Model != "Bearer shared" || ids[1].Model != "Bearer key-b" {
		t.Errorf("per-provider key not respected: %v", ids)
	}
}

func TestListAllModels_DeterministicOrder(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"m"}]}`)
	}))
	defer srv.Close()

	providers := []config.ProviderConfig{
		{Name: "p1", ChatURL: srv.URL, ModelsURL: srv.URL + "/v1/models"},
		{Name: "p2", ChatURL: srv.URL, ModelsURL: srv.URL + "/v1/models"},
		{Name: "p3", ChatURL: srv.URL, ModelsURL: srv.URL + "/v1/models"},
	}
	ids, err := ListAllModels(context.Background(), providers, keyForTest, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []ProviderModel{{Provider: "p1", Model: "m"}, {Provider: "p2", Model: "m"}, {Provider: "p3", Model: "m"}}
	if len(ids) != len(want) {
		t.Fatalf("want %v, got %v", want, ids)
	}
	for i, id := range ids {
		if id != want[i] {
			t.Errorf("order mismatch at %d: got %+v, want %+v", i, id, want[i])
		}
	}
}

// ListModels carries the custom request headers.
func TestChatClient_ListModels_CustomHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Custom") != "xyz" {
			t.Errorf("X-Custom = %q, want xyz", r.Header.Get("X-Custom"))
		}
		fmt.Fprint(w, `{"data":[{"id":"gpt-4o"}]}`)
	}))
	defer srv.Close()
	c := &ChatClient{
		APIKey:    "sk",
		ChatURL:   srv.URL,
		ModelsURL: srv.URL + "/v1/models",
		Headers:   map[string]string{"X-Custom": "xyz"},
	}
	ids, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(ids) != 1 || ids[0].ID != "gpt-4o" {
		t.Errorf("ids = %v", ids)
	}
}

// ListModels parses the non-standard context_window/max_output_tokens extensions when present.
func TestListModels_ParsesLimits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"DeepSeek-V4-Flash-amd-openai","object":"model","created":1786887755,"owned_by":"local","upstream":"amd","protocol":"openai","context_window":204800,"max_output_tokens":65536},{"id":"agnes-2.5-flash-openai","context_window":524288,"max_output_tokens":65536},{"id":"plain"}]}`)
	}))
	defer srv.Close()
	c := &ChatClient{APIKey: "sk", ModelsURL: srv.URL + "/v1/models"}
	ids, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(ids) != 3 {
		t.Fatalf("want 3 models, got %d", len(ids))
	}
	if ids[0].ContextWindow == nil || *ids[0].ContextWindow != 204800 {
		t.Errorf("context_window = %v, want 204800", ids[0].ContextWindow)
	}
	if ids[0].MaxOutputTokens == nil || *ids[0].MaxOutputTokens != 65536 {
		t.Errorf("max_output_tokens = %v, want 65536", ids[0].MaxOutputTokens)
	}
	if ids[1].ContextWindow == nil || *ids[1].ContextWindow != 524288 {
		t.Errorf("model[1] context_window = %v, want 524288", ids[1].ContextWindow)
	}
	if ids[2].ContextWindow != nil || ids[2].MaxOutputTokens != nil {
		t.Errorf("model without limit fields should parse nil limits, got %+v", ids[2])
	}
}
