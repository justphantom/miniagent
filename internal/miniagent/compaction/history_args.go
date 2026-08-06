package compaction

import (
	"github.com/justphantom/miniagent/internal/miniagent"
	"github.com/justphantom/miniagent/internal/text"
)

import (
	"encoding/json"
	"slices"
)

// stripStaleToolArgs（P4）对非最近 keepN 条 assistant 的 write/edit tool_call，把超大 Args
// （content/old_string/new_string）压缩为「前缀 + 省略标记」。与 stripStaleReasoning 同构：
// 仅改 context 侧拷贝，不碰 newMsgs/session，不动 ToolCallID（tool_calls/tool 配对完整）。
//
// 现状盲区：handleToolCalls 把 tool_call.Args 原样入历史、每轮全量回灌，而 tool 结果有 trimForHistory
// 三级裁剪、tool 参数零裁剪——不对称。write 的 content（≤10MiB）、edit 的 old/new_string 写成功后
// 已落盘或已被替换（old_string 在文件中已不存在），历史里那份纯占位重发；保留 path 让模型知道改过
// 哪个文件。仅当某字段超 toolArgsCompressThreshold 才压（小改动不动）；read/grep 等小 args 工具不压。
// 无可压缩项（非 write/edit 会话、或大 args 全在保留窗口内）时原样返回（零拷贝）。
func stripStaleToolArgs(msgs []miniagent.Message, keepN int) []miniagent.Message {
	if keepN < 0 {
		keepN = 0
	}
	// 预扫：从后往前跳过最近 keepN 条 assistant，检查更早的 assistant 是否有可压缩 tool_call。
	kept := 0
	hasCompressible := false
	for i := range slices.Backward(msgs) {
		if msgs[i].Role != miniagent.RoleAssistant {
			continue
		}
		if kept < keepN {
			kept++
			continue
		}
		for _, tc := range msgs[i].ToolCalls {
			if isLargeArgTool(tc.Name) && hasCompressibleArg(tc.Args) {
				hasCompressible = true
				break
			}
		}
		if hasCompressible {
			break
		}
	}
	if !hasCompressible {
		return msgs
	}
	out := make([]miniagent.Message, len(msgs))
	copy(out, msgs)
	kept = 0
	for i := range slices.Backward(out) {
		if out[i].Role != miniagent.RoleAssistant {
			continue
		}
		if kept < keepN {
			kept++
			continue
		}
		// 仅当该 assistant 含可压缩 tool_call 才处理。深拷贝其 ToolCalls slice 再改——
		// copy(msgs) 是浅拷贝，ToolCalls 底层 array 与调用方输入（msgs/History/newMsgs）共享，
		// 原地改元素会污染持久化层（违背「仅改 context 侧拷贝」）。
		compressible := false
		for _, tc := range out[i].ToolCalls {
			if isLargeArgTool(tc.Name) && hasCompressibleArg(tc.Args) {
				compressible = true
				break
			}
		}
		if !compressible {
			continue
		}
		calls := make([]miniagent.ToolCall, len(out[i].ToolCalls))
		copy(calls, out[i].ToolCalls)
		for j := range calls {
			if isLargeArgTool(calls[j].Name) {
				calls[j].Args = compressToolArgs(calls[j].Args)
			}
		}
		out[i].ToolCalls = calls
	}
	return out
}

// isLargeArgTool 判定工具是否携带可能超大的写入参数（write/edit）。其余工具 args 体积小，不压。
func isLargeArgTool(name string) bool {
	return name == "write" || name == "edit"
}

// hasCompressibleArg 判定 args JSON 是否含超过 toolArgsCompressThreshold 的大字段。
func hasCompressibleArg(args string) bool {
	var m map[string]any
	if json.Unmarshal([]byte(args), &m) != nil {
		return false
	}
	if hasLargeStringField(m, "content", "old_string", "new_string") {
		return true
	}
	if edits, ok := m["edits"].([]any); ok {
		for _, e := range edits {
			if em, ok := e.(map[string]any); ok && hasLargeStringField(em, "old_string", "new_string") {
				return true
			}
		}
	}
	return false
}

// hasLargeStringField 判定 m 中任一指定 key 的字符串值是否超过压缩阈值。
func hasLargeStringField(m map[string]any, keys ...string) bool {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && len([]rune(s)) > toolArgsCompressThreshold {
			return true
		}
	}
	return false
}

// compressToolArgs 把 args JSON 里超过阈值的大字段压缩为「前缀 + 省略标记」。解析或序列化失败、
// 或无任何字段超阈值（无需压缩）时原样返回——绝不破坏 JSON 合法性（否则配对/wire 出错），也避免
// 无谓重序列化（Go map 按 key 字典序输出，会改变 key 顺序引入噪声）。path 始终保留，让模型知道改过哪个文件。
func compressToolArgs(args string) string {
	var m map[string]any
	if json.Unmarshal([]byte(args), &m) != nil {
		return args
	}
	changed := false
	compressField := func(v any) (any, bool) {
		s, ok := v.(string)
		if !ok || len([]rune(s)) <= toolArgsCompressThreshold {
			return v, false
		}
		return text.Truncate(s, toolArgsKeepChars, "…[参数已省略]"), true
	}
	for _, k := range []string{"content", "old_string", "new_string"} {
		if _, ok := m[k]; ok {
			nv, c := compressField(m[k])
			m[k] = nv
			if c {
				changed = true
			}
		}
	}
	if edits, ok := m["edits"].([]any); ok {
		for _, e := range edits {
			if em, ok := e.(map[string]any); ok {
				for _, k := range []string{"old_string", "new_string"} {
					if _, ok := em[k]; ok {
						nv, c := compressField(em[k])
						em[k] = nv
						if c {
							changed = true
						}
					}
				}
			}
		}
	}
	if !changed {
		return args
	}
	b, err := json.Marshal(m)
	if err != nil {
		return args
	}
	return string(b)
}
