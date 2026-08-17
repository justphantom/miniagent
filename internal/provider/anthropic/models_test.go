package anthropic

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClient_ListModels_NonOKErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"message":"no"}}`))
	}))
	defer srv.Close()
	c, err := NewClient("k", srv.URL, srv.URL+"/v1/models", srv.Client(), nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListModels(context.Background()); err == nil {
		t.Fatal("expected error on 403")
	}
}

func TestClient_ListModels_EmptyData(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()
	c, err := NewClient("k", srv.URL, srv.URL+"/v1/models", srv.Client(), nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListModels(context.Background()); err == nil {
		t.Fatal("expected error on empty data")
	}
}

func TestClient_ListModels_ParsesIDs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[{"id":"m1","display_name":"One"},{"id":"m2"}]}`))
	}))
	defer srv.Close()
	c, err := NewClient("k", srv.URL, srv.URL+"/v1/models", srv.Client(), nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[0].ID != "m1" || ids[1].ID != "m2" {
		t.Fatalf("ids = %+v", ids)
	}
}

func TestClient_ListModels_RetryOn429(t *testing.T) {
	var attempts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[{"id":"ok"}]}`))
	}))
	defer srv.Close()
	c, err := NewClient("k", srv.URL, srv.URL+"/v1/models", srv.Client(), nil, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0].ID != "ok" {
		t.Fatalf("ids = %+v", ids)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
}

func TestClient_ListModels_CustomHeaders(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("X-Tenant-Id")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"data":[{"id":"ok"}]}`))
	}))
	defer srv.Close()
	c, err := NewClient("k", srv.URL, srv.URL+"/v1/models", srv.Client(), nil, map[string]string{"X-Tenant-Id": "tn"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListModels(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got != "tn" {
		t.Fatalf("X-Tenant-Id = %q, want tn", got)
	}
}
