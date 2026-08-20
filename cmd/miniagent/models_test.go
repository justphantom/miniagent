package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/justphantom/miniagent/config"
)

// listAllModels static entries: no ModelsURL → StaticModels surfaced without any HTTP.
func TestListAllModels_Static(t *testing.T) {
	providers := []config.ProviderConfig{
		{Name: "p", Models: []config.ModelConfig{{Name: "a"}, {Name: "b"}}},
	}
	ids, err := listAllModels(context.Background(), providers, 0, nil)
	if err != nil {
		t.Fatalf("listAllModels: %v", err)
	}
	if len(ids) != 2 || ids[0].Provider != "p" || ids[0].Model != "a" || ids[1].Model != "b" {
		t.Fatalf("ids = %+v", ids)
	}
}

// listAllModels dynamic: two providers aggregated, both hit their models_url.
func TestListAllModels_Dynamic(t *testing.T) {
	srv1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"o1"}]}`)
	}))
	defer srv1.Close()
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"o2"}]}`)
	}))
	defer srv2.Close()
	providers := []config.ProviderConfig{
		{Name: "openai", ChatURL: srv1.URL, ModelsURL: srv1.URL + "/v1/models"},
		{Name: "deepseek", ChatURL: srv2.URL, ModelsURL: srv2.URL + "/v1/models"},
	}
	ids, err := listAllModels(context.Background(), providers, 30*time.Second, nil)
	if err != nil {
		t.Fatalf("listAllModels: %v", err)
	}
	if len(ids) != 2 {
		t.Fatalf("ids = %+v, want 2 entries", ids)
	}
}

// listAllModels: empty provider slice yields empty result, no error.
func TestListAllModels_Empty(t *testing.T) {
	ids, err := listAllModels(context.Background(), []config.ProviderConfig{}, 0, nil)
	if err != nil {
		t.Fatalf("listAllModels: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("ids = %+v, want empty", ids)
	}
}

// listAllModels partial failure: the healthy provider's models still returned; err non-nil.
func TestListAllModels_PartialFailure(t *testing.T) {
	srv1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"ok1"}]}`)
	}))
	defer srv1.Close()
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv2.Close()
	providers := []config.ProviderConfig{
		{Name: "ok", ChatURL: srv1.URL, ModelsURL: srv1.URL + "/v1/models"},
		{Name: "fail", ChatURL: srv2.URL, ModelsURL: srv2.URL + "/v1/models"},
	}
	ids, err := listAllModels(context.Background(), providers, 30*time.Second, nil)
	if err == nil {
		t.Fatal("expected error from failing provider")
	}
	if len(ids) != 1 || ids[0].Model != "ok1" {
		t.Fatalf("ids = %+v, want ok1 only", ids)
	}
}

// listAllModels passes the provider-specific key to the endpoint.
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
	providers := []config.ProviderConfig{
		{Name: "a", Key: "key-a", ChatURL: srv.URL, ModelsURL: srv.URL + "/v1/models"},
		{Name: "b", Key: "key-b", ChatURL: srv.URL, ModelsURL: srv.URL + "/v1/models"},
	}
	if _, err := listAllModels(context.Background(), providers, 30*time.Second, nil); err != nil {
		t.Fatalf("listAllModels: %v", err)
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

// listAllModels preserves input provider order (stable output contract).
func TestListAllModels_OrderStable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"x"}]}`)
	}))
	defer srv.Close()
	providers := []config.ProviderConfig{
		{Name: "p1", ChatURL: srv.URL, ModelsURL: srv.URL + "/v1/models"},
		{Name: "p2", ChatURL: srv.URL, ModelsURL: srv.URL + "/v1/models"},
		{Name: "p3", ChatURL: srv.URL, ModelsURL: srv.URL + "/v1/models"},
	}
	ids, err := listAllModels(context.Background(), providers, 30*time.Second, nil)
	if err != nil {
		t.Fatalf("listAllModels: %v", err)
	}
	if len(ids) != 3 || ids[0].Provider != "p1" || ids[1].Provider != "p2" || ids[2].Provider != "p3" {
		t.Fatalf("order not stable: %+v", ids)
	}
}
