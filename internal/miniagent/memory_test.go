package miniagent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHistory_AppendAndLoad(t *testing.T) {
	dir := t.TempDir()
	h, _ := NewHistory(dir, nil)
	h.Append("chat1", []Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "hello"},
	})
	loaded := h.Load("chat1")
	if len(loaded) != 2 {
		t.Fatalf("loaded = %d", len(loaded))
	}
	if loaded[0].Content != "hi" || loaded[1].Content != "hello" {
		t.Errorf("loaded = %+v", loaded)
	}
}

func TestHistory_Load_ReturnsNilForUnknownChat(t *testing.T) {
	h, _ := NewHistory(t.TempDir(), nil)
	if h.Load("unknown") != nil {
		t.Error("expected nil")
	}
}

func TestHistory_TrimDropsOldTurns(t *testing.T) {
	dir := t.TempDir()
	h, _ := NewHistory(dir, nil)
	big := strings.Repeat("a", 3000)
	for i := 0; i < 5; i++ {
		h.Append("chat1", []Message{{Role: "user", Content: big}, {Role: "assistant", Content: big}})
	}
	loaded := h.Load("chat1")
	users := 0
	for _, m := range loaded {
		if m.Role == "user" {
			users++
		}
	}
	if users > 3 {
		t.Errorf("users = %d, expected trimming", users)
	}
}

func TestHistory_KeepsToolPairingWhenTrimming(t *testing.T) {
	dir := t.TempDir()
	h, _ := NewHistory(dir, nil)
	h.Append("chat1", []Message{
		{Role: "user", Content: "q"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Name: "x", Args: "{}"}}},
		{Role: "tool", ToolCallID: "c1", Content: "r"},
		{Role: "assistant", Content: "a"},
	})
	loaded := h.Load("chat1")
	var toolCallID string
	var toolMsgID string
	for _, m := range loaded {
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			toolCallID = m.ToolCalls[0].ID
		}
		if m.Role == "tool" {
			toolMsgID = m.ToolCallID
		}
	}
	if toolCallID != toolMsgID {
		t.Errorf("pairing broken: %q vs %q", toolCallID, toolMsgID)
	}
}

func TestHistory_ListSessions(t *testing.T) {
	dir := t.TempDir()
	h, _ := NewHistory(dir, nil)
	h.Append("chat1", []Message{{Role: "user", Content: "x"}})
	sessions, err := h.ListSessions("chat1")
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sessions) != 1 {
		t.Fatalf("sessions = %d", len(sessions))
	}
	if !sessions[0].Current {
		t.Error("expected current")
	}
}

func TestHistory_UseSession(t *testing.T) {
	dir := t.TempDir()
	h, _ := NewHistory(dir, nil)
	h.Append("chat1", []Message{{Role: "user", Content: "x"}})
	sid, err := h.NewSession("chat1")
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if err := h.UseSession("chat1", sid); err != nil {
		t.Fatalf("UseSession: %v", err)
	}
	if h.Current("chat1") != sid {
		t.Errorf("current = %q", h.Current("chat1"))
	}
}

func TestHistory_DeleteSession(t *testing.T) {
	dir := t.TempDir()
	h, _ := NewHistory(dir, nil)
	h.Append("chat1", []Message{{Role: "user", Content: "x"}})
	sid := h.Current("chat1")
	if err := h.DeleteSession("chat1", sid); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if h.Current("chat1") != "" {
		t.Error("expected current cleared")
	}
}

func TestHistory_LegacyMigration(t *testing.T) {
	dir := t.TempDir()
	h, _ := NewHistory(dir, nil)
	legacy := filepath.Join(dir, "miniagent", "history", "chat1.jsonl")
	if err := mkdir(filepath.Dir(legacy)); err != nil {
		t.Fatal(err)
	}
	if err := writeFile(legacy, `{"role":"user","content":"legacy"}`+"\n"); err != nil {
		t.Fatal(err)
	}
	loaded := h.Load("chat1")
	if len(loaded) != 1 || loaded[0].Content != "legacy" {
		t.Errorf("loaded = %+v", loaded)
	}
}

func TestSanitizeChatID(t *testing.T) {
	if sanitizeChatID("a/b@c") != "a_b_c" {
		t.Errorf("sanitize = %q", sanitizeChatID("a/b@c"))
	}
	if sanitizeChatID("") != "" {
		t.Errorf("empty sanitize = %q", sanitizeChatID(""))
	}
}

func TestValidSessionID(t *testing.T) {
	if !validSessionID("20260101-120000") {
		t.Error("expected valid")
	}
	if validSessionID("../x") || validSessionID("") {
		t.Error("expected invalid")
	}
}

func TestNewSessionID(t *testing.T) {
	if newSessionID(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)) != "20260102-030405" {
		t.Errorf("sid = %q", newSessionID(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)))
	}
}

func mkdir(p string) error              { return os.MkdirAll(p, 0o755) }
func writeFile(p, content string) error { return os.WriteFile(p, []byte(content), 0o644) }
