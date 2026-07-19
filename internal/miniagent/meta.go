package miniagent

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// MetaStore persists per-chat metadata (model, directory, permission)
// under {stateDir}/miniagent/meta, separate from conversation history.
type MetaStore struct {
	dir string
}

// NewMetaStore builds a MetaStore rooted at {stateDir}/miniagent/meta.
func NewMetaStore(stateDir string) (*MetaStore, error) {
	if strings.TrimSpace(stateDir) == "" {
		return nil, errors.New("miniagent: stateDir is empty")
	}
	return &MetaStore{dir: filepath.Join(stateDir, "miniagent", "meta")}, nil
}

// Model returns the per-chat pinned model id, or "" when none is set.
func (m *MetaStore) Model(chatID string) string {
	return m.read(chatID, "model")
}

// SetModel stores the per-chat pinned model id.
func (m *MetaStore) SetModel(chatID, value string) error {
	return m.write(chatID, "model", value)
}

// Directory returns the per-chat pinned working directory, or "" when none is set.
func (m *MetaStore) Directory(chatID string) string {
	return m.read(chatID, "dir")
}

// SetDirectory stores the per-chat pinned working directory.
func (m *MetaStore) SetDirectory(chatID, value string) error {
	return m.write(chatID, "dir", value)
}

// Permission returns the per-chat pinned permission mode, or "" when none is set.
func (m *MetaStore) Permission(chatID string) string {
	return m.read(chatID, "perm")
}

// SetPermission stores the per-chat pinned permission mode.
func (m *MetaStore) SetPermission(chatID, value string) error {
	return m.write(chatID, "perm", value)
}

func (m *MetaStore) path(chatID, name string) string {
	return filepath.Join(m.dir, sanitizeChatID(chatID)+"."+name)
}

func (m *MetaStore) read(chatID, name string) string {
	if m == nil {
		return ""
	}
	b, err := os.ReadFile(m.path(chatID, name))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func (m *MetaStore) write(chatID, name, value string) error {
	if m == nil {
		return nil
	}
	if err := os.MkdirAll(m.dir, 0o750); err != nil {
		return err
	}
	path := m.path(chatID, name)
	tmp, err := os.CreateTemp(m.dir, ".meta-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.WriteString(value); err != nil {
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
