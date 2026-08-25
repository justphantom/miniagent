package main

import (
	"bytes"
	"context"
	"log/slog"
	"testing"

	"github.com/justphantom/miniagent/config"
	"github.com/justphantom/miniagent/miniagent"
	"github.com/justphantom/miniagent/miniagent/session"
)

// TestRunTurnRemoteSessionPersistence: session.url 非空时 runTurn 走远端分支——
// 新会话经 saveSessionRemote 首次落库（Rewrite 404→Create→Rewrite），消息与 meta 正确。
func TestRunTurnRemoteSessionPersistence(t *testing.T) {
	stub := newRemoteStub(t)
	fake := &fakeLLM{}
	cfg := &config.Config{
		Providers: []config.ProviderConfig{{Name: "p", ChatURL: "http://127.0.0.1:1/v1/chat/completions", Key: "sk", Models: []config.ModelConfig{{Name: "m"}}}},
		Defaults:  config.DefaultsConfig{Provider: "p", Model: "m"},
		Session:   config.SessionConfig{URL: stub.URL},
	}
	engine := &turnEngine{
		cfg:           cfg,
		logger:        testLogger(),
		protectSignal: false,
		buildClients: func(resolved *config.Resolved, apiKey string, logger *slog.Logger, cache *transportCache) (miniagent.LLM, miniagent.Doer, error) {
			return fake, nil, nil
		},
	}
	spec := turnSpec{saveNew: true, sessionID: "remote-e2e-1", workdir: t.TempDir()}
	var out bytes.Buffer
	if err := engine.runTurn(context.Background(), spec, &out); err != nil {
		t.Fatalf("runTurn: %v", err)
	}

	c := newRemoteClientForTest(t, stub.URL)
	meta, msgs, err := c.LoadSession(context.Background(), "remote-e2e-1")
	if err != nil {
		t.Fatalf("load remote session: %v", err)
	}
	if meta.ID != "remote-e2e-1" || meta.Provider != "p" || meta.Model != "p/m" {
		t.Errorf("meta = %+v", meta)
	}
	if len(msgs) == 0 {
		t.Fatalf("remote session has no messages")
	}
	if msgs[0].Role != miniagent.RoleUser {
		t.Errorf("first msg role = %q", msgs[0].Role)
	}
	last := msgs[len(msgs)-1]
	if last.Role != miniagent.RoleAssistant || last.Content != "hello from fake" {
		t.Errorf("last msg = %+v", last)
	}
}

// TestRunTurnRemoteResumeSession: 远端接续——第二轮 runTurn 从远端加载历史，追加新消息。
func TestRunTurnRemoteResumeSession(t *testing.T) {
	stub := newRemoteStub(t)
	fake := &fakeLLM{}
	cfg := &config.Config{
		Providers: []config.ProviderConfig{{Name: "p", ChatURL: "http://127.0.0.1:1/v1/chat/completions", Key: "sk", Models: []config.ModelConfig{{Name: "m"}}}},
		Defaults:  config.DefaultsConfig{Provider: "p", Model: "m"},
		Session:   config.SessionConfig{URL: stub.URL},
	}
	newEngine := func() *turnEngine {
		return &turnEngine{
			cfg:           cfg,
			logger:        testLogger(),
			protectSignal: false,
			buildClients: func(resolved *config.Resolved, apiKey string, logger *slog.Logger, cache *transportCache) (miniagent.LLM, miniagent.Doer, error) {
				return fake, nil, nil
			},
		}
	}

	var out bytes.Buffer
	if err := newEngine().runTurn(context.Background(), turnSpec{saveNew: true, sessionID: "remote-resume-1", workdir: t.TempDir()}, &out); err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	out.Reset()
	if err := newEngine().runTurn(context.Background(), turnSpec{sessionArg: "remote-resume-1", workdir: t.TempDir()}, &out); err != nil {
		t.Fatalf("turn 2: %v", err)
	}

	c := newRemoteClientForTest(t, stub.URL)
	_, msgs, err := c.LoadSession(context.Background(), "remote-resume-1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	userCount := 0
	for _, m := range msgs {
		if m.Role == miniagent.RoleUser {
			userCount++
		}
	}
	if userCount != 2 {
		t.Errorf("user messages = %d, want 2 (resume must load remote history)", userCount)
	}
}

func newRemoteClientForTest(t *testing.T, url string) *session.Client {
	t.Helper()
	return session.NewClient(url, "")
}
