package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/justphantom/miniagent/miniagent"
)

// remoteStub 用内存 map 模拟 minisession 语义（Create 幂等、Rewrite 404 需先建、
// Append/Load/Delete 常规），使 cmd 层分支测试无需真实服务二进制。
func newRemoteStub(t *testing.T) *httptest.Server {
	t.Helper()
	type entry struct {
		Meta sessionMetaJSON
		Msgs []miniagent.Message
	}
	var mu sync.Mutex
	sessions := map[string]*entry{}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/sessions", func(w http.ResponseWriter, r *http.Request) {
		var req sessionMetaJSON
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeStubError(w, http.StatusBadRequest, err.Error())
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if _, ok := sessions[req.ID]; ok {
			writeStubError(w, http.StatusConflict, "session already exists")
			return
		}
		if req.Type == "" {
			req.Type = "session"
		}
		sessions[req.ID] = &entry{Meta: req}
		writeStubJSON(w, http.StatusCreated, req)
	})
	mux.HandleFunc("GET /api/sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		mu.Lock()
		defer mu.Unlock()
		e, ok := sessions[id]
		if !ok {
			writeStubError(w, http.StatusNotFound, "session not found")
			return
		}
		writeStubJSON(w, http.StatusOK, struct {
			sessionMetaJSON
			MessageCount int `json:"message_count"`
		}{e.Meta, len(e.Msgs)})
	})
	mux.HandleFunc("DELETE /api/sessions/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		mu.Lock()
		defer mu.Unlock()
		if _, ok := sessions[id]; !ok {
			writeStubError(w, http.StatusNotFound, "session not found")
			return
		}
		delete(sessions, id)
		writeStubJSON(w, http.StatusOK, map[string]bool{"deleted": true})
	})
	mux.HandleFunc("GET /api/sessions/{id}/messages", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		mu.Lock()
		defer mu.Unlock()
		e, ok := sessions[id]
		if !ok {
			writeStubError(w, http.StatusNotFound, "session not found")
			return
		}
		writeStubJSON(w, http.StatusOK, e.Msgs)
	})
	mux.HandleFunc("POST /api/sessions/{id}/messages", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		var req struct {
			Messages []miniagent.Message `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil { //nolint:musttag // 匿名 struct 元素有 tag，误报
			writeStubError(w, http.StatusBadRequest, err.Error())
			return
		}
		mu.Lock()
		defer mu.Unlock()
		e, ok := sessions[id]
		if !ok {
			writeStubError(w, http.StatusNotFound, "session not found")
			return
		}
		e.Msgs = append(e.Msgs, req.Messages...)
		writeStubJSON(w, http.StatusOK, map[string]int{"appended": len(req.Messages)})
	})
	mux.HandleFunc("PUT /api/sessions/{id}/messages", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		var req struct {
			Meta     sessionMetaJSON     `json:"meta"`
			Messages []miniagent.Message `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil { //nolint:musttag // 匿名 struct 元素有 tag，误报
			writeStubError(w, http.StatusBadRequest, err.Error())
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if _, ok := sessions[id]; !ok {
			writeStubError(w, http.StatusNotFound, "session not found")
			return
		}
		sessions[id].Meta = req.Meta
		sessions[id].Msgs = req.Messages
		writeStubJSON(w, http.StatusOK, map[string]bool{"rewritten": true})
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

type sessionMetaJSON struct {
	Type        string `json:"type"`
	ID          string `json:"id"`
	Model       string `json:"model"`
	Workdir     string `json:"workdir"`
	Provider    string `json:"provider"`
	Created     string `json:"created"`
	LLMRequests int    `json:"llm_requests,omitempty"`
}

func writeStubJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeStubError(w http.ResponseWriter, status int, msg string) {
	writeStubJSON(w, status, map[string]string{"error": msg})
}
