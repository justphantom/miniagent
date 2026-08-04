package miniagent

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// writeMemoryTool 追加 {type:note} 记录，readMemoryTool 渲染为文本；round-trip + 落盘到 .miniagent/memory.jsonl。
func TestMemory_WriteThenRead(t *testing.T) {
	dir := t.TempDir()
	if r := writeMemoryTool(dir, "避免在测试里 time.Sleep"); r.IsError {
		t.Fatalf("write: %s", r.Output)
	}
	if r := writeMemoryTool(dir, "client.go 是 HTTP 层"); r.IsError {
		t.Fatalf("write2: %s", r.Output)
	}
	// 文件落盘到 .miniagent/memory.jsonl，0o600。
	path := filepath.Join(dir, ".miniagent", "memory.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("memory file not written: %v", err)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("memory file mode = %o, want 600", info.Mode().Perm())
	}
	if !strings.Contains(string(data), `"type":"note"`) || !strings.Contains(string(data), "避免在测试里 time.Sleep") {
		t.Errorf("memory jsonl missing record: %s", data)
	}
	// readMemoryTool 渲染两条记录。
	r := readMemoryTool(dir)
	if r.IsError {
		t.Fatalf("read: %s", r.Output)
	}
	if !strings.Contains(r.Output, "[note]") || !strings.Contains(r.Output, "避免在测试里 time.Sleep") || !strings.Contains(r.Output, "client.go 是 HTTP 层") {
		t.Errorf("read rendered wrong: %s", r.Output)
	}
}

// 无 memory 文件 → readMemoryTool 给出友好提示而非报错。
func TestMemory_ReadEmpty(t *testing.T) {
	r := readMemoryTool(t.TempDir())
	if r.IsError || !strings.Contains(r.Output, "暂无") {
		t.Errorf("empty read should be friendly non-error: %+v", r)
	}
}

// writeMemoryTool 空 content 报错。
func TestMemory_WriteEmptyRejected(t *testing.T) {
	if r := writeMemoryTool(t.TempDir(), "   "); !r.IsError {
		t.Error("empty content should be rejected")
	}
}

// FormatMemorySnippet 只注入最近 memoryRecentN 条。
func TestMemory_FormatSnippetLastN(t *testing.T) {
	dir := t.TempDir()
	for i := range memoryRecentN + 2 {
		if err := appendMemoryRecord(dir, memoryRecord{Type: "lesson", Topic: "t", Content: "c" + strconv.Itoa(i)}); err != nil {
			t.Fatal(err)
		}
	}
	snip := FormatMemorySnippet(dir)
	if !strings.Contains(snip, "## 近期项目记忆") {
		t.Errorf("snippet missing header: %s", snip)
	}
	// 旧记录 c0、c1 不在最近 N（memoryRecentN）内。
	if strings.Contains(snip, "c0\n") || strings.Contains(snip, "c0 ") {
		t.Errorf("snippet should drop oldest beyond last N: %s", snip)
	}
	if !strings.Contains(snip, "c"+strconv.Itoa(memoryRecentN+1)) {
		t.Errorf("snippet should include newest: %s", snip)
	}
}

// FormatMemorySnippet 无记忆返回空串（不污染 system prompt）。
func TestMemory_FormatSnippetEmpty(t *testing.T) {
	if got := FormatMemorySnippet(t.TempDir()); got != "" {
		t.Errorf("empty snippet should be \"\", got %q", got)
	}
}

// read/write 工具的保留路径 "memory" 路由到项目记忆（绕过普通文件路径）。
func TestReadWriteTools_MemoryTokenRouting(t *testing.T) {
	dir := t.TempDir()
	w := WriteFileTool(dir, 0)
	if r := w.Call(context.Background(), `{"path":"memory","content":"一条记忆"}`); r.IsError {
		t.Fatalf("write memory: %s", r.Output)
	}
	r := ReadFileTool(dir, 0).Call(context.Background(), `{"path":"memory"}`)
	if r.IsError {
		t.Fatalf("read memory: %s", r.Output)
	}
	if !strings.Contains(r.Output, "一条记忆") {
		t.Errorf("read memory did not route to memory file: %s", r.Output)
	}
}

