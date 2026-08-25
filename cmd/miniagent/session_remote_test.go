package main

import (
	"testing"
	"time"

	"github.com/justphantom/miniagent/miniagent"
	"github.com/justphantom/miniagent/miniagent/session"
)

// resolveSessionRemote 与本地 resolveSession 行为对齐：新建构造 meta、resume 加载历史、
// resume 不存在报 os.ErrNotExist（供 web 层映射 404）。
func TestResolveSessionRemote(t *testing.T) {
	ts := newRemoteStub(t)
	c := session.NewClient(ts.URL, "")
	ctx := t.Context()

	// 新建：构造 meta（无历史）
	meta, history, err := resolveSessionRemote(ctx, c, true, "", "", "p/m", "p", "/wd")
	if err != nil {
		t.Fatalf("saveNew: %v", err)
	}
	if meta.ID == "" || meta.Model != "p/m" || meta.Provider != "p" {
		t.Fatalf("saveNew meta = %+v", meta)
	}
	if len(history) != 0 {
		t.Fatalf("saveNew history = %d msgs, want 0", len(history))
	}

	// resume 不存在：os.ErrNotExist
	_, _, err = resolveSessionRemote(ctx, c, false, "", "no-such", "p/m", "p", "/wd")
	if err == nil {
		t.Fatal("resume missing: want error")
	}

	// 建立远端会话后 resume：返回服务端 meta + 历史
	created, err := c.CreateSession(ctx, session.SessionMeta{ID: "e2e-resume", Model: "m"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := c.AppendMessages(ctx, created.ID, []miniagent.Message{{Role: "user", Content: "hi"}}); err != nil {
		t.Fatalf("append: %v", err)
	}
	meta, history, err = resolveSessionRemote(ctx, c, false, "", "e2e-resume", "p/m", "p", "/wd")
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if meta.ID != "e2e-resume" {
		t.Fatalf("resume meta.ID = %q", meta.ID)
	}
	if len(history) != 1 || history[0].Content != "hi" {
		t.Fatalf("resume history = %+v", history)
	}
}

// saveSessionRemote：已存在会话直接 Rewrite；不存在先 Create 再 Rewrite。
func TestSaveSessionRemote(t *testing.T) {
	ts := newRemoteStub(t)
	c := session.NewClient(ts.URL, "")
	ctx := t.Context()

	// 不存在：先建后写
	meta := session.SessionMeta{ID: "save-new", Model: "m", Provider: "p", Created: time.Now().Format(time.RFC3339)}
	msgs := []miniagent.Message{{Role: "user", Content: "first"}}
	if err := saveSessionRemote(ctx, c, meta, msgs); err != nil {
		t.Fatalf("save new: %v", err)
	}
	_, got, err := c.LoadSession(ctx, "save-new")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got) != 1 || got[0].Content != "first" {
		t.Fatalf("got = %+v", got)
	}

	// 已存在：直接 Rewrite 覆盖
	msgs2 := []miniagent.Message{{Role: "user", Content: "second"}}
	if err := saveSessionRemote(ctx, c, meta, msgs2); err != nil {
		t.Fatalf("save existing: %v", err)
	}
	_, got, err = c.LoadSession(ctx, "save-new")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got) != 1 || got[0].Content != "second" {
		t.Fatalf("after rewrite got = %+v", got)
	}
}
