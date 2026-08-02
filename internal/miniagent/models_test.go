package miniagent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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

func keyForTest(_ ProviderConfig) string { return "sk-test" }

func TestListAllModels_MultiProvider(t *testing.T) {
	srv1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"gpt-4o"},{"id":"gpt-3.5"}]}`)
	}))
	defer srv1.Close()
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"deepseek-chat"},{"id":"deepseek-coder"}]}`)
	}))
	defer srv2.Close()

	providers := []ProviderConfig{
		{Name: "openai", ChatURL: srv1.URL, ModelsURL: srv1.URL + "/v1/models"},
		{Name: "deepseek", ChatURL: srv2.URL, ModelsURL: srv2.URL + "/v1/models"},
	}
	ids, err := ListAllModels(context.Background(), providers, keyForTest, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := map[string]bool{
		"openai/gpt-4o":           true,
		"openai/gpt-3.5":          true,
		"deepseek/deepseek-chat":  true,
		"deepseek/deepseek-coder": true,
	}
	if len(ids) != 4 {
		t.Fatalf("want 4 ids, got %d: %v", len(ids), ids)
	}
	for _, id := range ids {
		if !want[id] {
			t.Errorf("unexpected id: %s", id)
		}
	}
}

func TestListAllModels_MixedStaticAndDynamic(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"dynamic-model"}]}`)
	}))
	defer srv.Close()

	providers := []ProviderConfig{
		{Name: "static", Models: []string{"static-1", "static-2"}},
		{Name: "dynamic", ChatURL: srv.URL, ModelsURL: srv.URL + "/v1/models"},
	}
	ids, err := ListAllModels(context.Background(), providers, keyForTest, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := map[string]bool{
		"static/static-1":       true,
		"static/static-2":       true,
		"dynamic/dynamic-model": true,
	}
	if len(ids) != 3 {
		t.Fatalf("want 3 ids, got %d: %v", len(ids), ids)
	}
	for _, id := range ids {
		if !want[id] {
			t.Errorf("unexpected id: %s", id)
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

	providers := []ProviderConfig{
		{Name: "ok", ChatURL: srv1.URL, ModelsURL: srv1.URL + "/v1/models"},
		{Name: "fail", ChatURL: srv2.URL, ModelsURL: srv2.URL + "/v1/models"},
	}
	ids, err := ListAllModels(context.Background(), providers, keyForTest, nil, nil)
	if err == nil {
		t.Fatal("expected error from failed provider")
	}
	if len(ids) != 1 || ids[0] != "ok/model-a" {
		t.Errorf("want [ok/model-a], got %v", ids)
	}
	if !strings.Contains(err.Error(), "fail") {
		t.Errorf("error should mention failing provider: %v", err)
	}
}

func TestListAllModels_EmptyProviders(t *testing.T) {
	ids, err := ListAllModels(context.Background(), []ProviderConfig{}, keyForTest, nil, nil)
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

	providers := []ProviderConfig{
		{Name: "a", ChatURL: srv.URL, ModelsURL: srv.URL + "/v1/models", Key: "key-a"},
		{Name: "b", ChatURL: srv.URL, ModelsURL: srv.URL + "/v1/models", Key: "key-b"},
	}
	keys := map[string]string{"a": "shared"}
	ids, err := ListAllModels(context.Background(), providers, func(p ProviderConfig) string {
		if k, ok := keys[p.Name]; ok {
			return k
		}
		return p.Key
	}, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(ids[0], "Bearer shared") || !strings.Contains(ids[1], "Bearer key-b") {
		t.Errorf("per-provider key not respected: %v", ids)
	}
}

func TestListAllModels_DeterministicOrder(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"m"}]}`)
	}))
	defer srv.Close()

	providers := []ProviderConfig{
		{Name: "p1", ChatURL: srv.URL, ModelsURL: srv.URL + "/v1/models"},
		{Name: "p2", ChatURL: srv.URL, ModelsURL: srv.URL + "/v1/models"},
		{Name: "p3", ChatURL: srv.URL, ModelsURL: srv.URL + "/v1/models"},
	}
	ids, err := ListAllModels(context.Background(), providers, keyForTest, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []string{"p1/m", "p2/m", "p3/m"}
	if len(ids) != len(want) {
		t.Fatalf("want %v, got %v", want, ids)
	}
	for i, id := range ids {
		if id != want[i] {
			t.Errorf("order mismatch at %d: got %s, want %s", i, id, want[i])
		}
	}
}