// .miniagent 目录为符号链接时，read/write memory 均须拒绝，防止越界读写。
func TestMemory_SymlinkDirRejected(t *testing.T) {
	dir := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(dir, ".miniagent")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	if r := readMemoryTool(dir); !r.IsError {
		t.Errorf("read should reject symlink .miniagent dir")
	}
	if r := writeMemoryTool(dir, "x"); !r.IsError {
		t.Errorf("write should reject symlink .miniagent dir")
	}
}

// memory.jsonl 自身为符号链接时，read/write memory 均须拒绝。
func TestMemory_SymlinkFileRejected(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "real.jsonl")
	if err := os.WriteFile(outside, []byte("leak"), 0o600); err != nil {
		t.Fatal(err)
	}
	maDir := filepath.Join(dir, ".miniagent")
	if err := os.MkdirAll(maDir, 0o750); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(maDir, "memory.jsonl")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	if r := readMemoryTool(dir); !r.IsError {
		t.Errorf("read should reject symlink memory file")
	}
	if r := writeMemoryTool(dir, "x"); !r.IsError {
		t.Errorf("write should reject symlink memory file")
	}
}

// writeMemoryTool / readMemoryTool 包装内部函数，便于测试。
func TestMessagesUseTools(t *testing.T) {
	if MessagesUseTools([]Message{{Role: roleUser}, {Role: roleAssistant}}) {
		t.Error("no tools: want false")
	}
	if !MessagesUseTools([]Message{{Role: roleAssistant, ToolCalls: []ToolCall{{ID: "1"}}}}) {
		t.Error("tool_calls: want true")
	}
	if !MessagesUseTools([]Message{{Role: roleTool}}) {
		t.Error("role tool: want true")
	}
}

func TestMessagesUseWriteOrEdit(t *testing.T) {
	// 无工具调用：false
	if MessagesUseWriteOrEdit([]Message{{Role: roleUser, Content: "hi"}, {Role: roleAssistant, Content: "hello"}}) {
		t.Error("no write/edit: want false")
	}
	// read/grep/glob/shell：false
	if MessagesUseWriteOrEdit([]Message{{Role: roleAssistant, ToolCalls: []ToolCall{{Name: "read"}}}}) {
		t.Error("read tool: want false")
	}
	if MessagesUseWriteOrEdit([]Message{{Role: roleAssistant, ToolCalls: []ToolCall{{Name: "glob"}}}}) {
		t.Error("glob tool: want false")
	}
	if MessagesUseWriteOrEdit([]Message{{Role: roleAssistant, ToolCalls: []ToolCall{{Name: "grep"}}}}) {
		t.Error("grep tool: want false")
	}
	if MessagesUseWriteOrEdit([]Message{{Role: roleAssistant, ToolCalls: []ToolCall{{Name: "shell"}}}}) {
		t.Error("shell tool: want false")
	}
	// write/edit：true
	if !MessagesUseWriteOrEdit([]Message{{Role: roleAssistant, ToolCalls: []ToolCall{{Name: "write"}}}}) {
		t.Error("write tool: want true")
	}
	if !MessagesUseWriteOrEdit([]Message{{Role: roleAssistant, ToolCalls: []ToolCall{{Name: "edit"}}}}) {
		t.Error("edit tool: want true")
	}
}

func TestFilterMemoryRecords_DedupSecretCapDefaultType(t *testing.T) {
	existing := []memoryRecord{{Type: "note", Content: "  重复  "}}
	in := []memoryRecord{
		{Topic: "构建", Content: "go build 用 ./..."},
		{Content: "重复"},             // 与 existing 归一化重复 → 丢
		{Content: "sk-secret-leak"}, // 含密钥 → 丢
		{Content: "事实A"},
		{Content: "事实B"},
		{Content: "事实C"},
		{Content: "事实D"}, // 超 maxRecords=3 → 丢
	}
	got := filterMemoryRecords(in, 3, existing, []string{"sk-secret-leak"})
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3: %+v", len(got), got)
	}
	if got[0].Topic != "构建" || got[0].Content != "go build 用 ./..." {
		t.Errorf("rec0 = %+v", got[0])
	}
	if got[1].Content != "事实A" || got[2].Content != "事实B" {
		t.Errorf("rec1/2 = %+v %+v", got[1], got[2])
	}
	for _, r := range got {
		if r.Type == "" {
			t.Error("type should default to note")
		}
		if strings.Contains(r.Content, "sk-secret") {
			t.Error("secret leaked into record")
		}
	}
}

