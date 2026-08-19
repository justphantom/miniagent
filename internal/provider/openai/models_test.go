package openai

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

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
