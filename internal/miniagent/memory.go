package miniagent

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"log/slog"
)

const maxHistoryTokens = 6000

// History persists per-chat conversation as jsonl session files.
type History struct {
	dir    string
	logger *slog.Logger
}

// NewHistory builds a History rooted at {stateDir}/miniagent/history.
func NewHistory(stateDir string, logger *slog.Logger) (*History, error) {
	if strings.TrimSpace(stateDir) == "" {
		return nil, fmt.Errorf("miniagent: stateDir is empty")
	}
	return &History{dir: filepath.Join(stateDir, "miniagent", "history"), logger: logger}, nil
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
func (h *History) Append(chatID string, msgs []Message) error {
	if h == nil {
		return nil
	}
	if chatID == "" {
		return fmt.Errorf("history: chatID is empty")
	}
	if len(msgs) == 0 {
		return nil
	}
	_, path := h.resolve(chatID)
	if path == "" {
		sid := newSessionID(now())
		if err := h.writeCur(chatID, sid); err != nil {
			return fmt.Errorf("history: session pointer failed: %w", err)
		}
		path = h.sessionPath(chatID, sid)
	}
	if err := os.MkdirAll(h.dir, 0o755); err != nil {
		return fmt.Errorf("history: mkdir failed: %w", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("history: open failed: %w", err)
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
		if _, err := w.Write(b); err != nil {
			return fmt.Errorf("history: write failed: %w", err)
		}
		if err := w.WriteByte('\n'); err != nil {
			return fmt.Errorf("history: write failed: %w", err)
		}
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("history: flush failed: %w", err)
	}
	return nil
}

func (h *History) trim(msgs []Message) []Message {
	return trimMessages(msgs, maxHistoryTokens)
}

// trimMessages drops old turns until the estimated token budget is met.
func trimMessages(msgs []Message, budget int) []Message {
	if len(msgs) == 0 {
		return msgs
	}
	for estimateTokens(msgs) > budget && hasMultipleTurns(msgs) {
		msgs = dropFirstTurn(msgs)
	}
	if estimateTokens(msgs) > budget {
		msgs = truncateLastContents(msgs, budget)
	}
	return msgs
}

func truncateLastContents(msgs []Message, budget int) []Message {
	for estimateTokens(msgs) > budget {
		truncated := false
		for i := len(msgs) - 1; i >= 0; i-- {
			if msgs[i].Role == "user" {
				continue
			}
			runes := []rune(msgs[i].Content)
			if len(runes) <= 50 {
				continue
			}
			msgs[i].Content = string(runes[:len(runes)*3/4]) + "…"
			truncated = true
			break
		}
		if !truncated {
			break
		}
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
