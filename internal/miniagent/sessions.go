package miniagent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// SessionInfo describes one stored session of a chat.
type SessionInfo struct {
	ID      string
	Bytes   int64
	ModTime time.Time
	Current bool
}

func newSessionID(now time.Time) string {
	return now.Format("20060102-150405")
}

func now() time.Time { return time.Now() }

// resolve 返回当前 session 的 jsonl 路径；无 session 时返回 ""。
func (h *History) resolve(chatID string) string {
	sid := h.current(chatID)
	if sid == "" {
		return ""
	}
	return h.sessionPath(chatID, sid)
}

// Current returns the active session id, or "" when none or history disabled.
func (h *History) Current(chatID string) string {
	if h == nil {
		return ""
	}
	return h.current(chatID)
}

// NewSession points the chat at a fresh empty session.
func (h *History) NewSession(chatID string) (string, error) {
	if h == nil {
		return "", errors.New("miniagent: history disabled")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	ts := now()
	sid := newSessionID(ts)
	if sid == h.current(chatID) {
		sid = fmt.Sprintf("%s-%d", sid, ts.Nanosecond())
	}
	if err := os.MkdirAll(h.dir, 0o750); err != nil {
		return "", err
	}
	f, err := os.OpenFile(h.sessionPath(chatID, sid), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	_ = f.Close()
	if err := h.writeCur(chatID, sid); err != nil {
		return "", err
	}
	return sid, nil
}

// ListSessions enumerates the chat's session files, oldest first.
func (h *History) ListSessions(chatID string) ([]SessionInfo, error) {
	if h == nil {
		return nil, errors.New("miniagent: history disabled")
	}
	if chatID == "" {
		return nil, errors.New("miniagent: chatID is empty")
	}
	entries, err := os.ReadDir(h.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	prefix := sanitizeChatID(chatID) + "__"
	cur := h.current(chatID)
	var out []SessionInfo
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		st, err := e.Info()
		if err != nil {
			continue
		}
		id := strings.TrimSuffix(strings.TrimPrefix(name, prefix), ".jsonl")
		out = append(out, SessionInfo{ID: id, Bytes: st.Size(), ModTime: st.ModTime(), Current: id == cur})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ModTime.Before(out[j].ModTime) })
	return out, nil
}

// UseSession switches the chat back to a stored session.
func (h *History) UseSession(chatID, sid string) error {
	if h == nil {
		return errors.New("miniagent: history disabled")
	}
	if !validSessionID(sid) {
		return fmt.Errorf("miniagent: invalid session id %q", sid)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.sessionExists(chatID, sid) {
		return fmt.Errorf("miniagent: session %s not found", sid)
	}
	return h.writeCur(chatID, sid)
}

// DeleteSession removes a session file.
func (h *History) DeleteSession(chatID, sid string) error {
	if h == nil {
		return errors.New("miniagent: history disabled")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !validSessionID(sid) {
		return fmt.Errorf("miniagent: invalid session id %q", sid)
	}
	if err := os.Remove(h.sessionPath(chatID, sid)); err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("miniagent: session %s not found", sid)
		}
		return err
	}
	if h.current(chatID) == sid {
		_ = os.Remove(h.curPathFor(chatID))
	}
	return nil
}

func validSessionID(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if r != '-' && (r < '0' || r > '9') && (r < 'a' || r > 'z') {
			return false
		}
	}
	return true
}

func (h *History) sessionExists(chatID, sid string) bool {
	_, err := os.Stat(h.sessionPath(chatID, sid))
	return err == nil
}

func (h *History) current(chatID string) string {
	b, err := os.ReadFile(h.curPathFor(chatID))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func (h *History) writeCur(chatID, sid string) error {
	if err := os.MkdirAll(h.dir, 0o750); err != nil {
		return err
	}
	return writeFileAtomic(h.curPathFor(chatID), []byte(sid), 0o600)
}

func (h *History) sessionPath(chatID, sid string) string {
	return filepath.Join(h.dir, sanitizeChatID(chatID)+"__"+sid+".jsonl")
}

func (h *History) curPathFor(chatID string) string {
	return filepath.Join(h.dir, sanitizeChatID(chatID)+".cur")
}

func sanitizeChatID(s string) string {
	if s == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '.', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "x"
	}
	return b.String()
}
