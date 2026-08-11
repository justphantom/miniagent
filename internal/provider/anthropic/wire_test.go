package anthropic

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/justphantom/miniagent/internal/miniagent"
)

func TestBuildBody_SystemAsTextBlockArrayAndMaxTokens(t *testing.T) {
	req := miniagent.Request{Model: "claude-sonnet-5", System: "You are helpful.", MaxTokens: 1024, Messages: []miniagent.Message{{Role: miniagent.RoleUser, Content: "hi"}}}
	body, err := buildBody(req, false)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	sys, ok := m["system"].([]any)
	if !ok || len(sys) != 1 {
		t.Fatalf("system = %v, want [1 text block]", m["system"])
	}
	blk := sys[0].(map[string]any)
	if blk["type"] != "text" || blk["text"] != "You are helpful." {
		t.Errorf("system block = %v", blk)
	}
	if _, has := blk["cache_control"]; has {
		t.Error("system block should not have cache_control when cache=false")
	}
	if v, ok := m["max_tokens"].(float64); !ok || v != 1024 {
		t.Errorf("max_tokens = %v, want 1024 (always written)", m["max_tokens"])
	}
	if _, has := m["stream_options"]; has {
		t.Error("stream_options must never be written")
	}
}

func TestBuildBody_SystemCacheControlWhenCaching(t *testing.T) {
	req := miniagent.Request{Model: "m", System: "sys", MaxTokens: 1, Messages: []miniagent.Message{{Role: miniagent.RoleUser, Content: "x"}}}
	body, _ := buildBody(req, true)
	var m map[string]any
	json.Unmarshal(body, &m)
	blk := m["system"].([]any)[0].(map[string]any)
	cc, ok := blk["cache_control"].(map[string]any)
	if !ok || cc["type"] != "ephemeral" {
		t.Errorf("cache_control = %v, want ephemeral", blk["cache_control"])
	}
}

func TestBuildBody_LastUserGetsCacheBreakpointOnly(t *testing.T) {
	req := miniagent.Request{Model: "m", MaxTokens: 1, Messages: []miniagent.Message{
		{Role: miniagent.RoleUser, Content: "first"},
		{Role: miniagent.RoleAssistant, Content: "ok"},
		{Role: miniagent.RoleUser, Content: "second"},
	}}
	body, _ := buildBody(req, true)
	var m map[string]any
	json.Unmarshal(body, &m)
	msgs := m["messages"].([]any)
	last := msgs[len(msgs)-1].(map[string]any)
	if last["role"] != "user" {
		t.Fatalf("last msg role = %v", last["role"])
	}
	lblk := last["content"].([]any)[0].(map[string]any)
	if _, ok := lblk["cache_control"]; !ok {
		t.Error("last user message should carry cache_control when caching")
	}
	first := msgs[0].(map[string]any)
	fblk := first["content"].([]any)[0].(map[string]any)
	if _, ok := fblk["cache_control"]; ok {
		t.Error("non-last user message should not carry cache_control")
	}
}

func TestBuildBody_AssistantTextThenToolUseOrder(t *testing.T) {
	req := miniagent.Request{Model: "m", MaxTokens: 1, Messages: []miniagent.Message{
		{Role: miniagent.RoleUser, Content: "q"},
		{Role: miniagent.RoleAssistant, Content: "out loud", ToolCalls: []miniagent.ToolCall{{ID: "t1", Name: "search", Args: `{"q":"x"}`}}},
	}}
	body, _ := buildBody(req, false)
	var m map[string]any
	json.Unmarshal(body, &m)
	msgs := m["messages"].([]any)
	asst := msgs[1].(map[string]any)
	blocks := asst["content"].([]any)
	if len(blocks) != 2 {
		t.Fatalf("blocks = %d, want 2 (text then tool_use)", len(blocks))
	}
	if blocks[0].(map[string]any)["type"] != "text" {
		t.Errorf("block[0] = %v, want text", blocks[0])
	}
	tu := blocks[1].(map[string]any)
	if tu["type"] != "tool_use" || tu["id"] != "t1" || tu["name"] != "search" {
		t.Errorf("tool_use block = %v", tu)
	}
	if _, ok := tu["input"].(map[string]any); !ok {
		t.Errorf("tool_use input = %T, want object (not string)", tu["input"])
	}
}

func TestBuildBody_ConsecutiveToolMessagesMerged(t *testing.T) {
	req := miniagent.Request{Model: "m", MaxTokens: 1, Messages: []miniagent.Message{
		{Role: miniagent.RoleUser, Content: "q"},
		{Role: miniagent.RoleAssistant, ToolCalls: []miniagent.ToolCall{{ID: "a", Name: "f", Args: "{}"}, {ID: "b", Name: "g", Args: "{}"}}},
		{Role: miniagent.RoleTool, ToolCallID: "a", Content: "ra"},
		{Role: miniagent.RoleTool, ToolCallID: "b", Content: "rb"},
	}}
	body, _ := buildBody(req, false)
	var m map[string]any
	json.Unmarshal(body, &m)
	msgs := m["messages"].([]any)
	tr := msgs[len(msgs)-1].(map[string]any)
	if tr["role"] != "user" {
		t.Fatalf("merged tool_result role = %v, want user", tr["role"])
	}
	if len(tr["content"].([]any)) != 2 {
		t.Errorf("merged tool_result blocks = %d, want 2 (consecutive tool msgs in ONE user)", len(tr["content"].([]any)))
	}
}

