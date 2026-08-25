package session

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/justphantom/miniagent/miniagent"
)

// startStubMinisession 启动一个覆盖 Client 全部端点的桩服务。
func startStubMinisession(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	sessions := map[string]SessionMeta{}
	msgs := map[string][]miniagent.Message{}

	mux.HandleFunc("GET /api/sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		meta, ok := sessions[id]
		if !ok {
			http.Error(w, `{"error":"session not found"}`, http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(meta)
	})
	mux.HandleFunc("GET /api/sessions/{id}/messages", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		list, ok := msgs[id]
		if !ok {
			http.Error(w, `{"error":"session not found"}`, http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(list) //nolint:musttag // []Message 元素均有 json tag，musttag 对内联切片误报
	})
	mux.HandleFunc("POST /api/sessions", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID       string `json:"id"`
			Model    string `json:"model"`
			Provider string `json:"provider"`
			Workdir  string `json:"workdir"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
			return
		}
		meta := SessionMeta{Type: "session", ID: req.ID, Model: req.Model, Provider: req.Provider, Workdir: req.Workdir, Created: "2026-08-25T00:00:00Z"}
		sessions[req.ID] = meta
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(meta)
	})
	mux.HandleFunc("POST /api/sessions/{id}/messages", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if _, ok := sessions[id]; !ok {
			http.Error(w, `{"error":"session not found"}`, http.StatusNotFound)
			return
		}
		var req struct {
			Messages []miniagent.Message `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil { //nolint:musttag // 匿名 struct 元素有 tag，误报
			http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
			return
		}
		msgs[id] = append(msgs[id], req.Messages...)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"appended":1}`))
	})
	mux.HandleFunc("PUT /api/sessions/{id}/messages", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if _, ok := sessions[id]; !ok {
			http.Error(w, `{"error":"session not found"}`, http.StatusNotFound)
			return
		}
		var req struct {
			Meta     SessionMeta         `json:"meta"`
			Messages []miniagent.Message `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil { //nolint:musttag // 匿名 struct 元素有 tag，误报
			http.Error(w, `{"error":"bad request"}`, http.StatusBadRequest)
			return
		}
		sessions[id] = req.Meta
		msgs[id] = req.Messages
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"rewritten":true}`))
	})
	mux.HandleFunc("DELETE /api/sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if _, ok := sessions[id]; !ok {
			http.Error(w, `{"error":"session not found"}`, http.StatusNotFound)
			return
		}
		delete(sessions, id)
		delete(msgs, id)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"deleted":true}`))
	})

	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func TestClientFullFlow(t *testing.T) {
	ts := startStubMinisession(t)
	c := NewClient(ts.URL, "")
	ctx := context.Background()

	// 创建
	meta, err := c.CreateSession(ctx, SessionMeta{ID: "client-e2e", Model: "gpt-4o"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if meta.ID != "client-e2e" || meta.Model != "gpt-4o" {
		t.Errorf("meta = %+v", meta)
	}

	// 追加
	if err := c.AppendMessages(ctx, "client-e2e", []miniagent.Message{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatalf("AppendMessages: %v", err)
	}

	// 读取
	_, got, err := c.LoadSession(ctx, "client-e2e")
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if len(got) != 1 || got[0].Content != "hi" {
		t.Errorf("messages = %+v", got)
	}

	// 覆写
	if err := c.RewriteMessages(ctx, "client-e2e", SessionMeta{Type: "session", ID: "client-e2e", Model: "gpt-4o"}, []miniagent.Message{{Role: "user", Content: "rewritten"}}); err != nil {
		t.Fatalf("RewriteMessages: %v", err)
	}
	_, got, err = c.LoadSession(ctx, "client-e2e")
	if err != nil {
		t.Fatalf("LoadSession after rewrite: %v", err)
	}
	if len(got) != 1 || got[0].Content != "rewritten" {
		t.Errorf("messages after rewrite = %+v", got)
	}

	// 删除
	if err := c.DeleteSession(ctx, "client-e2e"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	_, _, err = c.LoadSession(ctx, "client-e2e")
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("LoadSession after delete = %v, want os.ErrNotExist", err)
	}
}

func TestClientInvalidID(t *testing.T) {
	ts := startStubMinisession(t)
	c := NewClient(ts.URL, "")
	ctx := context.Background()
	if err := c.AppendMessages(ctx, "../evil", nil); err == nil {
		t.Errorf("expected error for path traversal id")
	}
	if _, _, err := c.LoadSession(ctx, "bad/id"); err == nil {
		t.Errorf("expected error for invalid id")
	}
}

func TestClientServerDown(t *testing.T) {
	c := NewClient("http://127.0.0.1:0", "")
	ctx := context.Background()
	if _, err := c.CreateSession(ctx, SessionMeta{ID: "x"}); err == nil {
		t.Errorf("expected error when server unreachable")
	}
}
