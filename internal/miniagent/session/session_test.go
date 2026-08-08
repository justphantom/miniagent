package session

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/justphantom/miniagent/internal/miniagent"
)

// TestMain 保留 os.Exit 语义；session 上限改为各测试经 LoadSession/AppendMessages 的 maxBytes 参数注入。
func TestMain(m *testing.M) {
	os.Exit(m.Run())
}

func sampleTranscript() []miniagent.Message {
	return []miniagent.Message{
		{Role: "user", Content: "看下 a.txt"},
		{Role: "assistant", ToolCalls: []miniagent.ToolCall{{ID: "c1", Name: "read", Args: `{"path":"a.txt"}`}}},
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
	const maxSz = int64(1 << 20)
	path := filepath.Join(t.TempDir(), "big.jsonl")
	if err := os.WriteFile(path, make([]byte, maxSz+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadSession(path, maxSz); err == nil {
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
	if err := AppendMessages(path, SessionMeta{ID: "s"}, []miniagent.Message{
		{Role: "user", Kind: miniagent.KindSummary, Content: "[既往对话摘要] xxx"},
	}); err != nil {
		t.Fatal(err)
	}
	_, msgs, err := LoadSession(path)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Kind != miniagent.KindSummary || msgs[0].Role != "user" {
		t.Errorf("summary kind not recognized: %+v", msgs)
	}
}

// P2-7：单条大消息（>1MiB 旧行上限、<maxSessionBytes 总上限）可正常 load，
// 不再因 scanner ErrTooLong 致整会话不可读、append-only 无法修复。
func TestLoadSession_LargeSingleLineOK(t *testing.T) {
	const maxSz = int64(1 << 20)
	path := filepath.Join(t.TempDir(), "s.jsonl")
	big := strings.Repeat("x", int(maxSz/2))
	if err := AppendMessages(path, SessionMeta{ID: "s"}, []miniagent.Message{{Role: "user", Content: big}}, maxSz); err != nil {
		t.Fatalf("AppendMessages: %v", err)
	}
	_, msgs, err := LoadSession(path, maxSz)
	if err != nil {
		t.Fatalf("P2-7：大单行 load 失败（scanner 单行上限不足）: %v", err)
	}
	if len(msgs) != 1 || len(msgs[0].Content) != len(big) {
		t.Errorf("大单行 round-trip 不一致: %+v", msgs)
	}
}

// H3-1：崩溃半写残留的尾行（无换行结尾）经 AppendMessages 必须被截断，否则 O_APPEND 盲写把新消息
// 拼到残行上，下次 LoadSession 把合并后的非法行容忍为尾行而丢失新消息（再下一次即中段损坏永久失败）。
func TestAppendMessages_HealsPartialTrailingLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	// 合法行 + 无换行结尾的半行（模拟崩溃中断的 append，末行无 \n）。
	content := "{\"type\":\"session\",\"id\":\"s\"}\n" +
		"{\"type\":\"message\",\"role\":\"user\",\"content\":\"ok\"}\n" +
		"{\"type\":\"message\",\"role\":\"assistant\",\"content\":\"half"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := AppendMessages(path, SessionMeta{ID: "s"}, []miniagent.Message{{Role: "user", Content: "after"}}); err != nil {
		t.Fatalf("AppendMessages: %v", err)
	}
	_, msgs, err := LoadSession(path)
	if err != nil {
		t.Fatalf("LoadSession after heal: %v", err)
	}
	// 半行 "half" 被截断；保留 ok + 新 after。未修时残行与 after 合并成非法行被容忍为尾行，msgs 仅 [ok]。
	if len(msgs) != 2 || msgs[0].Content != "ok" || msgs[1].Content != "after" {
		t.Errorf("heal 后消息 = %+v，want [ok, after]（残行截断、新消息保留）", msgs)
	}
}

// R4-1：已超 maxBytes 的文件（含崩溃半行）须直接拒绝且零修改——ensureTrailingNewline 慢路径
// LimitReader(mb+1) 在 size>mb 时不完整读取会错位截断丢合法行，故前置 size>mb 守卫（与 LoadSession 一致）。
func TestAppendMessages_OversizedFileRejectedUntouched(t *testing.T) {
	const mb = int64(1024)
	path := filepath.Join(t.TempDir(), "s.jsonl")
	line := "{\"type\":\"message\",\"role\":\"user\",\"content\":\"ok\"}\n"
	content := "{\"type\":\"session\",\"id\":\"s\"}\n" + strings.Repeat(line, 40) + "{half"
	if int64(len(content)) <= mb {
		t.Fatalf("fixture too small: %d bytes, need > %d", len(content), mb)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := AppendMessages(path, SessionMeta{ID: "s"}, []miniagent.Message{{Role: "user", Content: "after"}}, mb); err == nil {
		t.Fatal("超 mb 的文件应被拒绝")
	}
	// 关键：文件未被截断（R4-1 回归点——旧实现会静默截断丢合法行）。
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != content {
		t.Errorf("超 mb 文件被修改（应零副作用）：orig=%d 字节 got=%d 字节", len(content), len(got))
	}
}
