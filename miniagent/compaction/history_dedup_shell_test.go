package compaction

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/justphantom/miniagent/miniagent"
	"github.com/justphantom/miniagent/miniagent/policy"
)

func TestDedupShellCommands(t *testing.T) {
	msgs := []miniagent.Message{
		{Role: "assistant", ToolCalls: []miniagent.ToolCall{{ID: "s1", Name: "shell", Args: `{"command":"ls -la"}`}}},
		{Role: "tool", ToolCallID: "s1", Content: "out1"},
		{Role: "assistant", ToolCalls: []miniagent.ToolCall{{ID: "s2", Name: "shell", Args: `{"command":"LS  -la"}`}}}, // normalized synonym
		{Role: "tool", ToolCallID: "s2", Content: "out2"},
	}
	out := dedupShellCommands(msgs, 1)
	if !strings.Contains(out[0].ToolCalls[0].Args, "superseded by a later execution") {
		t.Errorf("earlier synonymous shell command should be folded, got %q", out[0].ToolCalls[0].Args)
	}
	if out[2].ToolCalls[0].Args != msgs[2].ToolCalls[0].Args {
		t.Errorf("the newest shell command inside the window should keep its original text")
	}
	if !strings.Contains(msgs[0].ToolCalls[0].Args, "ls -la") {
		t.Errorf("caller input was modified")
	}

	// different commands are not merged.
	diff := []miniagent.Message{
		{Role: "assistant", ToolCalls: []miniagent.ToolCall{{ID: "s1", Name: "shell", Args: `{"command":"pwd"}`}}},
		{Role: "tool", ToolCallID: "s1", Content: "/"},
		{Role: "assistant", ToolCalls: []miniagent.ToolCall{{ID: "s2", Name: "shell", Args: `{"command":"ls"}`}}},
		{Role: "tool", ToolCallID: "s2", Content: "f"},
	}
	if got := dedupShellCommands(diff, 1); got[0].ToolCalls[0].Args != diff[0].ToolCalls[0].Args {
		t.Errorf("different shell commands should not be merged")
	}

	// no shell → zero-copy.
	plain := []miniagent.Message{{Role: "user", Content: "q"}}
	if got := dedupShellCommands(plain, 1); &got[0] != &plain[0] {
		t.Errorf("session without shell should return as-is")
	}
	// all inside the retention window → zero-copy.
	if got := dedupShellCommands(msgs, 10); &got[0] != &msgs[0] {
		t.Errorf("all inside window should return as-is")
	}
}

func TestDedupShellCommands_FoldsResultContent(t *testing.T) {
	// same shell command run 3 times; each run's output differs (re-running is the point).
	msgs := []miniagent.Message{
		{Role: "assistant", ToolCalls: []miniagent.ToolCall{{ID: "s1", Name: "shell", Args: `{"command":"go test ./..."}`}}}, // 0
		{Role: "tool", ToolCallID: "s1", Content: "run1 output (FAIL: pkg_a)"},                                               // 1
		{Role: "assistant", ToolCalls: []miniagent.ToolCall{{ID: "s2", Name: "shell", Args: `{"command":"go test ./..."}`}}}, // 2
		{Role: "tool", ToolCallID: "s2", Content: "run2 output (FAIL: pkg_b)"},                                               // 3
		{Role: "assistant", ToolCalls: []miniagent.ToolCall{{ID: "s3", Name: "shell", Args: `{"command":"go test ./..."}`}}}, // 4
		{Role: "tool", ToolCallID: "s3", Content: "run3 output (ok)"},                                                        // 5
	}
	out := dedupShellCommands(msgs, 1) // retain the most recent 1 assistant (idx4) → windowStart=4
	// LAST result stays verbatim.
	if out[5].Content != "run3 output (ok)" {
		t.Errorf("last shell result should stay verbatim, got %q", out[5].Content)
	}
	// EARLIER results are placeholdered (foldedIDs={s1,s2}; s3 stays verbatim).
	for _, i := range []int{1, 3} {
		if out[i].Content == msgs[i].Content {
			t.Errorf("earlier shell result at idx %d should be folded, got %q", i, out[i].Content)
		}
		if !strings.Contains(out[i].Content, "superseded by a later execution") {
			t.Errorf("folded shell result at idx %d should contain the superseded marker, got %q", i, out[i].Content)
		}
	}
	// args folding (existing behavior) still applies to the earlier commands.
	for _, i := range []int{0, 2} {
		if !strings.Contains(out[i].ToolCalls[0].Args, "superseded by a later execution") {
			t.Errorf("earlier shell args at assistant idx %d should be folded, got %q", i, out[i].ToolCalls[0].Args)
		}
	}
	// the last command (inside the window) keeps its original args.
	if !strings.Contains(out[4].ToolCalls[0].Args, "go test") {
		t.Errorf("last shell command args inside the window should stay verbatim, got %q", out[4].ToolCalls[0].Args)
	}
	// pairing unchanged: each tool_call_id still has a role=tool message with a matching id.
	paired := map[string]bool{"s1": false, "s2": false, "s3": false}
	for _, m := range out {
		if m.Role == miniagent.RoleTool {
			if _, ok := paired[m.ToolCallID]; ok {
				paired[m.ToolCallID] = true
			}
		}
	}
	for id, ok := range paired {
		if !ok {
			t.Errorf("role=tool pairing lost for tool_call_id %s", id)
		}
	}
	// caller input not modified.
	if msgs[1].Content != "run1 output (FAIL: pkg_a)" {
		t.Errorf("caller input was modified")
	}
}

func TestApplyContextStrips_Debug(t *testing.T) {
	big := strings.Repeat("x", 4000)
	msgs := []miniagent.Message{
		{Role: "user", Content: "q"},
		{Role: "assistant", ToolCalls: []miniagent.ToolCall{{ID: "r1", Name: "read", Args: `{"path":"a.go"}`}}},
		{Role: "tool", ToolCallID: "r1", Content: big},
		{Role: "assistant", ToolCalls: []miniagent.ToolCall{{ID: "w1", Name: "write", Args: `{"path":"a.go","content":"` + big + `"}`}}},
		{Role: "tool", ToolCallID: "w1", Content: "ok"},
		{Role: "assistant", Content: "done1"},
		{Role: "assistant", Content: "done2"},
	}
	// Debug level: P11 should fold read r1 (superseded by write w1, outside the window); the log contains stage + saved_tokens + fit done.
	var buf bytes.Buffer
	dbg := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	applyContextStrips(context.Background(), msgs, 1, 0, 2, dbg, policy.EstimateTokens)
	logs := buf.String()
	if !strings.Contains(logs, "P11_foldRead") || !strings.Contains(logs, "saved_tokens") {
		t.Errorf("Debug log should contain P11_foldRead and saved_tokens, got:\n%s", logs)
	}
	if !strings.Contains(logs, "fit done") {
		t.Errorf("Debug log should contain the fit done summary")
	}
	// Info level: no strip saved log (zero overhead).
	var buf2 bytes.Buffer
	info := slog.New(slog.NewTextHandler(&buf2, &slog.HandlerOptions{Level: slog.LevelInfo}))
	applyContextStrips(context.Background(), msgs, 1, 0, 2, info, func(m []miniagent.Message, _ string, _ []miniagent.Tool) int { return 0 })
	if strings.Contains(buf2.String(), "strip saved") {
		t.Errorf("Info level should not output strip saved")
	}
}
