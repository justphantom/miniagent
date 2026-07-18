package miniagent

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"log/slog"
)

// 历史裁剪的 token 预算：按 4k 上下文模型的 60% 预留给历史，剩余留给系统
// 提示、用户本轮输入与输出。搭配 estimateTokens 的保守估算，避免实际请求
// 撞到模型上下文窗口上限。
const maxHistoryTokens = 6000

// History persists per-chat conversation as jsonl session files.
// mu 保护 Append 与 writeCur 之间的复合操作，避免同进程内多 goroutine
// 并发追加同一 chatID 时 bufio 多次 write 交错产生畸形行。
// 注意：跨进程并发仍需调用方用文件锁或调度保证，mutex 无法跨进程。
type History struct {
	dir    string
	logger *slog.Logger
	mu     sync.Mutex
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
	path := h.resolve(chatID)
	if path == "" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer func() { _ = f.Close() }()
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
	h.mu.Lock()
	defer h.mu.Unlock()
	path := h.resolve(chatID)
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
	defer func() { _ = f.Close() }()
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

// estimateTokens 估算消息列表的 token 数。用 len/3 而非更常见的 len/4：
// 中文字符 UTF-8 占 3 字节，按 1-2 token/字 计费，/3 是中英混排场景下偏保守
// 的下界。最后整体放大 1.2（safetyMargin），让裁剪更早触发，降低实际请求
// 超出模型上下文窗口的风险。
func estimateTokens(msgs []Message) int {
	const perMessageOverhead = 4
	const safetyMargin = 6 / 5 // 1.2
	total := 0
	for i := range msgs {
		total += perMessageOverhead
		total += (len(msgs[i].Content) + 2) / 3
		for _, tc := range msgs[i].ToolCalls {
			total += (len(tc.Args) + 2) / 3
		}
		total += (len(msgs[i].ToolCallID) + 2) / 3
	}
	return total * safetyMargin
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
