package miniagent

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// 用较小上限覆盖内置 50MB，避免 race detector 下 JSON marshal 大字符串超时。
func TestMain(m *testing.M) {
	SetMaxSessionBytes(1 << 20) // 1MB，足够覆盖边界测试，race 下快速
	os.Exit(m.Run())
}

func sampleTranscript() []Message {
	return []Message{
		{Role: "user", Content: "看下 a.txt"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Name: "read", Args: `{"path":"a.txt"}`}}},
		{Role: "tool", ToolCallID: "c1", Content: "1 │ hello"},
		{Role: "assistant", Content: "a.txt 内容是 hello"},
	}
}

func writeLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Append→Load round-trip：消息逐一相等，metadata 复现。
func TestSession_RoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	want := sampleTranscript()
	meta := SessionMeta{ID: "s", Model: "main/glm", Workdir: "/abs", Provider: "main"}
	if err := AppendMessages(path, meta, want); err != nil {
		t.Fatalf("AppendMessages: %v", err)
	}
	gotMeta, got, err := LoadSession(path)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("round-trip mismatch:\n got %+v\nwant %+v", got, want)
	}
	if gotMeta.ID != "s" || gotMeta.Model != "main/glm" {
		t.Errorf("meta mismatch: %+v", gotMeta)
	}
}

// 文件不存在 → (零 meta, nil, nil)，等同新会话。
func TestLoadSession_MissingFileReturnsNil(t *testing.T) {
	meta, msgs, err := LoadSession(filepath.Join(t.TempDir(), "nope.jsonl"))
	if err != nil || msgs != nil || meta.Type != "" {
		t.Errorf("got (%+v, %v, %v), want (零 meta, nil, nil)", meta, msgs, err)
	}
}

// 中间损坏的 JSON 行（合法行夹在前后）必须报错。
func TestLoadSession_CorruptJSONFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.jsonl")
	writeLines(t, path, `{"type":"session","id":"s"}`, `{not json`, `{"type":"message","role":"user","content":"after"}`)
	if _, _, err := LoadSession(path); err == nil {
		t.Fatal("expected error for corrupt JSON line in the middle")
	}
}

// append-only 崩溃残留的尾行半写应被容忍：丢弃残行，加载此前合法的历史。
func TestLoadSession_ToleratesCorruptTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tail.jsonl")
	writeLines(t, path,
		`{"type":"session","id":"s"}`,
		`{"type":"message","role":"user","content":"ok"}`,
		`{broken half line`)
	_, msgs, err := LoadSession(path)
	if err != nil {
		t.Fatalf("corrupt tail should be tolerated: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Content != "ok" {
		t.Errorf("expected 1 valid message before corrupt tail, got %+v", msgs)
	}
}

func TestLoadSession_UnknownRoleFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "role.jsonl")
	writeLines(t, path, `{"type":"message","role":"system","content":"x"}`)
	if _, _, err := LoadSession(path); err == nil {
		t.Fatal("expected error for unknown role")
	}
}

func TestLoadSession_ToolMissingCallIDFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tool.jsonl")
	writeLines(t, path, `{"type":"message","role":"tool","content":"x"}`)
	if _, _, err := LoadSession(path); err == nil {
		t.Fatal("expected error for tool message without tool_call_id")
	}
}

func TestLoadSession_OversizedFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "big.jsonl")
	if err := os.WriteFile(path, make([]byte, sessionBytes()+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadSession(path); err == nil {
		t.Fatal("expected error for oversized session file")
	}
}

// tool_calls 与 tool 配对断裂必须拒绝。
func TestLoadSession_DanglingToolCallFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dangling.jsonl")
	writeLines(t, path,
		`{"type":"message","role":"user","content":"q"}`,
		`{"type":"message","role":"assistant","content":"","tool_calls":[{"id":"c1","name":"read","args":"{}"}]}`)
	if _, _, err := LoadSession(path); err == nil {
		t.Fatal("expected error for dangling tool_call")
	}
}

func TestLoadSession_OrphanToolMessageFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "orphan.jsonl")
	writeLines(t, path,
		`{"type":"message","role":"user","content":"q"}`,
		`{"type":"message","role":"tool","tool_call_id":"cX","content":"x"}`)
	if _, _, err := LoadSession(path); err == nil {
		t.Fatal("expected error for orphan tool message")
	}
}

// kind=summary 结构化标记可读（审查 v3 #2），role=user 合法持久化。
func TestLoadSession_KindSummaryRecognized(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sum.jsonl")
	if err := AppendMessages(path, SessionMeta{ID: "s"}, []Message{
		{Role: "user", Kind: KindSummary, Content: "[既往对话摘要] xxx"},
	}); err != nil {
		t.Fatal(err)
	}
	_, msgs, err := LoadSession(path)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Kind != KindSummary || msgs[0].Role != "user" {
		t.Errorf("summary kind not recognized: %+v", msgs)
	}
}

// P2-7：单条大消息（>1MiB 旧行上限、<maxSessionBytes 总上限）可正常 load，
// 不再因 scanner ErrTooLong 致整会话不可读、append-only 无法修复。
func TestLoadSession_LargeSingleLineOK(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	big := strings.Repeat("x", int(sessionBytes()/2))
	if err := AppendMessages(path, SessionMeta{ID: "s"}, []Message{{Role: "user", Content: big}}); err != nil {
		t.Fatalf("AppendMessages: %v", err)
	}
	_, msgs, err := LoadSession(path)
	if err != nil {
		t.Fatalf("P2-7：大单行 load 失败（scanner 单行上限不足）: %v", err)
	}
	if len(msgs) != 1 || len(msgs[0].Content) != len(big) {
		t.Errorf("大单行 round-trip 不一致: %+v", msgs)
	}
}
