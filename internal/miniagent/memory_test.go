package miniagent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// trim 触发时（多 turn + 大文本）必须保证：每个保留的 tool 消息都能在
// 保留段内找到配对的 assistant(tool_calls)。OpenAI 等后端要求 tool 消息
// 前必须有对应 tool_call，否则整批请求被拒。
func TestTrimMessages_PreservesToolPairingAcrossTurns(t *testing.T) {
	big := strings.Repeat("x", 3000)
	var msgs []Message
	// 5 turn，每 turn 都有 tool_call，确保触发多轮 drop。
	for i := 1; i <= 5; i++ {
		msgs = append(msgs,
			Message{Role: "user", Content: big},
			Message{Role: "assistant", ToolCalls: []ToolCall{{ID: fmt.Sprintf("c%d", i), Name: "t", Args: big}}},
			Message{Role: "tool", ToolCallID: fmt.Sprintf("c%d", i), Content: big},
			Message{Role: "assistant", Content: big},
		)
	}
	trimmed := trimMessages(msgs, maxHistoryTokens)
	if len(trimmed) >= len(msgs) {
		t.Skipf("trim did not trigger, len=%d", len(trimmed))
	}
	// 校验：每个 tool 消息的 ToolCallID 在 trimmed 段内能找到 assistant 配对。
	assistantCalls := map[string]bool{}
	for _, m := range trimmed {
		for _, tc := range m.ToolCalls {
			assistantCalls[tc.ID] = true
		}
	}
	for _, m := range trimmed {
		if m.Role == "tool" {
			if !assistantCalls[m.ToolCallID] {
				t.Errorf("orphan tool message: tool_call_id=%q has no preceding assistant tool_call in trimmed slice", m.ToolCallID)
			}
		}
	}
	// trimmed 开头不应是 tool 或带 ToolCallID 的消息（OpenAI 会拒）。
	if len(trimmed) > 0 && trimmed[0].Role == "tool" {
		t.Errorf("trimmed slice starts with orphan tool: %+v", trimmed[0])
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

// estimateTokens 对中文应给出比 len/4 更保守（更大）的估算。
func TestEstimateTokens_ChineseConservative(t *testing.T) {
	// 30 个中文字 = 90 字节 UTF-8。
	chinese := strings.Repeat("中", 30)
	msgs := []Message{{Role: "user", Content: chinese}}
	got := estimateTokens(msgs)
	// len/4 = 22；len/3 ≈ 30；×1.2 ≈ 36。应明显大于 len/4 估算。
	if got < 30 {
		t.Errorf("chinese estimate too low: got %d (len/4 would be %d)", got, (len(chinese)+3)/4)
	}
}

// 纯 ASCII 应保持合理量级（不爆炸放大）。
func TestEstimateTokens_ASCIIReasonable(t *testing.T) {
	ascii := strings.Repeat("a", 120)
	msgs := []Message{{Role: "user", Content: ascii}}
	got := estimateTokens(msgs)
	// len/3 = 40；×1.2 = 48。期望在 [40, 60] 区间。
	if got < 40 || got > 60 {
		t.Errorf("ascii estimate = %d, want 40..60", got)
	}
}

// 空消息列表返回 0。
func TestEstimateTokens_Empty(t *testing.T) {
	if got := estimateTokens(nil); got != 0 {
		t.Errorf("empty = %d, want 0", got)
	}
}

// 多 goroutine 并发 Append 同一 chatID 不应损坏 jsonl 文件：
// 每条 Message 序列化为一行，最终 load 回来行数应等于写入总数。
func TestHistory_ConcurrentAppendSafe(t *testing.T) {
	dir := t.TempDir()
	h, _ := NewHistory(dir, nil)
	const writers = 8
	const perWriter = 20
	var wg sync.WaitGroup
	wg.Add(writers)
	for w := 0; w < writers; w++ {
		go func(prefix int) {
			defer wg.Done()
			for i := 0; i < perWriter; i++ {
				if err := h.Append("chat1", []Message{
					{Role: "user", Content: fmt.Sprintf("u-%d-%d", prefix, i)},
					{Role: "assistant", Content: fmt.Sprintf("a-%d-%d", prefix, i)},
				}); err != nil {
					t.Errorf("append: %v", err)
				}
			}
		}(w)
	}
	wg.Wait()
	loaded := h.Load("chat1")
	// Load 会 trim，但写入总数远小于 maxHistoryTokens，应全部保留。
	want := writers * perWriter * 2
	if len(loaded) != want {
		t.Errorf("loaded = %d lines, want %d (concurrent append corrupted jsonl?)", len(loaded), want)
	}
}

func mkdir(p string) error              { return os.MkdirAll(p, 0o755) }
func writeFile(p, content string) error { return os.WriteFile(p, []byte(content), 0o644) }
