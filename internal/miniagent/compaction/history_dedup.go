package compaction

import "github.com/justphantom/miniagent/internal/miniagent"

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
)

// ─── P6 / P8' / P9b：tool 结果与 tool_call args 的跨消息去重/折叠 ───
//
// 三者均仅改 context 侧拷贝（不碰 newMsgs/session），复用 keepToolArgs 保留窗口
// （最近 N 条 assistant 及其 tool 消息不动），与 stripStaleToolArgs 同窗口语义。
// 裁剪触发条件统一为「同 key 更晚的出现已取代更早的」——更早的纯占位重发，压成占位零/低损失。

// windowStartOf 返回从后往前第 keepN 条 assistant 的 index（保留窗口起点：该 index 及之后不动）。
// keepN<=0 视作 0；assistant 不足 keepN 时返回 0（全部在窗口内，无可裁剪）。
func windowStartOf(msgs []miniagent.Message, keepN int) int {
	if keepN <= 0 {
		return 0
	}
	seen := 0
	for i := range slices.Backward(msgs) {
		if msgs[i].Role == miniagent.RoleAssistant {
			seen++
			if seen == keepN {
				return i
			}
		}
	}
	return 0
}

// argPath 从工具调用 raw JSON args 提取非空 path 字段（read/write/edit/glob 等路径类工具）。
// 解析失败或无 path 返回 ("", false)。
func argPath(args string) (string, bool) {
	var m map[string]any
	if json.Unmarshal([]byte(args), &m) != nil {
		return "", false
	}
	s, ok := m["path"].(string)
	if !ok || s == "" {
		return "", false
	}
	return s, true
}

// argReadOffset 提取 read 的 offset（默认 1）。P6 按 (path,offset) 分组——同 path 不同 offset
// 是文件不同段，不能互相覆盖（v5 §2.1 关键约束）。
func argReadOffset(args string) int {
	var m map[string]any
	if json.Unmarshal([]byte(args), &m) != nil {
		return 1
	}
	if f, ok := m["offset"].(float64); ok && f > 0 {
		return int(f)
	}
	return 1
}

// normalizePath 归一工具路径供跨调用比较：filepath.Clean 统一 "a/./b"、"./a"、"a//" 等。
// 不做 EvalSymlinks——history 阶段无 workspaceRoot，且同会话模型发出的路径基准一致，
// 相对路径 Clean 已覆盖绝大多数情形；绝对/相对混用罕见，按字面比较即可。
func normalizePath(p string) string {
	return filepath.Clean(p)
}

// shellCmdKey 提取 shell command 的规范化签名供 P9b 去重：按空白拆 token、小写、重 join。
// 使 "ls  -la"、"LS -la"、"ls -la" 视作同义。解析失败/无 command 返回 ("", false)。
func shellCmdKey(args string) (string, bool) {
	var m map[string]any
	if json.Unmarshal([]byte(args), &m) != nil {
		return "", false
	}
	s, ok := m["command"].(string)
	if !ok || s == "" {
		return "", false
	}
	fields := strings.Fields(s)
	for i := range fields {
		fields[i] = strings.ToLower(fields[i])
	}
	return strings.Join(fields, " "), true
}

// successWriteEditPaths 扫描 msgs，返回 path → 成功 write/edit 的 assistant msgIdx 升序列表。
// 成功 = 对应 tool 消息 IsError=false（失败写入不改文件，不计入）。供 P8'/P11 判定
// 「同 path 是否存在更晚的成功写入」——P8' 据此折叠更早的 write/edit args，P11 折叠更早的 read 结果。
func successWriteEditPaths(msgs []miniagent.Message) map[string][]int {
	isErrOf := map[string]bool{}
	for _, m := range msgs {
		if m.Role == miniagent.RoleTool && m.ToolCallID != "" {
			isErrOf[m.ToolCallID] = m.IsError
		}
	}
	succ := map[string][]int{}
	for i, m := range msgs {
		if m.Role != miniagent.RoleAssistant {
			continue
		}
		for _, tc := range m.ToolCalls {
			if tc.Name != "write" && tc.Name != "edit" {
				continue
			}
			if isErrOf[tc.ID] { // 失败写入不改文件，不计入「成功写入」
				continue
			}
			p, ok := argPath(tc.Args)
			if !ok {
				continue
			}
			succ[normalizePath(p)] = append(succ[normalizePath(p)], i)
		}
	}
	return succ
}

