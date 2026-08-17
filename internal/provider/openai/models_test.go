package openai

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func keyForTest(_ ModelSource) string { return "sk-test" }

// ListModels non-200: error mentions status.
func TestListModels_NonOKErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	llm := &ChatClient{APIKey: "sk", ChatURL: srv.URL, ModelsURL: srv.URL + "/v1/models"}
	if _, err := llm.ListModels(context.Background()); err == nil {
		t.Fatal("expected error on non-200")
	}
}

// Empty data: empty slice, no error.
func TestListModels_EmptyData(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[]}`)
	}))
	defer srv.Close()
	llm := &ChatClient{APIKey: "sk", ChatURL: srv.URL, ModelsURL: srv.URL + "/v1/models"}
	ids, err := llm.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("ids = %v, want empty", ids)
	}
}

// Empty ModelsURL: ChatClient.ListModels errors (models_url not configured); static fallback lives in
// ListAllModels via ModelSource.StaticModels, never a GET (no server needed proves no HTTP).
func TestListModels_StaticFallback(t *testing.T) {
	c, err := NewChatClient("sk", "http://example.invalid/v1/chat/completions", "", nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListModels(context.Background()); err == nil {
		t.Fatal("expected error when models_url is empty")
	}
}

// ListAllModels static entries: no ModelsURL -> StaticModels surfaced without any HTTP.
func TestListAllModels_Static(t *testing.T) {
	providers := []ModelSource{{Name: "p", StaticModels: []string{"a", "b"}}}
	ids, err := ListAllModels(context.Background(), providers, keyForTest, nil, nil)
	if err != nil {
		t.Fatalf("ListAllModels: %v", err)
	}
	if len(ids) != 2 || ids[0].Provider != "p" || ids[0].Model != "a" || ids[1].Model != "b" {
		t.Fatalf("ids = %+v", ids)
	}
}

// ListAllModels dynamic: two providers aggregated, both hit their models_url.
func TestListAllModels_Dynamic(t *testing.T) {
	srv1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"o1"}]}`)
	}))
	defer srv1.Close()
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"o2"}]}`)
	}))
	defer srv2.Close()
	providers := []ModelSource{
		{Name: "openai", ChatURL: srv1.URL, ModelsURL: srv1.URL + "/v1/models"},
		{Name: "deepseek", ChatURL: srv2.URL, ModelsURL: srv2.URL + "/v1/models"},
	}
	ids, err := ListAllModels(context.Background(), providers, keyForTest, nil, nil)
	if err != nil {
		t.Fatalf("ListAllModels: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("ids = %+v, want 2 entries", ids)
	}
}

// ListAllModels: empty provider slice yields empty result, no error.
func TestListAllModels_Empty(t *testing.T) {
	ids, err := ListAllModels(context.Background(), []ModelSource{}, keyForTest, nil, nil)
	if err != nil {
		t.Fatalf("ListAllModels: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("ids = %+v, want empty", ids)
	}
}

// ListAllModels partial failure: the healthy provider's models still returned; err non-nil.
func TestListAllModels_PartialFailure(t *testing.T) {
	srv1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"ok1"}]}`)
	}))
	defer srv1.Close()
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv2.Close()
	providers := []ModelSource{
		{Name: "ok", ChatURL: srv1.URL, ModelsURL: srv1.URL + "/v1/models"},
		{Name: "fail", ChatURL: srv2.URL, ModelsURL: srv2.URL + "/v1/models"},
	}
	ids, err := ListAllModels(context.Background(), providers, keyForTest, nil, nil)
	if err == nil {
		t.Fatal("expected error from failing provider")
	}
	if len(ids) != 1 || ids[0].Model != "ok1" {
		t.Fatalf("ids = %+v, want ok1 only", ids)
	}
}

