package miniagent

import (
	"context"
	"strconv"
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
	// 纯 ASCII：4 字符 ≈ 1 token；空 system + 无工具时仅加 systemOverheadTokens 固定开销。
	if n := estimateTokens([]Message{{Role: "user", Content: "abcdefgh"}}, "", nil); n != 2+systemOverheadTokens {
		t.Errorf("ascii 8 chars = %d, want %d", n, 2+systemOverheadTokens)
	}
	// 纯中文：2 字符 ≈ 1 token
	if n := estimateTokens([]Message{{Role: "user", Content: "四个汉字"}}, "", nil); n != 2+systemOverheadTokens {
		t.Errorf("cjk 4 chars = %d, want %d", n, 2+systemOverheadTokens)
	}
	// tool_calls.Args 计入估算
	if n := estimateTokens([]Message{{Role: "assistant", ToolCalls: []ToolCall{{Args: "abcd"}}}}, "", nil); n != 1+systemOverheadTokens {
		t.Errorf("args 4 chars = %d, want %d", n, 1+systemOverheadTokens)
	}
}

// P2 estimateTokens 失明：system prompt 内容 + 工具 schema 固定开销须计入，否则压缩触发偏晚。
// 用 delta 断言，使测试不依赖具体常量取值（常量凭经验可调）。
func TestEstimateTokens_Overhead(t *testing.T) {
	msgs := []Message{{Role: "user", Content: "abcdefgh"}}
	base := estimateTokens(msgs, "", nil) // 2 内容 + systemOverheadTokens
	// system prompt 文本计入：4 ASCII 字符 = 1 token。
	if got := estimateTokens(msgs, "abcd", nil) - base; got != 1 {
		t.Errorf("system 4 chars should add 1 token, got delta %d", got)
	}
	// 每个工具 schema 加 perToolSchemaTokens。
	if got := estimateTokens(msgs, "", []Tool{{}, {}}) - base; got != 2*perToolSchemaTokens {
		t.Errorf("2 tools should add %d tokens, got delta %d", 2*perToolSchemaTokens, got)
	}
	// system 内容随长度增长（防回归成纯常量）。
	if got := estimateTokens(msgs, strings.Repeat("a", 40), nil) - base; got != 10 {
		t.Errorf("system 40 chars should add 10 tokens, got delta %d", got)
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

// Run 超 window 阈值触发摘要压缩：summary 进 context 又落 NewMessages（审查 v3 #12），
// 且压缩后不再超 → Run 正常结束。transport 对摘要调用与主调用都回短文本。
func TestRun_SummaryReducesAndPersists(t *testing.T) {
	var hist []Message
	for i := range 10 {
		hist = append(hist,
			Message{Role: roleUser, Content: strings.Repeat("q", 50) + strconv.Itoa(i)},
			Message{Role: roleAssistant, Content: strings.Repeat("a", 50) + strconv.Itoa(i)},
		)
	}
	tr := &fakeTransport{responses: []string{textResponse("ok"), textResponse("done")}}
	chat, stream := testClients(tr)
	// ContextWindow 取在「压缩前 > 80%、压缩后 ≤ 80%」之间；estimateTokens 现计入 system/tools
	// 固定开销（systemOverheadTokens=400），256 的小窗口会使压缩后仍超 → 误报失败，故放到 750。
	res, err := Run(context.Background(), chat, stream, LoopConfig{ContextWindow: 750, History: hist}, "now", LoopHooks{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var hasSummary bool
	for _, m := range res.NewMessages {
		if m.Kind == KindSummary {
			hasSummary = true
		}
	}
	if !hasSummary {
		t.Errorf("NewMessages missing persisted summary: %+v", res.NewMessages)
	}
	// Fix 6：摘要压缩成功 → result.Compacted=true（交互层据此 rewrite session）。
	if !res.Compacted {
		t.Errorf("Compacted should be true after summary compaction")
	}
	// Fix 3：摘要调用 usage 须计入 total。摘要与主调用各回 prompt=1/completion=1（textResponse），
	// 摘要 usage 入 total 后 InputTokens ≥ 2（不计则仅主调用 1）。
	if res.Usage.InputTokens < 2 || res.Usage.OutputTokens < 2 {
		t.Errorf("summary usage not counted in total: %+v", res.Usage)
	}
}

// 即使反复有损裁剪仍超 window → 报错终止（避免循环烧请求，审查 v3 #4）。
func TestRun_OverWindowIrreducibleErrors(t *testing.T) {
	tool := Tool{Name: "q", Call: func(context.Context, string) ToolResult { return ToolResult{Output: "x"} }}
	bigArgs := strings.Repeat("a", 1000)
	tr := &fakeTransport{responses: []string{
		toolResponse(ToolCall{ID: "c1", Name: "q", Args: bigArgs}),
		textResponse("ok"),
		textResponse("done"),
	}}
	chat, stream := testClients(tr)
	_, err := Run(context.Background(), chat, stream, LoopConfig{Tools: []Tool{tool}, ContextWindow: 200}, "x", LoopHooks{}, nil)
	if err == nil {
		t.Fatal("expected error when irreducibly over window")
	}
}
