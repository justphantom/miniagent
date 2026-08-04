package miniagent

import (
	"encoding/json"
	"strings"
	"testing"
)

// dedupReadResults（P6）：同 (path,offset) 保留最后一次，更早压占位；不同 offset 不合并；
// 保留窗口内不动；无 read 零拷贝；不改调用方输入。
func TestDedupReadResults(t *testing.T) {
	msgs := []Message{
		{Role: "user", Content: "q"},                                                      // 0
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "r1", Name: "read", Args: `{"path":"a.go"}`}}}, // 1
		{Role: "tool", ToolCallID: "r1", Content: "first read"},                           // 2
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "r2", Name: "read", Args: `{"path":"a.go"}`}}}, // 3
		{Role: "tool", ToolCallID: "r2", Content: "second read"},                          // 4
	}
	out := dedupReadResults(msgs, 1) // 保留最近 1 条 assistant（idx3）
	if out[2].Content == "first read" {
		t.Errorf("旧 read 结果应被压占位，got %q", out[2].Content)
	}
	if !strings.Contains(out[2].Content, "已被更新的读取取代") || !strings.Contains(out[2].Content, "a.go") {
		t.Errorf("旧 read 占位应含路径与取代说明，got %q", out[2].Content)
	}
	if out[4].Content != "second read" {
		t.Errorf("窗口内最新 read 应保留原文，got %q", out[4].Content)
	}
	if msgs[2].Content != "first read" {
		t.Errorf("调用方输入被修改")
	}

	// 不同 offset 不合并。
	diff := []Message{
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "r1", Name: "read", Args: `{"path":"a.go","offset":1}`}}},
		{Role: "tool", ToolCallID: "r1", Content: "seg1"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "r2", Name: "read", Args: `{"path":"a.go","offset":100}`}}},
		{Role: "tool", ToolCallID: "r2", Content: "seg100"},
	}
	if got := dedupReadResults(diff, 1); got[1].Content != "seg1" {
		t.Errorf("不同 offset 的同 path read 不应合并，got %q", got[1].Content)
	}

	// 无 read → 零拷贝。
	plain := []Message{{Role: "user", Content: "q"}}
	if got := dedupReadResults(plain, 1); &got[0] != &plain[0] {
		t.Errorf("无 read 会话应原样返回同一 slice")
	}
	// 全在保留窗口内 → 零拷贝。
	if got := dedupReadResults(msgs, 10); &got[0] != &msgs[0] {
		t.Errorf("全在窗口内应原样返回")
	}
}