func TestExtractMemory_ParsesAndFilters(t *testing.T) {
	arr := `[{"type":"note","topic":"构建","content":"go build 用 ./..."},{"content":"重复"},{"content":"sk-secret-leak"},{"content":"事实A"},{"content":"事实B"},{"content":"事实C"},{"content":"事实D"}]`
	tr := &fakeTransport{responses: []string{textResponse(arr)}}
	llm := &ChatClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	existing := []memoryRecord{{Type: "note", Content: "重复"}}
	transcript := []Message{{Role: roleUser, Content: "跑构建"}, {Role: roleAssistant, Content: "ok"}}

	recs, _, err := ExtractMemory(context.Background(), llm, "m", "", 3, existing, []string{"sk-secret-leak"}, transcript)
	if err != nil {
		t.Fatalf("ExtractMemory: %v", err)
	}
	if len(recs) != 3 {
		t.Fatalf("len = %d, want 3: %+v", len(recs), recs)
	}
	if recs[0].Topic != "构建" {
		t.Errorf("rec0 topic = %q", recs[0].Topic)
	}
}

// 模型输出非法 JSON：解析失败应安静丢弃（返回空记录，不报错）。
func TestExtractMemory_BadJSONReturnsEmpty(t *testing.T) {
	tr := &fakeTransport{responses: []string{textResponse("不是 JSON")}}
	llm := &ChatClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	recs, _, err := ExtractMemory(context.Background(), llm, "m", "", 3, nil, nil, []Message{{Role: roleUser, Content: "x"}})
	if err != nil {
		t.Fatalf("want nil err on bad json, got %v", err)
	}
	if len(recs) != 0 {
		t.Errorf("want 0 recs, got %+v", recs)
	}
}

// ExtractMemory 必须把 transcript 作为 user message 传入，而非全塞 system——
// 要求 user role 的端点（如 agnes）对仅 system 的请求会 400 "No user query found in messages"。
func TestExtractMemory_SendsTranscriptAsUserMessage(t *testing.T) {
	tr := &fakeTransport{responses: []string{textResponse(`[{"content":"事实A"}]`)}}
	llm := &ChatClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	transcript := []Message{{Role: roleUser, Content: "跑构建 go build"}, {Role: roleAssistant, Content: "ok"}}

	if _, _, err := ExtractMemory(context.Background(), llm, "m", "", 3, nil, nil, transcript); err != nil {
		t.Fatalf("ExtractMemory: %v", err)
	}
	if tr.calls != 1 {
		t.Fatalf("calls = %d, want 1", tr.calls)
	}
	body := tr.lastBody
	if !strings.Contains(body, `"role":"user"`) {
		t.Errorf("request missing user message: %s", body)
	}
	if !strings.Contains(body, "跑构建 go build") {
		t.Errorf("user message missing transcript content: %s", body)
	}
	if strings.Count(body, `"role":"system"`) != 1 {
		t.Errorf("want exactly 1 system message, body: %s", body)
	}
}

// SetMemoryRecentN 必须在 FormatMemorySnippet 调用前生效（main.go 已调整为先 Set 再 load）。
func TestFormatMemorySnippet_RespectsRecentNOverride(t *testing.T) {
	dir := t.TempDir()
	for i := range 15 {
		if err := appendMemoryRecord(dir, memoryRecord{Type: "note", Content: "m" + strconv.Itoa(i)}); err != nil {
			t.Fatal(err)
		}
	}
	SetMemoryRecentN(3)
	defer SetMemoryRecentN(memoryRecentN) // 还原默认，避免污染其他测试
	snip := FormatMemorySnippet(dir)
	for _, want := range []string{"m12", "m13", "m14"} {
		if !strings.Contains(snip, want) {
			t.Errorf("snippet should include %s: %s", want, snip)
		}
	}
	for _, old := range []string{"m0", "m11"} {
		if strings.Contains(snip, old) {
			t.Errorf("snippet should drop records beyond last N=3: %s", snip)
		}
	}
}
