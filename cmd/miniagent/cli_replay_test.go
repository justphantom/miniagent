package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/justphantom/miniagent/internal/miniagent"
	"github.com/justphantom/miniagent/internal/miniagent/session"
)

// parseNDJSON 把 combined output 按行解析为事件列表，跳过空行与非 JSON（如 slog 日志行）。
func parseNDJSON(t *testing.T, out string) []map[string]any {
	t.Helper()
	var events []map[string]any
	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.HasPrefix(line, "{") {
			continue
		}
		var e map[string]any
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue
		}
		events = append(events, e)
	}
	return events
}

func equalSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// replaySession 单测：已知消息序列 → 精确事件序列。覆盖：
//   - assistant 带 tool_calls+reasoning（step1：tool_use + reasoning_delta，无 text_delta）；
//   - tool 消息经 callID→name map 反查工具名发 tool_result；
//   - 终态 assistant 纯文本（step2：text_delta）；
//   - 多步用量累加、result 汇总（steps/text/finish/model/usage）。
func TestReplaySession(t *testing.T) {
	meta := session.SessionMeta{
		Type: "session", ID: "s1", Model: "p/m", Provider: "p", Workdir: "/wd", Created: "2026-01-01T00:00:00Z",
	}
	msgs := []miniagent.Message{
		{Role: miniagent.RoleUser, Content: "提问"},
		{Role: miniagent.RoleAssistant, Reasoning: "我想想", ToolCalls: []miniagent.ToolCall{{ID: "c1", Name: "read", Args: `{"path":"/x"}`}}, Usage: &miniagent.Usage{InputTokens: 10, OutputTokens: 5}},
		{Role: miniagent.RoleTool, ToolCallID: "c1", Content: "文件内容"},
		{Role: miniagent.RoleAssistant, Content: "最终回答", Usage: &miniagent.Usage{InputTokens: 20, OutputTokens: 8}},
	}
	var buf bytes.Buffer
	if err := replaySession(&buf, meta, msgs); err != nil {
		t.Fatalf("replaySession: %v", err)
	}
	ev := parseNDJSON(t, buf.String())

	wantTypes := []string{"session", "tool_use", "reasoning_delta", "tool_result", "text_delta", "result"}
	if !equalSlice(eventTypes(ev), wantTypes) {
		t.Fatalf("事件类型 = %v, want %v", eventTypes(ev), wantTypes)
	}
	if ev[0]["id"] != "s1" || ev[0]["model"] != "p/m" {
		t.Errorf("session id/model = %v/%v", ev[0]["id"], ev[0]["model"])
	}
	if ev[1]["name"] != "read" || ev[1]["input"] != `{"path":"/x"}` {
		t.Errorf("tool_use name/input = %v/%v", ev[1]["name"], ev[1]["input"])
	}
	if ev[2]["step"] != float64(1) || ev[2]["text"] != "我想想" {
		t.Errorf("reasoning_delta = %+v", ev[2])
	}
	// tool_result 的 name 由 c1 反查得 read；output 经 EmitToolResult（短串不截断）。
	if ev[3]["name"] != "read" || ev[3]["call_id"] != "c1" || ev[3]["output"] != "文件内容" {
		t.Errorf("tool_result = %+v", ev[3])
	}
	if ev[4]["step"] != float64(2) || ev[4]["text"] != "最终回答" {
		t.Errorf("text_delta = %+v", ev[4])
	}
	if ev[5]["text"] != "最终回答" || ev[5]["steps"] != float64(2) || ev[5]["finish"] != "stop" ||
		ev[5]["model"] != "p/m" || ev[5]["input_tokens"] != float64(30) || ev[5]["output_tokens"] != float64(13) {
		t.Errorf("result = %+v", ev[5])
	}
}

// 孤立 tool 消息（tool_call_id 在 assistant.ToolCalls 中无对应）→ name 回落空串，不崩；steps=0。
func TestReplaySession_OrphanTool(t *testing.T) {
	msgs := []miniagent.Message{
		{Role: miniagent.RoleTool, ToolCallID: "ghost", Content: "残"},
	}
	var buf bytes.Buffer
	if err := replaySession(&buf, session.SessionMeta{Type: "session", ID: "s", Model: "m"}, msgs); err != nil {
		t.Fatalf("replaySession: %v", err)
	}
	ev := parseNDJSON(t, buf.String())
	if !equalSlice(eventTypes(ev), []string{"session", "tool_result", "result"}) {
		t.Fatalf("事件类型 = %v", eventTypes(ev))
	}
	if ev[1]["name"] != "" || ev[1]["call_id"] != "ghost" {
		t.Errorf("孤立 tool_result name 应为空串: %+v", ev[1])
	}
}

