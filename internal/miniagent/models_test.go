package miniagent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestListModels_NonOKErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, "err")
	}))
	defer srv.Close()
	llm := &HTTPClient{APIKey: "sk", ChatURL: srv.URL, ModelsURL: srv.URL + "/v1/models"}
	if _, err := llm.ListModels(context.Background()); err == nil {
		t.Error("non-200 should error")
	}
}

func TestListModels_EmptyData(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[]}`)
	}))
	defer srv.Close()
	llm := &HTTPClient{APIKey: "sk", ChatURL: srv.URL, ModelsURL: srv.URL + "/v1/models"}
	ids, err := llm.ListModels(context.Background())
	if err != nil {
		t.Fatalf("empty data should not error: %v", err)
	}
	if len(ids) != 0 {
		t.Errorf("want empty, got %v", ids)
	}
}
