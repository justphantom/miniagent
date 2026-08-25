package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/justphantom/miniagent/miniagent"
	"github.com/justphantom/miniagent/miniagent/session"
)

// seedRemoteSession 在 stub 上按服务端语义建会话（Create → Rewrite）。
func seedRemoteSession(t *testing.T, stubURL, id string, msgs []miniagent.Message) {
	t.Helper()
	c := session.NewClient(stubURL, "")
	meta := session.SessionMeta{Type: "session", ID: id, Model: "p/m", Provider: "p", Workdir: "/wd", Created: "2026-01-01T00:00:00Z"}
	if _, err := c.CreateSession(context.Background(), meta); err != nil {
		t.Fatalf("seed create %s: %v", id, err)
	}
	if err := c.RewriteMessages(context.Background(), id, meta, msgs); err != nil {
		t.Fatalf("seed rewrite %s: %v", id, err)
	}
}

// remoteWebServer 构造 session.url 指向 stub 的 web server。
func remoteWebServer(stubURL string) *webServer {
	s := testWebServer("")
	s.cfg.Session.URL = stubURL
	return s
}

func TestWebSessionsListRemote(t *testing.T) {
	stub := newRemoteStub(t)
	seedRemoteSession(t, stub.URL, "older", []miniagent.Message{{Role: miniagent.RoleUser, Content: "q1"}})
	seedRemoteSession(t, stub.URL, "newer", []miniagent.Message{{Role: miniagent.RoleUser, Content: "q2"}})
	s := remoteWebServer(stub.URL)

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/sessions", nil)
	rec := httptest.NewRecorder()
	s.mux().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list code = %d body=%s, want 200", rec.Code, rec.Body.String())
	}
	var out []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode summaries: %v (body=%s)", err, rec.Body.String())
	}
	if len(out) != 2 {
		t.Fatalf("summary count = %d, want 2 (body=%s)", len(out), rec.Body.String())
	}
	// 两次 seed 的 rev 单调递增 → newer 的 modified 更大，倒序排前。
	if out[0]["id"] != "newer" || out[1]["id"] != "older" {
		t.Errorf("order = %v then %v, want newer first", out[0]["id"], out[1]["id"])
	}
	first := out[0]
	if first["provider"] != "p" || first["model"] != "p/m" || first["created"] != "2026-01-01T00:00:00Z" {
		t.Errorf("mapped meta fields = %+v", first)
	}
	if first["preview"] != "q2" {
		t.Errorf("preview = %v, want q2", first["preview"])
	}
	if _, has := first["workdir"]; !has {
		t.Errorf("workdir key missing: frontend contract requires the field (empty is the accepted degradation)")
	}
	if first["running"] != false {
		t.Errorf("running = %v, want false", first["running"])
	}
}

// 服务不可达 → 500：配置错误不得被空列表掩盖（与本地 ReadDir 失败同语义）。
func TestWebSessionsListRemote_Unreachable(t *testing.T) {
	s := remoteWebServer("http://127.0.0.1:1")
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/sessions", nil)
	rec := httptest.NewRecorder()
	s.mux().ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("unreachable list code = %d, want 500", rec.Code)
	}
}

func TestWebSessionReplayRemote(t *testing.T) {
	stub := newRemoteStub(t)
	seedRemoteSession(t, stub.URL, "rp-1", []miniagent.Message{
		{Role: miniagent.RoleUser, Content: "remote q"},
		{Role: miniagent.RoleAssistant, Content: "remote a"},
	})
	s := remoteWebServer(stub.URL)

	get := func(id string) *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/api/sessions/"+id, nil)
		rec := httptest.NewRecorder()
		s.mux().ServeHTTP(rec, req)
		return rec
	}
	rec := get("rp-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("replay code = %d body=%s, want 200", rec.Code, rec.Body.String())
	}
	ev := parseNDJSON(t, rec.Body.String())
	if !equalSlice(eventTypes(ev), []string{"session", "user_prompt", "text_delta", "result"}) {
		t.Fatalf("replay event types = %v (body=%s)", eventTypes(ev), rec.Body.String())
	}
	if ev[0]["id"] != "rp-1" || ev[0]["workdir"] != "/wd" {
		t.Errorf("session event = %+v (workdir 来自 detail meta，前端回填依赖它)", ev[0])
	}
	if ev[1]["text"] != "remote q" || ev[2]["text"] != "remote a" {
		t.Errorf("prompt/text = %+v / %+v", ev[1], ev[2])
	}
	// 404 映射：远端 404 → os.ErrNotExist → 404。
	if rec := get("missing-1"); rec.Code != http.StatusNotFound {
		t.Errorf("missing replay code = %d, want 404", rec.Code)
	}
	// 非法 id：与本地分支同走白名单校验 → 400。
	if rec := get("..evil"); rec.Code != http.StatusBadRequest {
		t.Errorf("bad id replay code = %d, want 400", rec.Code)
	}
}

func TestWebSessionDeleteRemote(t *testing.T) {
	stub := newRemoteStub(t)
	seedRemoteSession(t, stub.URL, "del-r", []miniagent.Message{{Role: miniagent.RoleUser, Content: "hi"}})
	s := remoteWebServer(stub.URL)

	del := func(id string) *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodDelete, "/api/sessions/"+id, nil)
		rec := httptest.NewRecorder()
		s.mux().ServeHTTP(rec, req)
		return rec
	}
	if rec := del("del-r"); rec.Code != http.StatusNoContent {
		t.Fatalf("delete code = %d, want 204", rec.Code)
	}
	if _, ok := s.turns.running("del-r"); ok {
		t.Error("registry entry still present after remote delete")
	}
	if rec := del("del-r"); rec.Code != http.StatusNotFound {
		t.Errorf("delete missing code = %d, want 404", rec.Code)
	}
	// 在途守卫同样覆盖远端分支：本地 writer 的下一次 Rewrite 不得复活已删会话。
	seedRemoteSession(t, stub.URL, "busy-r", []miniagent.Message{{Role: miniagent.RoleUser, Content: "hi"}})
	if _, busy := s.turns.register("busy-r", func() {}, nil); busy {
		t.Fatal("register unexpectedly busy on a fresh registry")
	}
	defer s.turns.finish("busy-r", nil)
	if rec := del("busy-r"); rec.Code != http.StatusConflict {
		t.Errorf("in-flight delete code = %d, want 409", rec.Code)
	}
}