// foldStaleWriteEditArgs（P8'）：同 path 更晚成功 write/edit 触发折叠更早 args；
// 更晚失败不触发；不同 path 不互折叠；单条/全窗口零拷贝；不改调用方输入。
func TestFoldStaleWriteEditArgs(t *testing.T) {
	// 两次成功 write 同 path：更早折叠。
	msgs := []Message{
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "w1", Name: "write", Args: `{"path":"a.go","content":"old"}`}}},
		{Role: "tool", ToolCallID: "w1", Content: "ok"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "w2", Name: "write", Args: `{"path":"a.go","content":"new"}`}}},
		{Role: "tool", ToolCallID: "w2", Content: "ok"},
	}
	out := foldStaleWriteEditArgs(msgs, 1) // 保留最近 1 条 assistant（idx2）
	if !strings.Contains(out[0].ToolCalls[0].Args, "已被后续对同文件的成功写入取代") {
		t.Errorf("被后续成功写入取代的旧 write args 应折叠，got %q", out[0].ToolCalls[0].Args)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(out[0].ToolCalls[0].Args), &m); err != nil {
		t.Errorf("折叠后 args 非合法 JSON: %q (err %v)", out[0].ToolCalls[0].Args, err)
	}
	if m["path"] != "a.go" {
		t.Errorf("折叠后应保留 path，got %v", m["path"])
	}
	if out[2].ToolCalls[0].Args != msgs[2].ToolCalls[0].Args {
		t.Errorf("窗口内最新 write args 应保留原文")
	}
	if !strings.Contains(msgs[0].ToolCalls[0].Args, "old") {
		t.Errorf("调用方输入被修改")
	}

	// 更晚的失败 write 不触发折叠（前置正文仍有效）。
	failLater := []Message{
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "w1", Name: "write", Args: `{"path":"a.go","content":"v1"}`}}},
		{Role: "tool", ToolCallID: "w1", Content: "ok", IsError: false},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "w2", Name: "write", Args: `{"path":"a.go","content":"v2"}`}}},
		{Role: "tool", ToolCallID: "w2", Content: "err", IsError: true},
	}
	if got := foldStaleWriteEditArgs(failLater, 1); got[0].ToolCalls[0].Args != failLater[0].ToolCalls[0].Args {
		t.Errorf("更晚的失败写入不应触发折叠更早 args")
	}

	// 不同 path 不互折叠。
	diffPath := []Message{
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "w1", Name: "write", Args: `{"path":"a.go","content":"x"}`}}},
		{Role: "tool", ToolCallID: "w1", Content: "ok"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "w2", Name: "write", Args: `{"path":"b.go","content":"y"}`}}},
		{Role: "tool", ToolCallID: "w2", Content: "ok"},
	}
	if got := foldStaleWriteEditArgs(diffPath, 1); got[0].ToolCalls[0].Args != diffPath[0].ToolCalls[0].Args {
		t.Errorf("不同 path 的 write 不应互相折叠")
	}

	// 单条 write → 零拷贝。
	single := []Message{{Role: "assistant", ToolCalls: []ToolCall{{ID: "w1", Name: "write", Args: `{"path":"a.go","content":"x"}`}}}}
	if got := foldStaleWriteEditArgs(single, 0); &got[0] != &single[0] {
		t.Errorf("单条 write 会话应原样返回")
	}
	// 全在保留窗口内 → 零拷贝。
	if got := foldStaleWriteEditArgs(msgs, 10); &got[0] != &msgs[0] {
		t.Errorf("全在窗口内应原样返回")
	}
}

// dedupShellCommands（P9b）：规范化同义 command 去重，保留最后一次；不同命令不合并；
// 无 shell 零拷贝；全窗口零拷贝；不改调用方输入。
func TestDedupShellCommands(t *testing.T) {
	msgs := []Message{
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "s1", Name: "shell", Args: `{"command":"ls -la"}`}}},
		{Role: "tool", ToolCallID: "s1", Content: "out1"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "s2", Name: "shell", Args: `{"command":"LS  -la"}`}}}, // 规范化同义
		{Role: "tool", ToolCallID: "s2", Content: "out2"},
	}
	out := dedupShellCommands(msgs, 1)
	if !strings.Contains(out[0].ToolCalls[0].Args, "已被后续执行取代") {
		t.Errorf("同义 shell command 的更早调用应折叠，got %q", out[0].ToolCalls[0].Args)
	}
	if out[2].ToolCalls[0].Args != msgs[2].ToolCalls[0].Args {
		t.Errorf("窗口内最新 shell command 应保留原文")
	}
	if !strings.Contains(msgs[0].ToolCalls[0].Args, "ls -la") {
		t.Errorf("调用方输入被修改")
	}

	// 不同命令不合并。
	diff := []Message{
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "s1", Name: "shell", Args: `{"command":"pwd"}`}}},
		{Role: "tool", ToolCallID: "s1", Content: "/"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "s2", Name: "shell", Args: `{"command":"ls"}`}}},
		{Role: "tool", ToolCallID: "s2", Content: "f"},
	}
	if got := dedupShellCommands(diff, 1); got[0].ToolCalls[0].Args != diff[0].ToolCalls[0].Args {
		t.Errorf("不同 shell 命令不应合并")
	}

	// 无 shell → 零拷贝。
	plain := []Message{{Role: "user", Content: "q"}}
	if got := dedupShellCommands(plain, 1); &got[0] != &plain[0] {
		t.Errorf("无 shell 会话应原样返回")
	}
	// 全在保留窗口内 → 零拷贝。
	if got := dedupShellCommands(msgs, 10); &got[0] != &msgs[0] {
		t.Errorf("全在窗口内应原样返回")
	}
}