// e2e：-save-session 造一个含 read 工具调用的会话 → -replay 重显。验证 replay 把落盘消息
// 翻译成 NDJSON 事件流：session/tool_use/tool_result/text_delta/result，关键字段对齐。
// 已知差异：save 那次非流式不发 text_delta，replay 总发（整串）；result.model 取 session 元数据 "p/m"，
// 与运行时 result（用 ModelID "m"）不同——均属认可精度边界。
func TestCLI_Replay(t *testing.T) {
	target := filepath.Join(t.TempDir(), "t.txt")
	if err := os.WriteFile(target, []byte("hello-target"), 0o600); err != nil {
		t.Fatal(err)
	}
	// read args 经二次 Marshal 成合法 JSON 字符串嵌入 wire 的 arguments 字段。
	argsVal, _ := json.Marshal(map[string]string{"path": target})
	argsStr, _ := json.Marshal(string(argsVal))

	var mu sync.Mutex
	count := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		count++
		n := count
		mu.Unlock()
		if n == 1 {
			fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"read","arguments":`+string(argsStr)+`}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
			return
		}
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":2}}`)
	}))
	defer srv.Close()

	sessionDir := t.TempDir()
	cfgPath := writeSessionConfig(t, srv.URL, sessionDir)
	code, out := runMainBin(t, "读那个文件", []string{"-config", cfgPath, "-save-session"}, "MINIAGENT_API_KEY=sk-test")
	if code != 0 {
		t.Fatalf("save-session code=%d out=%s", code, out)
	}
	matches, _ := filepath.Glob(filepath.Join(sessionDir, "*.jsonl"))
	if len(matches) != 1 {
		t.Fatalf("session 文件数=%d out=%s", len(matches), out)
	}
	id := strings.TrimSuffix(filepath.Base(matches[0]), ".jsonl")

	// replay：空 stdin（replay 不读 stdin、不调 LLM）。
	code, out2 := runMainBin(t, "", []string{"-config", cfgPath, "-replay", id}, "MINIAGENT_API_KEY=sk-test")
	if code != 0 {
		t.Fatalf("replay code=%d out=%s", code, out2)
	}
	ev := parseNDJSON(t, out2)

	wantTypes := []string{"session", "tool_use", "tool_result", "text_delta", "result"}
	if !equalSlice(eventTypes(ev), wantTypes) {
		t.Fatalf("replay 事件类型 = %v, want %v (out=%s)", eventTypes(ev), wantTypes, out2)
	}
	if ev[0]["id"] != id {
		t.Errorf("session id = %v, want %q", ev[0]["id"], id)
	}
	if ev[1]["name"] != "read" {
		t.Errorf("tool_use name = %v, want read", ev[1]["name"])
	}
	if ev[2]["call_id"] != "c1" || !strings.Contains(fmt.Sprint(ev[2]["output"]), "hello-target") {
		t.Errorf("tool_result = %+v", ev[2])
	}
	if ev[3]["text"] != "done" {
		t.Errorf("text_delta = %+v", ev[3])
	}
	if ev[4]["text"] != "done" || ev[4]["steps"] != float64(2) || ev[4]["finish"] != "stop" || ev[4]["model"] != "p/m" {
		t.Errorf("result = %+v", ev[4])
	}
}

// -replay 与 -session 互斥 → 报错退出 1。
func TestCLI_ReplayMutex(t *testing.T) {
	sessionDir := t.TempDir()
	cfgPath := writeSessionConfig(t, "http://127.0.0.1:1", sessionDir)
	code, out := runMainBin(t, "", []string{"-config", cfgPath, "-replay", "x", "-session", "y"}, "MINIAGENT_API_KEY=sk-test")
	if code != 1 {
		t.Errorf("code=%d want 1", code)
	}
	if !strings.Contains(out, "-replay") || !strings.Contains(out, "互斥") {
		t.Errorf("missing mutex error: %s", out)
	}
}

// -replay 指向不存在的会话 → 报错退出 1（防 typo 静默成功）。
func TestCLI_ReplayMissingExits1(t *testing.T) {
	sessionDir := t.TempDir()
	cfgPath := writeSessionConfig(t, "http://127.0.0.1:1", sessionDir)
	code, out := runMainBin(t, "", []string{"-config", cfgPath, "-replay", "nope"}, "MINIAGENT_API_KEY=sk-test")
	if code != 1 {
		t.Errorf("code=%d want 1", code)
	}
	if !strings.Contains(out, "不存在") {
		t.Errorf("missing not-exist error: %s", out)
	}
}

// eventTypes 抽取事件列表的 type 字段（断言用）。
func eventTypes(events []map[string]any) []string {
	types := make([]string, 0, len(events))
	for _, e := range events {
		if t, ok := e["type"].(string); ok {
			types = append(types, t)
		}
	}
	return types
}