// ListAllModels passes the provider-specific key to the endpoint.
func TestListAllModels_KeyPerProvider(t *testing.T) {
	var mu sync.Mutex
	got := make(map[string]string)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got[r.Header.Get("Authorization")] = ""
		mu.Unlock()
		fmt.Fprint(w, `{"data":[{"id":"k"}]}`)
	}))
	defer srv.Close()
	providers := []ModelSource{
		{Name: "a", ChatURL: srv.URL, ModelsURL: srv.URL + "/v1/models"},
		{Name: "b", ChatURL: srv.URL, ModelsURL: srv.URL + "/v1/models"},
	}
	if _, err := ListAllModels(context.Background(), providers, func(p ModelSource) string {
		return "key-" + p.Name
	}, nil, nil); err != nil {
		t.Fatalf("ListAllModels: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if _, ok := got["Bearer key-a"]; !ok {
		t.Fatalf("missing Bearer key-a, got %v", got)
	}
	if _, ok := got["Bearer key-b"]; !ok {
		t.Fatalf("missing Bearer key-b, got %v", got)
	}
}

// ListAllModels preserves input provider order (stable output contract).
func TestListAllModels_OrderStable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"x"}]}`)
	}))
	defer srv.Close()
	providers := []ModelSource{
		{Name: "p1", ChatURL: srv.URL, ModelsURL: srv.URL + "/v1/models"},
		{Name: "p2", ChatURL: srv.URL, ModelsURL: srv.URL + "/v1/models"},
		{Name: "p3", ChatURL: srv.URL, ModelsURL: srv.URL + "/v1/models"},
	}
	ids, err := ListAllModels(context.Background(), providers, keyForTest, nil, nil)
	if err != nil {
		t.Fatalf("ListAllModels: %v", err)
	}
	if len(ids) != 3 || ids[0].Provider != "p1" || ids[1].Provider != "p2" || ids[2].Provider != "p3" {
		t.Fatalf("order not stable: %+v", ids)
	}
}

// ListModels carries the custom request headers.
func TestChatClient_ListModels_CustomHeaders(t *testing.T) {
	var gotTenant string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTenant = r.Header.Get("X-Tenant-Id")
		fmt.Fprint(w, `{"data":[{"id":"m"}]}`)
	}))
	defer srv.Close()
	c := &ChatClient{
		APIKey:    "sk",
		ChatURL:   srv.URL,
		ModelsURL: srv.URL + "/v1/models",
		Headers:   map[string]string{"X-Tenant-Id": "tn"},
	}
	if _, err := c.ListModels(context.Background()); err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if gotTenant != "tn" {
		t.Fatalf("X-Tenant-Id = %q, want tn", gotTenant)
	}
}

// ListModels parses the non-standard context_window/max_output_tokens extensions when present.
func TestListModels_ParsesLimits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"m","context_window":200000,"max_output_tokens":32000}]}`)
	}))
	defer srv.Close()
	c := &ChatClient{APIKey: "sk", ModelsURL: srv.URL + "/v1/models"}
	ids, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(ids) != 1 || ids[0].ContextWindow == nil || *ids[0].ContextWindow != 200000 || ids[0].MaxOutputTokens == nil || *ids[0].MaxOutputTokens != 32000 {
		t.Fatalf("limits not parsed: %+v", ids)
	}
}

// ListAllModels kind=anthropic: dispatches to the Anthropic /v1/models endpoint (x-api-key auth).
func TestListAllModels_AnthropicDispatch(t *testing.T) {
	var gotKey, gotVer string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-Api-Key")
		gotVer = r.Header.Get("Anthropic-Version")
		fmt.Fprint(w, `{"data":[{"id":"claude-opus-4-8","context_window":200000,"max_output_tokens":32000}]}`)
	}))
	defer srv.Close()
	providers := []ModelSource{{Name: "ant", Kind: "anthropic", ChatURL: srv.URL, ModelsURL: srv.URL + "/v1/models"}}
	ids, err := ListAllModels(context.Background(), providers, keyForTest, nil, nil)
	if err != nil {
		t.Fatalf("ListAllModels: %v", err)
	}
	if gotKey != "sk-test" || gotVer == "" {
		t.Fatalf("anthropic auth headers missing: key=%q ver=%q", gotKey, gotVer)
	}
	if len(ids) != 1 || ids[0].Model != "claude-opus-4-8" {
		t.Fatalf("ids = %+v", ids)
	}
	if ids[0].Limits.ContextWindow == nil || *ids[0].Limits.ContextWindow != 200000 {
		t.Fatalf("anthropic limits not surfaced: %+v", ids[0].Limits)
	}
}
