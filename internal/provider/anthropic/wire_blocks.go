package anthropic

import (
	"encoding/json"
	"slices"
	"strings"

	"github.com/justphantom/miniagent/internal/miniagent"
)

// cacheControl is the ephemeral cache-breakpoint marker attached to a content block (5-minute default TTL).
var cacheControl = map[string]any{"type": "ephemeral"}

// systemBlocks projects Request.System to the top-level system field as a text-block array (array shape so
// a cache_control breakpoint can be attached when caching is enabled; Anthropic also accepts a bare string,
// but the array form is uniform with the cache path).
func systemBlocks(text string, cache bool) []map[string]any {
	blk := map[string]any{"type": "text", "text": text}
	if cache {
		blk["cache_control"] = cacheControl
	}
	return []map[string]any{blk}
}

// toolBlocks projects Tools to Anthropic's tool schema: {name, description, input_schema} (no function
// wrapper; the core Parameters map becomes input_schema verbatim).
func toolBlocks(tools []miniagent.Tool) []map[string]any {
	arr := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		arr = append(arr, map[string]any{
			"name":         t.Name,
			"description":  t.Description,
			"input_schema": t.Parameters,
		})
	}
	return arr
}

// projectMessages projects the flat core Message list to Anthropic's role+content-block model:
//   - system is handled at the top level (skipped here);
//   - consecutive role=tool messages are merged into ONE user message carrying multiple tool_result blocks
//     (splitting them would train the model to stop parallel tool use);
//   - an assistant's Content+ToolCalls are re-ordered into [text_block?, tool_use_block...];
//   - historical Reasoning is DROPPED: the core has no thinking signature, and replaying a thinking block
//     without its signature is rejected by the server (plan §2.5). The current turn's Reasoning still flows
//     to the consumer via streaming/Response — only historical replay is lossy.
func projectMessages(msgs []miniagent.Message, cache bool) []map[string]any {
	lastUser := -1
	for i := range slices.Backward(msgs) {
		if msgs[i].Role == miniagent.RoleUser {
			lastUser = i
			break
		}
	}
	var out []map[string]any
	var pendingTool []map[string]any
	flush := func() {
		if len(pendingTool) > 0 {
			out = append(out, map[string]any{"role": "user", "content": pendingTool})
			pendingTool = nil
		}
	}
	for i, m := range msgs {
		switch m.Role {
		case miniagent.RoleSystem:
			// handled at the top level (buildBody)
		case miniagent.RoleTool:
			blk := map[string]any{
				"type":        "tool_result",
				"tool_use_id": m.ToolCallID,
				"content":     m.Content,
			}
			if m.IsError {
				blk["is_error"] = true
			}
			pendingTool = append(pendingTool, blk)
		case miniagent.RoleUser:
			flush()
			blk := map[string]any{"type": "text", "text": m.Content}
			if cache && i == lastUser {
				blk["cache_control"] = cacheControl
			}
			out = append(out, map[string]any{"role": "user", "content": []map[string]any{blk}})
		case miniagent.RoleAssistant:
			flush()
			var blocks []map[string]any
			if m.Content != "" {
				blocks = append(blocks, map[string]any{"type": "text", "text": m.Content})
			}
			for _, tc := range m.ToolCalls {
				blocks = append(blocks, map[string]any{
					"type":  "tool_use",
					"id":    tc.ID,
					"name":  tc.Name,
					"input": argsToObject(tc.Args),
				})
			}
			if len(blocks) == 0 {
				// Anthropic requires a non-empty content array; an assistant turn with neither text nor
				// tool_use is anomalous, emit an empty text block rather than risk a 400 on an empty array.
				blocks = []map[string]any{{"type": "text", "text": ""}}
			}
			out = append(out, map[string]any{"role": "assistant", "content": blocks})
		}
	}
	flush()
	return out
}

// argsToObject projects a ToolCall.Args JSON string to an Anthropic tool_use input object. Empty or invalid
// JSON falls back to {} — the wire must carry an object, never a string (a string is rejected by the API).
func argsToObject(args string) json.RawMessage {
	args = strings.TrimSpace(args)
	if args == "" {
		return json.RawMessage("{}")
	}
	var obj map[string]any
	if json.Unmarshal([]byte(args), &obj) != nil {
		return json.RawMessage("{}")
	}
	return json.RawMessage(args)
}

// argsToString is the inverse of argsToObject: a tool_use input object (json.RawMessage) back to the core
// ToolCall.Args string. Empty/invalid falls back to "{}".
func argsToString(raw json.RawMessage) string {
	if len(strings.TrimSpace(string(raw))) == 0 {
		return "{}"
	}
	var obj map[string]any
	if json.Unmarshal(raw, &obj) != nil {
		return "{}"
	}
	return string(raw)
}

// resolveThinking renders the thinking field from the provider ThinkingMapping, WITHOUT changing the core
// ThinkingMapping{field,map} type. Map[level] holds a JSON OBJECT string (e.g. {"type":"enabled","budget_tokens":N}
// for Claude ≤4.5, or {"type":"adaptive","effort":"high"} for Claude 4.7+). The "effort" key is split into
// output_config (the adaptive-mode effort knob); the remainder becomes the top-level thinking object.
// Unmapped level / invalid JSON → thinking is not sent (defensive; isThinkingError catches model-family
// mismatch at the server). ThinkingMapping.Field is a placeholder "thinking" here — the wire key is hardcoded.
func resolveThinking(payload map[string]any, req miniagent.Request) {
	if req.ThinkingLevel == "" || req.ThinkingLevel == miniagent.ThinkingOff || req.Thinking == nil {
		return
	}
	raw, ok := req.Thinking.Map[req.ThinkingLevel]
	if !ok {
		return
	}
	var obj map[string]any
	if json.Unmarshal([]byte(raw), &obj) != nil {
		return
	}
	if eff, has := obj["effort"]; has {
		payload["output_config"] = map[string]any{"effort": eff}
		delete(obj, "effort")
	}
	payload["thinking"] = obj
}

// mapStopReason maps Anthropic stop_reason to the core FinishReason vocabulary (matches the openai values
// the core loop already branches on: stop / tool_calls / length).
func mapStopReason(s string) string {
	switch s {
	case "end_turn", "stop_sequence":
		return "stop"
	case "tool_use":
		return "tool_calls"
	case "max_tokens":
		return "length"
	}
	return s
}
