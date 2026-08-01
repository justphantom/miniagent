package miniagent

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strings"
	"testing"
)

// trimHistoryForContext：清 reasoning + 压 tool content，不删消息、不改调用方输入。
func TestTrimHistoryForContext(t *testing.T) {
	msgs := []Message{
		{Role: "user", Content: "q"},
		{Role: "assistant", Content: "a", Reasoning: "long thought", ToolCalls: []ToolCall{{ID: "c1", Name: "read", Args: "{}"}}},
		{Role: "tool", ToolCallID: "c1", Content: strings.Repeat("x", 5000)},
	}
	out := trimHistoryForContext(msgs)

	if out[1].Reasoning != "" {
		t.Errorf("reasoning not cleared: %q", out[1].Reasoning)
	}
	if len(out[2].Content) > contextTrimToolChars+50 {
		t.Errorf("tool content not compressed: len=%d (want <= %d+marker)", len(out[2].Content), contextTrimToolChars)
	}
	if len(out) != 3 {
		t.Errorf("messages deleted: got %d, want 3 (pairing must hold)", len(out))
	}
	// 调用方输入未被修改。
	if msgs[1].Reasoning != "long thought" {
		t.Errorf("caller reasoning mutated")
	}
	if len(msgs[2].Content) != 5000 {
		t.Errorf("caller tool content mutated: len=%d", len(msgs[2].Content))
	}
}

func TestEstimateTokens(t *testing.T) {
	// 纯 ASCII：4 字符 ≈ 1 token
	if n := estimateTokens([]Message{{Role: "user", Content: "abcdefgh"}}); n != 2 {
		t.Errorf("ascii 8 chars = %d, want 2", n)
	}
	// 纯中文：2 字符 ≈ 1 token
	if n := estimateTokens([]Message{{Role: "user", Content: "四个汉字"}}); n != 2 {
		t.Errorf("cjk 4 chars = %d, want 2", n)
	}
	// tool_calls.Args 计入估算
	if n := estimateTokens([]Message{{Role: "assistant", ToolCalls: []ToolCall{{Args: "abcd"}}}}); n != 1 {
		t.Errorf("args 4 chars = %d, want 1", n)
	}
}

// compactHistory：保留首轮 + 末 keepRecent 轮，中段整轮剔除；tool_calls/tool 配对不变。
func TestCompactHistory(t *testing.T) {
	msgs := []Message{
		{Role: "user", Content: "task"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Name: "r", Args: "{}"}}},
		{Role: "tool", ToolCallID: "c1", Content: "r1"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c2", Name: "r", Args: "{}"}}},
		{Role: "tool", ToolCallID: "c2", Content: "r2"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c3", Name: "r", Args: "{}"}}},
		{Role: "tool", ToolCallID: "c3", Content: "r3"},
		{Role: "user", Content: "follow"},
	}
	out := compactHistory(msgs, 1) // 首轮 + 末 1 轮
	if err := validateToolPairing(out); err != nil {
		t.Errorf("pairing broken after compact: %v", err)
	}
	if out[0].Content != "task" {
		t.Errorf("first round lost: %+v", out[0])
	}
	if out[len(out)-1].Content != "follow" {
		t.Errorf("last round lost: %+v", out[len(out)-1])
	}
	for _, m := range out {
		if m.Role == roleTool {
			t.Errorf("middle tool round not compacted: %+v", m)
		}
	}
}

// 轮数不足 1+keepRecent 时原样返回（不裁剪）。
func TestCompactHistory_NoOpWhenSmall(t *testing.T) {
	msgs := []Message{
		{Role: "user", Content: "q"},
		{Role: "assistant", Content: "a"},
	}
	out := compactHistory(msgs, contextKeepRecent)
	if len(out) != 2 {
		t.Errorf("should not compact small history: len=%d", len(out))
	}
}

// ContextWindow 驱动 Run 在超阈值时调用 compactHistory（观察 logger warn）。
func TestRun_CompactsWhenOverWindow(t *testing.T) {
	tool := Tool{Name: "q", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "x"} }}
	bigArgs := strings.Repeat("a", 1000)
	tr := &fakeTransport{responses: []string{
		toolResponse(ToolCall{ID: "c1", Name: "q", Args: bigArgs}),
		toolResponse(ToolCall{ID: "c2", Name: "q", Args: bigArgs}),
		textResponse("done"),
	}}
	llm := &HTTPClient{APIKey: "sk", BaseURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))
	if _, err := Run(context.Background(), llm, LoopConfig{Tools: []Tool{tool}, ContextWindow: 200}, "x", LoopHooks{}, logger); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(buf.String(), "compacted") {
		t.Errorf("expected compact log when over window, got: %s", buf.String())
	}
}
