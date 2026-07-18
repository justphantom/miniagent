package miniagent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"log/slog"
)

// Fact is one structured long-term memory entry.
type Fact struct {
	Key       string    `json:"key"`
	Value     string    `json:"value"`
	Source    string    `json:"source,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// FactScope controls which conversations can see a fact.
type FactScope string

const (
	ScopeChat    FactScope = "chat"
	ScopeProject FactScope = "project"
	ScopeGlobal  FactScope = "global"
)

// ParseFactScope normalizes a scope string. Unknown values fall back to chat.
func ParseFactScope(s string) FactScope {
	switch FactScope(s) {
	case ScopeGlobal, ScopeProject, ScopeChat:
		return FactScope(s)
	default:
		return ScopeChat
	}
}

// FactStore persists and retrieves facts by scope.
type FactStore struct {
	dir    string
	logger *slog.Logger
	mu     sync.RWMutex
}

// NewFactStore builds a FactStore rooted at {stateDir}/miniagent/memory.
func NewFactStore(stateDir string, logger *slog.Logger) *FactStore {
	return &FactStore{
		dir:    filepath.Join(stateDir, "miniagent", "memory"),
		logger: logger,
	}
}

func (s *FactStore) scopedFile(scope FactScope, chatID string) string {
	if scope == ScopeGlobal {
		return filepath.Join(s.dir, "global.json")
	}
	if scope == ScopeProject {
		return filepath.Join(s.dir, "project.json")
	}
	return filepath.Join(s.dir, sanitizeChatID(chatID)+".json")
}

func (s *FactStore) load(scope FactScope, chatID string) (map[string]Fact, error) {
	path := s.scopedFile(scope, chatID)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]Fact{}, nil
		}
		return nil, err
	}
	out := map[string]Fact{}
	if len(data) == 0 {
		return out, nil
	}
	if err := json.Unmarshal(data, &out); err != nil {
		if s.logger != nil {
			s.logger.Warn("corrupt fact file, resetting", "path", path, "error", err)
		}
		return map[string]Fact{}, nil
	}
	return out, nil
}

func (s *FactStore) save(scope FactScope, chatID string, facts map[string]Fact) error {
	path := s.scopedFile(scope, chatID)
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.dir, ".memory-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(facts); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return err
	}
	return nil
}

// Get returns one fact. ok is false when the key is absent or memory is off.
func (s *FactStore) Get(scope FactScope, chatID, key string) (Fact, bool, error) {
	if s == nil {
		return Fact{}, false, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	facts, err := s.load(scope, chatID)
	if err != nil {
		return Fact{}, false, err
	}
	f, ok := facts[key]
	return f, ok, nil
}

// Set writes or overwrites a fact.
func (s *FactStore) Set(scope FactScope, chatID, key, value, source string) error {
	if s == nil {
		return nil
	}
	if key == "" {
		return fmt.Errorf("memory key cannot be empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	facts, err := s.load(scope, chatID)
	if err != nil {
		return err
	}
	facts[key] = Fact{
		Key:       key,
		Value:     value,
		Source:    source,
		UpdatedAt: time.Now(),
	}
	return s.save(scope, chatID, facts)
}

// List returns all facts matching the optional key prefix, sorted by key.
func (s *FactStore) List(scope FactScope, chatID, prefix string) ([]Fact, error) {
	if s == nil {
		return nil, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	facts, err := s.load(scope, chatID)
	if err != nil {
		return nil, err
	}
	out := make([]Fact, 0, len(facts))
	for _, f := range facts {
		if prefix != "" && !strings.HasPrefix(f.Key, prefix) {
			continue
		}
		out = append(out, f)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out, nil
}

// Delete removes one fact. Deleting a missing key is not an error.
func (s *FactStore) Delete(scope FactScope, chatID, key string) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	facts, err := s.load(scope, chatID)
	if err != nil {
		return err
	}
	if _, ok := facts[key]; !ok {
		return nil
	}
	delete(facts, key)
	return s.save(scope, chatID, facts)
}

// formatFacts renders a slice of facts as a short bullet list for the system prompt.
func formatFacts(facts []Fact) string {
	if len(facts) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString("\n\n以下是与当前对话相关的已知事实（由用户或之前的对话沉淀）：\n")
	for _, f := range facts {
		fmt.Fprintf(&sb, "- %s: %s\n", f.Key, f.Value)
	}
	return sb.String()
}
