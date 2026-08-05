package miniagent

import (
	"encoding/json"
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

// truncateKeptReasoning（P7）：对保留窗口内（最近 keepN 条 assistant）的超长 Reasoning 做头尾分段，
// 压中段发散、留两端（头 threshold/4 + 尾 3*threshold/4 + 标记）；短 reasoning 不动；threshold<=0 关闭；
// 保留窗口外的不动（清空非保留是 P1 的职责）；无超长项零拷贝原样返回；不改调用方输入。
func TestTruncateKeptReasoning(t *testing.T) {
	threshold := 100
	long := strings.Repeat("x", threshold+200) // 超 threshold
	msgs := []Message{
		{Role: "user", Content: "q"},
		{Role: "assistant", Content: "a1", Reasoning: long}, // 保留窗口外（keepN=1）
		{Role: "assistant", Content: "a2", Reasoning: long}, // 最近 1 条 → 保留并截断
	}

	// keepN=1：仅最近一条 assistant 的超长 reasoning 被截断；更早的（窗口外）不动。
	out := truncateKeptReasoning(msgs, 1, threshold)
	if len(out[2].Reasoning) >= len(long) {
		t.Errorf("最近超长 reasoning 应被截断: len=%d", len(out[2].Reasoning))
	}
	if !strings.Contains(out[2].Reasoning, "推理中段已省略") {
		t.Errorf("截断应含中段省略标记: %q", out[2].Reasoning)
	}
	// 截断后体积约等于 threshold（头 1/4 + 尾 3/4 + marker）。
	if len([]rune(out[2].Reasoning)) > threshold+50 {
		t.Errorf("截断后应约等于 threshold: got %d runes", len([]rune(out[2].Reasoning)))
	}
	// 窗口外（a1）的 reasoning 原样——truncateKeptReasoning 只管保留窗口内。
	if out[1].Reasoning != long {
		t.Errorf("窗口外 reasoning 不应被动: len=%d", len(out[1].Reasoning))
	}
	// 调用方输入未被修改。
	if msgs[2].Reasoning != long {
		t.Errorf("caller reasoning mutated")
	}

	// 短 reasoning（< threshold）不动 → 原样返回（零拷贝）。
	short := []Message{
		{Role: "user", Content: "q"},
		{Role: "assistant", Content: "a", Reasoning: strings.Repeat("y", threshold-1)},
	}
	if got := truncateKeptReasoning(short, 1, threshold); &got[0] != &short[0] {
		t.Errorf("无超长项应原样返回同一 slice")
	}

	// threshold<=0 / <0 → 关闭，原样返回。
	if got := truncateKeptReasoning(msgs, 1, 0); &got[0] != &msgs[0] {
		t.Errorf("threshold=0 应原样返回")
	}
	if got := truncateKeptReasoning(msgs, 1, -1); &got[0] != &msgs[0] {
		t.Errorf("threshold<0 应原样返回（关闭）")
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
