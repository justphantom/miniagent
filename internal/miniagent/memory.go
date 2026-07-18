package miniagent

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"

	"log/slog"
)

const maxHistoryTokens = 6000

// History persists per-chat conversation as jsonl session files.
type History struct {
	dir    string
	logger *slog.Logger
}

// NewHistory builds a History rooted at {stateDir}/miniagent/history.
func NewHistory(stateDir string, logger *slog.Logger) *History {
	return &History{dir: filepath.Join(stateDir, "miniagent", "history"), logger: logger}
}

// Load returns the stored conversation for chatID (trimmed to the token budget), or nil.
func (h *History) Load(chatID string) []Message {
	if h == nil {
		return nil
	}
	_, path := h.resolve(chatID)
	if path == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	var msgs []Message
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for sc.Scan() {
		var m Message
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			if h.logger != nil {
				h.logger.Debug("history: skip malformed line", "error", err)
			}
			continue
		}
		msgs = append(msgs, m)
	}
	if err := sc.Err(); err != nil && h.logger != nil {
		h.logger.Warn("history: read error", "error", err)
	}
	return h.trim(msgs)
}

// Append writes msgs as additional jsonl lines for chatID.
func (h *History) Append(chatID string, msgs []Message) {
	if h == nil || len(msgs) == 0 {
		return
	}
	_, path := h.resolve(chatID)
	if path == "" {
		sid := newSessionID(now())
		if err := h.writeCur(chatID, sid); err != nil {
			if h.logger != nil {
				h.logger.Warn("history: session pointer failed", "error", err)
			}
			return
		}
		path = h.sessionPath(chatID, sid)
	}
	if err := os.MkdirAll(h.dir, 0o755); err != nil {
		if h.logger != nil {
			h.logger.Warn("history: mkdir failed", "error", err)
		}
		return
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		if h.logger != nil {
			h.logger.Warn("history: open failed", "error", err)
		}
		return
	}
	defer f.Close()
	w := bufio.NewWriter(f)
	for _, m := range msgs {
		b, err := json.Marshal(m)
		if err != nil {
			if h.logger != nil {
				h.logger.Warn("history: skip unmarshalable message", "error", err)
			}
			continue
		}
		_, _ = w.Write(b)
		_ = w.WriteByte('\n')
	}
	if err := w.Flush(); err != nil && h.logger != nil {
		h.logger.Warn("history: flush failed", "error", err)
	}
}

func (h *History) trim(msgs []Message) []Message {
	if h == nil || len(msgs) == 0 {
		return msgs
	}
	for estimateTokens(msgs) > maxHistoryTokens && hasMultipleTurns(msgs) {
		msgs = dropFirstTurn(msgs)
	}
	return msgs
}

func estimateTokens(msgs []Message) int {
	const perMessageOverhead = 4
	total := 0
	for i := range msgs {
		total += perMessageOverhead
		total += (len(msgs[i].Content) + 3) / 4
		for _, tc := range msgs[i].ToolCalls {
			total += (len(tc.Args) + 3) / 4
		}
		total += (len(msgs[i].ToolCallID) + 3) / 4
	}
	return total
}

func hasMultipleTurns(msgs []Message) bool {
	users := 0
	for _, m := range msgs {
		if m.Role == "user" {
			users++
		}
	}
	return users > 1
}

func dropFirstTurn(msgs []Message) []Message {
	if len(msgs) == 0 {
		return msgs
	}
	i := 1
	for i < len(msgs) && msgs[i].Role != "user" {
		i++
	}
	return msgs[i:]
}
