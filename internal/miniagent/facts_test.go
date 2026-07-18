package miniagent

import (
	"testing"
)

func TestFactStore_SetGet(t *testing.T) {
	s, _ := NewFactStore(t.TempDir(), nil)
	if err := s.Set(ScopeChat, "chat1", "user.lang", "zh", "test"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	f, ok, err := s.Get(ScopeChat, "chat1", "user.lang")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !ok || f.Value != "zh" {
		t.Errorf("fact = %+v, ok=%v", f, ok)
	}
}

func TestFactStore_GetMissing(t *testing.T) {
	s, _ := NewFactStore(t.TempDir(), nil)
	_, ok, err := s.Get(ScopeChat, "chat1", "missing")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if ok {
		t.Error("expected not found")
	}
}

func TestFactStore_List(t *testing.T) {
	s, _ := NewFactStore(t.TempDir(), nil)
	s.Set(ScopeChat, "chat1", "user.lang", "zh", "")
	s.Set(ScopeChat, "chat1", "user.name", "a", "")
	s.Set(ScopeChat, "chat1", "project.x", "y", "")
	facts, err := s.List(ScopeChat, "chat1", "user.")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(facts) != 2 {
		t.Errorf("facts = %d", len(facts))
	}
	if facts[0].Key > facts[1].Key {
		t.Error("not sorted")
	}
}

func TestFactStore_Delete(t *testing.T) {
	s, _ := NewFactStore(t.TempDir(), nil)
	s.Set(ScopeChat, "chat1", "k", "v", "")
	if err := s.Delete(ScopeChat, "chat1", "k"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	_, ok, _ := s.Get(ScopeChat, "chat1", "k")
	if ok {
		t.Error("expected deleted")
	}
}

func TestFactStore_ScopesIsolated(t *testing.T) {
	dir := t.TempDir()
	s, _ := NewFactStore(dir, nil)
	s.Set(ScopeChat, "chat1", "k", "chat-v", "")
	s.Set(ScopeGlobal, "chat1", "k", "global-v", "")
	f, _, _ := s.Get(ScopeChat, "chat1", "k")
	if f.Value != "chat-v" {
		t.Errorf("chat value = %q", f.Value)
	}
	g, _, _ := s.Get(ScopeGlobal, "chat1", "k")
	if g.Value != "global-v" {
		t.Errorf("global value = %q", g.Value)
	}
}

func TestParseFactScope(t *testing.T) {
	if ParseFactScope("global") != ScopeGlobal {
		t.Error("global mismatch")
	}
	if ParseFactScope("unknown") != ScopeChat {
		t.Error("unknown should default to chat")
	}
}
