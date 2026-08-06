package miniagent

import "strconv"

// dedupReadResults（P6）对保留窗口外的 read 结果按 (path,offset) 去重：每组保留时间序最后一次
// 的原文，更早的压成占位。覆盖两类（v5 §2.1）：连续同 (path,offset) read（P6a，零损失）与
// read→edit/write→read 验证（P6b，消除 stale 旧内容）。无需 IsError——保留最后一次即正确语义
// （失败 read 的错误内容被后续成功 read 覆盖天然合理）。不同 offset 不合并。无可压项零拷贝。
func dedupReadResults(msgs []Message, keepN int) []Message {
	readKeyOf := map[string]string{}
	for _, m := range msgs {
		if m.Role != RoleAssistant {
			continue
		}
		for _, tc := range m.ToolCalls {
			if tc.Name != "read" {
				continue
			}
			p, ok := argPath(tc.Args)
			if !ok {
				continue
			}
			readKeyOf[tc.ID] = normalizePath(p) + "\x00" + strconv.Itoa(argReadOffset(tc.Args))
		}
	}
	if len(readKeyOf) == 0 {
		return msgs
	}
	windowStart := windowStartOf(msgs, keepN)
	// 窗口外同 key 最后出现 index；窗口内是否出现过同 key（窗口内出现则窗口外该 key 全压）。
	lastIdx := map[string]int{}
	hasInWindow := map[string]bool{}
	for i, m := range msgs {
		if m.Role != RoleTool {
			continue
		}
		key, ok := readKeyOf[m.ToolCallID]
		if !ok {
			continue
		}
		if i < windowStart {
			lastIdx[key] = i // 升序遍历后覆盖前 → 窗口外该 key 的最大 index
		} else {
			hasInWindow[key] = true
		}
	}
	toCompress := map[int]string{}
	for i := range windowStart {
		if msgs[i].Role != RoleTool {
			continue
		}
		key, ok := readKeyOf[msgs[i].ToolCallID]
		if !ok {
			continue
		}
		if hasInWindow[key] || lastIdx[key] > i {
			toCompress[i] = "…[此前的 read(" + key + ") 已被更新的读取取代]"
		}
	}
	if len(toCompress) == 0 {
		return msgs
	}
	out := make([]Message, len(msgs))
	copy(out, msgs)
	for i, marker := range toCompress {
		out[i].Content = marker
	}
	return out
}

// foldStaleReadResults（P11）对保留窗口外的 read 结果，若同 path 上存在更晚的成功 write/edit，
// 把其 Content 折叠为占位。edit/write 成功后文件已变，更早的 read 读到的是旧版本（stale，可能
// 误导）；P6 需「再次 read 同 (path,offset)」才清前置 read（P6b），P8' 只清 write/edit args——
// 二者都漏「edit 后未再 read」的旧 read（v6 §4 实测：占 payload 36%）。P11 与 P8' 对称补全：同
// path 更晚成功写入触发，按 path 不限 offset（edit 可影响任意行，同 path 所有 offset 的旧 read 均
// 过期）。成功判定经 successWriteEditPaths（IsError）。无可压项零拷贝；仅改 tool 消息 Content 拷贝。
func foldStaleReadResults(msgs []Message, keepN int) []Message {
	readPathOf := map[string]string{}
	for _, m := range msgs {
		if m.Role != RoleAssistant {
			continue
		}
		for _, tc := range m.ToolCalls {
			if tc.Name != "read" {
				continue
			}
			if p, ok := argPath(tc.Args); ok {
				readPathOf[tc.ID] = normalizePath(p)
			}
		}
	}
	if len(readPathOf) == 0 {
		return msgs
	}
	succ := successWriteEditPaths(msgs)
	if len(succ) == 0 {
		return msgs
	}
	windowStart := windowStartOf(msgs, keepN)
	toFold := map[int]string{}
	for i, m := range msgs {
		if m.Role != RoleTool || i >= windowStart {
			continue
		}
		rp, ok := readPathOf[m.ToolCallID]
		if !ok {
			continue
		}
		for _, wIdx := range succ[rp] {
			if wIdx > i { // 同 path 更晚的成功写入存在 → 旧 read 结果折叠
				toFold[i] = "…[此前的 read(" + rp + ") 结果已被后续编辑取代]"
				break
			}
		}
	}
	if len(toFold) == 0 {
		return msgs
	}
	out := make([]Message, len(msgs))
	copy(out, msgs)
	for i, marker := range toFold {
		out[i].Content = marker
	}
	return out
}