// foldStaleWriteEditArgs（P8'）对保留窗口外的 write/edit tool_call，若同 path 上存在更晚的
// 成功 write/edit，把其 args 折叠为「path + 占位」。成功判定经 successWriteEditPaths（依赖
// tool 消息 IsError）——失败写入不改文件，前置 write/edit 正文仍有效，不能折叠（v5 §2 P8' 与
// P4 的关键区别）。与 P4（compressToolArgs 压成前缀）互补：P4 减单条体积，P8' 在「被后续同 path
// 写入取代」时整条折叠。无可折叠项零拷贝；深拷贝被改 assistant 的 ToolCalls slice。
func foldStaleWriteEditArgs(msgs []miniagent.Message, keepN int) []miniagent.Message {
	succ := successWriteEditPaths(msgs)
	if len(succ) == 0 {
		return msgs
	}
	type we struct {
		msgIdx, tcIdx int
		path          string
	}
	var list []we
	for i, m := range msgs {
		if m.Role != miniagent.RoleAssistant {
			continue
		}
		for j, tc := range m.ToolCalls {
			if tc.Name != "write" && tc.Name != "edit" {
				continue
			}
			p, ok := argPath(tc.Args)
			if !ok {
				continue
			}
			list = append(list, we{i, j, normalizePath(p)})
		}
	}
	if len(list) < 2 {
		return msgs
	}
	windowStart := windowStartOf(msgs, keepN)
	type foldKey struct{ msgIdx, tcIdx int }
	toFold := map[foldKey]string{}
	for _, e := range list {
		if e.msgIdx >= windowStart {
			continue
		}
		for _, wIdx := range succ[e.path] {
			if wIdx > e.msgIdx { // 同 path 更晚的成功写入存在 → 本条 args 折叠
				toFold[foldKey{e.msgIdx, e.tcIdx}] = e.path
				break
			}
		}
	}
	if len(toFold) == 0 {
		return msgs
	}
	out := make([]miniagent.Message, len(msgs))
	copy(out, msgs)
	dirty := map[int]bool{}
	for fk := range toFold {
		dirty[fk.msgIdx] = true
	}
	for i := range out {
		if !dirty[i] {
			continue
		}
		calls := make([]miniagent.ToolCall, len(out[i].ToolCalls))
		copy(calls, out[i].ToolCalls)
		for j := range calls {
			if p, ok := toFold[foldKey{i, j}]; ok {
				calls[j].Args = foldedWriteEditArgs(p)
			}
		}
		out[i].ToolCalls = calls
	}
	return out
}

// foldedWriteEditArgs 构造 P8' 折叠后的 args：仅保留 path + 占位。历史里的旧 tool_call 不必
// 严格匹配工具 schema（端点不校验历史 args），模型只需知道「这个文件被改过」。
func foldedWriteEditArgs(path string) string {
	b, err := json.Marshal(map[string]any{
		"path":    path,
		"content": "…[此前的 write/edit 旧正文已被后续对同文件的成功写入取代]",
	})
	if err != nil {
		return `{"path":"` + path + `"}`
	}
	return string(b)
}

// dedupShellCommands（P9b）对保留窗口外的 shell tool_call，按规范化 command 签名去重：
// 每组保留时间序最后一次原文，更早的同义 command 折叠为占位。ReAct 会话高频重复探查命令
// （pwd && ls、find ... -name），更早的同义命令对后续决策无价值（v5 §3 P9b）。无需 IsError。
// 深拷贝被改 assistant 的 ToolCalls slice。
func dedupShellCommands(msgs []miniagent.Message, keepN int) []miniagent.Message {
	shellKeyOf := map[string]string{}
	for _, m := range msgs {
		if m.Role != miniagent.RoleAssistant {
			continue
		}
		for _, tc := range m.ToolCalls {
			if tc.Name != "shell" {
				continue
			}
			if k, ok := shellCmdKey(tc.Args); ok {
				shellKeyOf[tc.ID] = k
			}
		}
	}
	if len(shellKeyOf) == 0 {
		return msgs
	}
	windowStart := windowStartOf(msgs, keepN)
	type se struct {
		msgIdx, tcIdx, order int
		key                  string
	}
	var list []se
	order := 0
	for i, m := range msgs {
		if m.Role != miniagent.RoleAssistant {
			continue
		}
		for j, tc := range m.ToolCalls {
			k, ok := shellKeyOf[tc.ID]
			if !ok {
				continue
			}
			list = append(list, se{i, j, order, k})
			order++
		}
	}
	hasInWindow := map[string]bool{}
	lastOrder := map[string]int{}
	for _, e := range list {
		if e.msgIdx >= windowStart {
			hasInWindow[e.key] = true
		} else {
			lastOrder[e.key] = e.order
		}
	}
	type foldKey struct{ msgIdx, tcIdx int }
	toFold := map[foldKey]bool{}
	for _, e := range list {
		if e.msgIdx >= windowStart {
			continue
		}
		if hasInWindow[e.key] || lastOrder[e.key] > e.order {
			toFold[foldKey{e.msgIdx, e.tcIdx}] = true
		}
	}
	if len(toFold) == 0 {
		return msgs
	}
	out := make([]miniagent.Message, len(msgs))
	copy(out, msgs)
	dirty := map[int]bool{}
	for fk := range toFold {
		dirty[fk.msgIdx] = true
	}
	for i := range out {
		if !dirty[i] {
			continue
		}
		calls := make([]miniagent.ToolCall, len(out[i].ToolCalls))
		copy(calls, out[i].ToolCalls)
		for j := range calls {
			if toFold[foldKey{i, j}] {
				// json.Marshal 构造占位（与 foldedWriteEditArgs 一致），防硬编码 JSON 因文案引入引号/反斜杠而断裂。
				placeholder, _ := json.Marshal(map[string]string{"command": "…[此前的同类 shell 命令已被后续执行取代]"})
				calls[j].Args = string(placeholder)
			}
		}
		out[i].ToolCalls = calls
	}
	return out
}
