package miniagent

import (
	"context"
	"encoding/json"
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

// stripStaleReasoning（P1）：清空非最近 N 条 assistant 的 Reasoning，保留正文/tool_calls/最近 N 条思考；
// 不改调用方输入；无 reasoning 或全在保留窗口内时零拷贝原样返回。
func TestStripStaleReasoning(t *testing.T) {
	msgs := []Message{
		{Role: "user", Content: "q"},
		{Role: "assistant", Content: "a1", Reasoning: "think1", ToolCalls: []ToolCall{{ID: "c1", Name: "read", Args: "{}"}}},
		{Role: "tool", ToolCallID: "c1", Content: "r1"},
		{Role: "assistant", Content: "a2", Reasoning: "think2"},
		{Role: "assistant", Content: "a3", Reasoning: "think3"},
	}
	out := stripStaleReasoning(msgs, 1)
	// 仅最近一条 assistant（a3）保留 reasoning，更早的清空。
	if out[1].Reasoning != "" || out[3].Reasoning != "" {
		t.Errorf("旧 reasoning 未清空: out[1]=%q out[3]=%q", out[1].Reasoning, out[3].Reasoning)
	}
	if out[4].Reasoning != "think3" {
		t.Errorf("最近 reasoning 应保留: got %q", out[4].Reasoning)
	}
	// 正文 / tool_calls / tool 消息不动（结论与配对完整）。
	if out[1].Content != "a1" || len(out[1].ToolCalls) != 1 {
		t.Errorf("正文/tool_calls 被改动: %+v", out[1])
	}
	if out[2].Content != "r1" {
		t.Errorf("tool 消息被改动: %q", out[2].Content)
	}
	// 调用方输入未被修改。
	if msgs[1].Reasoning != "think1" {
		t.Errorf("caller reasoning mutated")
	}

	// keepN 覆盖全部 assistant → 原样返回（不拷贝）。
	if got := stripStaleReasoning(msgs, 10); &got[0] != &msgs[0] {
		t.Errorf("全在保留窗口内应原样返回同一 slice")
	}

	// 无 reasoning 的历史（非思考模型）→ 原样返回。
	plain := []Message{{Role: "user", Content: "q"}, {Role: "assistant", Content: "a"}}
	if got := stripStaleReasoning(plain, 1); &got[0] != &plain[0] {
		t.Errorf("无 reasoning 应原样返回同一 slice")
	}

	// keepN=0 → 清空全部 assistant reasoning。
	out0 := stripStaleReasoning(msgs, 0)
	for i, m := range out0 {
		if m.Role == roleAssistant && m.Reasoning != "" {
			t.Errorf("keepN=0 应清空所有 reasoning: out[%d]=%q", i, m.Reasoning)
		}
	}
}

// stripStaleToolArgs（P4）：压缩非最近 N 条 assistant 的 write/edit 大 Args（content/old_string/
// new_string）为前缀占位；保留最近 N 条原文；不碰配对/正文/ID；小 args 与非 write/edit 工具不动；
// 无可压缩项原样返回（零拷贝）；不改调用方输入。
func TestStripStaleToolArgs(t *testing.T) {
	big := strings.Repeat("x", toolArgsCompressThreshold+100)
	writeArgs := func(path string) string {
		return `{"path":"` + path + `","content":"` + big + `"}`
	}
	msgs := []Message{
		{Role: "user", Content: "q"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Name: "write", Args: writeArgs("a.go")}}},
		{Role: "tool", ToolCallID: "c1", Content: "已写入"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c2", Name: "write", Args: writeArgs("b.go")}}},
		{Role: "tool", ToolCallID: "c2", Content: "已写入"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c3", Name: "write", Args: writeArgs("c.go")}}},
	}
	out := stripStaleToolArgs(msgs, 1) // 仅最近 1 条 assistant 保留原文
	// 最近一条 assistant（c3）的 args 原样；更早的（c1）被压缩。
	if out[5].ToolCalls[0].Args != msgs[5].ToolCalls[0].Args {
		t.Errorf("最近 assistant args 应保留原文")
	}
	if len(out[1].ToolCalls[0].Args) >= len(big) {
		t.Errorf("旧 write args 应被压缩: len=%d", len(out[1].ToolCalls[0].Args))
	}
	// 压缩后仍是合法 JSON 且保留 path；ID 与 tool 消息不动。
	var m map[string]any
	if err := json.Unmarshal([]byte(out[1].ToolCalls[0].Args), &m); err != nil {
		t.Errorf("压缩后 args 非合法 JSON: %q (err %v)", out[1].ToolCalls[0].Args, err)
	}
	if m["path"] != "a.go" {
		t.Errorf("path 应保留: got %v", m["path"])
	}
	if out[1].ToolCalls[0].ID != "c1" {
		t.Errorf("ToolCallID 被改动")
	}
	if out[2].Content != "已写入" {
		t.Errorf("tool 消息被改动: %q", out[2].Content)
	}
	// 调用方输入未被修改（仍含完整 big 原文）。
	if !strings.Contains(msgs[1].ToolCalls[0].Args, big) {
		t.Errorf("caller args 被修改")
	}

	// 小 args（< threshold）不压缩 → 原样返回（零拷贝）。
	small := []Message{
		{Role: "user", Content: "q"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c", Name: "write", Args: `{"path":"a","content":"short"}`}}},
	}
	if got := stripStaleToolArgs(small, 0); &got[0] != &small[0] {
		t.Errorf("无可压缩大 args 应原样返回同一 slice")
	}

	// 非 write/edit 工具（read）不压缩 → 原样返回。
	readMsgs := []Message{
		{Role: "user", Content: "q"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c", Name: "read", Args: `{"path":"a"}`}}},
	}
	if got := stripStaleToolArgs(readMsgs, 0); &got[0] != &readMsgs[0] {
		t.Errorf("非 write/edit 会话应原样返回")
	}

	// 全在保留窗口内 → 原样返回。
	if got := stripStaleToolArgs(msgs, 10); &got[0] != &msgs[0] {
		t.Errorf("全在保留窗口内应原样返回同一 slice")
	}
}

// compressToolArgs（P4）：超阈值字段压成前缀 + 标记；阈值内不动；edits 数组同样处理；
// path 保留；解析失败原样返回。
func TestCompressToolArgs(t *testing.T) {
	big := strings.Repeat("x", toolArgsCompressThreshold+10)
	// write content 超阈值 → 压缩，path 保留。
	got := compressToolArgs(`{"path":"a.go","content":"` + big + `"}`)
	var m map[string]any
	if err := json.Unmarshal([]byte(got), &m); err != nil {
		t.Fatalf("compressed args not valid JSON: %v %q", err, got)
	}
	if m["path"] != "a.go" {
		t.Errorf("path should be preserved: %v", m["path"])
	}
	if c, _ := m["content"].(string); len(c) >= len(big) {
		t.Errorf("content should be compressed: len=%d", len(c))
	}

	// 小 content（< threshold）→ 原样（无标记）。
	small := `{"path":"a.go","content":"short"}`
	if got := compressToolArgs(small); got != small {
		t.Errorf("small args should be unchanged: got %q", got)
	}

	// edits 数组里的 old/new_string 超阈值也压。
	got = compressToolArgs(`{"path":"a.go","edits":[{"old_string":"` + big + `","new_string":"` + big + `"}]}`)
	var em map[string]any
	if err := json.Unmarshal([]byte(got), &em); err != nil {
		t.Fatalf("edits args not valid JSON: %v", err)
	}
	edits, _ := em["edits"].([]any)
	if len(edits) != 1 {
		t.Fatalf("edits lost: %+v", em)
	}
	e0, _ := edits[0].(map[string]any)
	if oldS, _ := e0["old_string"].(string); len(oldS) >= len(big) {
		t.Errorf("edits old_string should be compressed: len=%d", len(oldS))
	}

	// 非法 JSON → 原样返回。
	bad := `{"path": broken`
	if got := compressToolArgs(bad); got != bad {
		t.Errorf("invalid JSON should be returned as-is: got %q", got)
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
	// tools schema 按实际 JSON 体积估算（估算对齐）：description 越长 token 越多，非 flat 常量。
	small := estimateTokens(msgs, "", []Tool{{Name: "t", Description: "abcd"}}) - base
	large := estimateTokens(msgs, "", []Tool{{Name: "t", Description: strings.Repeat("a", 400)}}) - base
	if large <= small {
		t.Errorf("schema estimate should grow with description length: small=%d large=%d", small, large)
	}
	if small <= 0 {
		t.Errorf("non-empty tool schema should add tokens: small=%d", small)
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