func TestBuildBody_DropsReasoningOnReplay(t *testing.T) {
	req := miniagent.Request{Model: "m", MaxTokens: 1, Messages: []miniagent.Message{
		{Role: miniagent.RoleUser, Content: "q"},
		{Role: miniagent.RoleAssistant, Content: "ans", Reasoning: "secret chain of thought"},
	}}
	body, _ := buildBody(req, false)
	if strings.Contains(string(body), "secret chain of thought") {
		t.Error("historical Reasoning must be dropped (no signature to replay); body leaked it")
	}
}

func TestBuildBody_ToolResultIsErrorPropagated(t *testing.T) {
	req := miniagent.Request{Model: "m", MaxTokens: 1, Messages: []miniagent.Message{
		{Role: miniagent.RoleUser, Content: "q"},
		{Role: miniagent.RoleAssistant, ToolCalls: []miniagent.ToolCall{{ID: "a", Name: "f", Args: "{}"}}},
		{Role: miniagent.RoleTool, ToolCallID: "a", Content: "boom", IsError: true},
	}}
	body, _ := buildBody(req, false)
	var m map[string]any
	json.Unmarshal(body, &m)
	msgs := m["messages"].([]any)
	tr := msgs[len(msgs)-1].(map[string]any)
	blk := tr["content"].([]any)[0].(map[string]any)
	if blk["is_error"] != true {
		t.Errorf("is_error = %v, want true", blk["is_error"])
	}
}

func TestBuildBody_ThinkingAdaptiveSplitsEffort(t *testing.T) {
	req := miniagent.Request{Model: "m", MaxTokens: 1, Messages: []miniagent.Message{{Role: miniagent.RoleUser, Content: "q"}},
		ThinkingLevel: "high", Thinking: &miniagent.ThinkingMapping{Field: "thinking", Map: map[string]string{"high": `{"type":"adaptive","effort":"high"}`}}}
	body, _ := buildBody(req, false)
	var m map[string]any
	json.Unmarshal(body, &m)
	th := m["thinking"].(map[string]any)
	if th["type"] != "adaptive" {
		t.Errorf("thinking.type = %v, want adaptive", th["type"])
	}
	if _, has := th["effort"]; has {
		t.Error("effort must be split out of thinking into output_config")
	}
	if m["output_config"].(map[string]any)["effort"] != "high" {
		t.Errorf("output_config.effort = %v, want high", m["output_config"])
	}
}

func TestBuildBody_ThinkingEnabledBudgetNoOutputConfig(t *testing.T) {
	req := miniagent.Request{Model: "m", MaxTokens: 30000, Messages: []miniagent.Message{{Role: miniagent.RoleUser, Content: "q"}},
		ThinkingLevel: "high", Thinking: &miniagent.ThinkingMapping{Field: "thinking", Map: map[string]string{"high": `{"type":"enabled","budget_tokens":20000}`}}}
	body, _ := buildBody(req, false)
	var m map[string]any
	json.Unmarshal(body, &m)
	th := m["thinking"].(map[string]any)
	if th["type"] != "enabled" {
		t.Errorf("thinking.type = %v, want enabled", th["type"])
	}
	if bt, ok := th["budget_tokens"].(float64); !ok || bt != 20000 {
		t.Errorf("budget_tokens = %v, want 20000", th["budget_tokens"])
	}
	if _, has := m["output_config"]; has {
		t.Error("output_config should not be set for enabled-budget (no effort key)")
	}
}

func TestBuildBody_ThinkingOffNotWritten(t *testing.T) {
	req := miniagent.Request{Model: "m", MaxTokens: 1, Messages: []miniagent.Message{{Role: miniagent.RoleUser, Content: "q"}},
		ThinkingLevel: miniagent.ThinkingOff, Thinking: &miniagent.ThinkingMapping{Field: "thinking", Map: map[string]string{"high": `{"type":"adaptive","effort":"high"}`}}}
	body, _ := buildBody(req, false)
	var m map[string]any
	json.Unmarshal(body, &m)
	if _, has := m["thinking"]; has {
		t.Error("thinking must not be written when ThinkingLevel is off")
	}
}

func TestBuildBody_ToolsInputSchemaNoFunctionWrapper(t *testing.T) {
	req := miniagent.Request{Model: "m", MaxTokens: 1, Messages: []miniagent.Message{{Role: miniagent.RoleUser, Content: "q"}},
		Tools: []miniagent.Tool{{Name: "ls", Description: "list", Parameters: map[string]any{"type": "object"}}}}
	body, _ := buildBody(req, false)
	var m map[string]any
	json.Unmarshal(body, &m)
	t0 := m["tools"].([]any)[0].(map[string]any)
	if t0["name"] != "ls" || t0["input_schema"] == nil {
		t.Errorf("tool = %v, want name + input_schema", t0)
	}
	if _, has := t0["function"]; has {
		t.Error("tool must NOT use the openai function wrapper")
	}
}

func TestBuildBody_StreamTrueOnly(t *testing.T) {
	req := miniagent.Request{Model: "m", MaxTokens: 1, Stream: true, Messages: []miniagent.Message{{Role: miniagent.RoleUser, Content: "q"}}}
	body, _ := buildBody(req, false)
	var m map[string]any
	json.Unmarshal(body, &m)
	if m["stream"] != true {
		t.Errorf("stream = %v, want true", m["stream"])
	}
	if _, has := m["stream_options"]; has {
		t.Error("stream_options must not be written")
	}
}
