package miniagent

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"unicode"
)

// estimateTokens 估算一次请求的 token 数，仅用于历史裁剪决策。启发式：CJK ≈ 1 token/2 字符，
// 其他 ≈ 1 token/4 字符。除 msgs 内容外，计入 system prompt 文本 + 请求信封 + 工具 schema。
//
// 信封对齐：固定开销除 systemOverheadTokens（base）外，再按消息数与 tool_call 数线性增长——
// 每条 message 的 role/字段名/标点、每个 tool_call 嵌套的 function 对象（id/type/function{name,
// arguments}）随会话长度累积，长 ReAct 会话远超 flat 400，原先单一常数系统性低估、压缩触发偏晚、
// 更易撞 context_length_exceeded 白烧一次失败往返。
func estimateTokens(msgs []Message, system string, tools []Tool) int {
	var nonCJK, cjk int
	var toolCalls int
	add := func(s string) {
		for _, r := range s {
			if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) ||
				unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r) {
				cjk++
			} else {
				nonCJK++
			}
		}
	}
	for _, m := range msgs {
		add(m.Content)
		add(m.Reasoning)
		toolCalls += len(m.ToolCalls)
		for _, tc := range m.ToolCalls {
			add(tc.Args)
		}
	}
	add(system)
	return nonCJK/4 + cjk/2 + systemOverheadTokens +
		envelopePerMsgTokens*len(msgs) + envelopePerToolCallTokens*toolCalls +
		schemaTokens(tools)
}

// schemaTokens 按 tools 实际 JSON schema 体积估算固定 token 开销，替代早期的 flat
// perToolSchemaTokens*len：后者与 schema 真实体积脱钩（description 较长的工具实际远超 60 token），
// 使 estimateTokens 系统性偏低、压缩触发偏晚，更易撞 context_length_exceeded 白烧一次失败往返。
// 按 wire.go 的 function schema 形状序列化（name+description+parameters），走与消息相同的
// CJK≈1 token/2 字符、其他≈1 token/4 字符启发式；序列化失败（理论不会发生，Parameters 由基础类型
// 构造）回落 flat 估算兜底。
func schemaTokens(tools []Tool) int {
	if len(tools) == 0 {
		return 0
	}
	var nonCJK, cjk int
	for _, t := range tools {
		b, err := json.Marshal(map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        t.Name,
				"description": t.Description,
				"parameters":  t.Parameters,
			},
		})
		if err != nil {
			return perToolSchemaTokens * len(tools)
		}
		for _, r := range string(b) {
			if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) ||
				unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r) {
				cjk++
			} else {
				nonCJK++
			}
		}
	}
	return nonCJK/4 + cjk/2
}

// estimateResponseTokens 基于响应文本/思考链/工具参数做本地 token 估算，
// 供 provider 未返回 usage 时的预算熔断 fallback。
func estimateResponseTokens(resp Response) int {
	var nonCJK, cjk int
	add := func(s string) {
		for _, r := range s {
			if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) ||
				unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r) {
				cjk++
			} else {
				nonCJK++
			}
		}
	}
	add(resp.Text)
	add(resp.Reasoning)
	for _, tc := range resp.ToolCalls {
		add(tc.Args)
	}
	return nonCJK/4 + cjk/2
}

// 一轮（成组，保 tool_calls/tool 配对）；user 与无 tool_calls 的 assistant 各自独立成轮。
func splitRounds(msgs []Message) [][]Message {
	var rounds [][]Message
	var cur []Message
	flush := func() {
		if len(cur) > 0 {
			rounds = append(rounds, cur)
			cur = nil
		}
	}
	for _, m := range msgs {
		if m.Role == roleTool && len(cur) > 0 {
			cur = append(cur, m) // tool 归属当前开启的 assistant(tool_calls) 轮
			continue
		}
		flush()
		cur = []Message{m}
		if len(m.ToolCalls) == 0 {
			flush() // user / 纯 assistant：独立成轮
		}
		// 带 tool_calls 的 assistant：cur 保持开启，吸收后续 tool 消息
	}
	flush()
	return rounds
}

func flatten(rounds [][]Message) []Message {
	var out []Message
	for _, r := range rounds {
		out = append(out, r...)
	}
	return out
}

// compactHistory 是摘要失败/无中段时的有损 fallback：保留「最早 1 轮 + 最近 keepRecent 轮」，
// 中段整轮剔除（保 tool 配对）。轮数不足 1+keepRecent 时原样返回（无需裁剪）。
func compactHistory(msgs []Message, keepRecent int) []Message {
	rounds := splitRounds(msgs)
	if len(rounds) <= 1+keepRecent {
		return msgs
	}
	out := append([]Message{}, rounds[0]...)
	out = append(out, flatten(rounds[len(rounds)-keepRecent:])...)
	return out
}

// trimRecentRounds 只保留最近 keepRecent 轮（丢 summary + 最早 + 全部旧历史），
// 是 compactHistory 仍超 window 时的最终有损裁剪（审查 v3 #4：避免循环烧请求）。
func trimRecentRounds(msgs []Message, keepRecent int) []Message {
	rounds := splitRounds(msgs)
	if len(rounds) <= keepRecent {
		return msgs
	}
	return flatten(rounds[len(rounds)-keepRecent:])
}

// contextTrimToolChars 是 context 超限时把每条 tool 结果 content 压到的字符上限。
// 2000 与 maxToolResultInHistory 持平：1000 过紧，关键错误信息（如 go build 输出）可能被截断。
const contextTrimToolChars = 2000

// contextTrimToolCharsOverride 允许配置覆盖内置默认；nil 用常量默认。
// 用 atomic 保护并发 Set/Get，防 -race 检测告警。
var contextTrimToolCharsOverride atomic.Int64

// SetContextTrimToolChars 覆盖 context 超限时 tool 结果压缩上限；测试用，正常流程由 Resolve 调用。
func SetContextTrimToolChars(n int) {
	if n > 0 {
		contextTrimToolCharsOverride.Store(int64(n))
	}
}

func getContextTrimToolChars() int {
	if v := contextTrimToolCharsOverride.Load(); v > 0 {
		return int(v)
	}
	return contextTrimToolChars
}

// trimHistoryForContext 在端点返回 context_length_exceeded 时收紧历史，供 Run 单次重试。
// 策略按「可丢失性」排序：1. 清空所有 reasoning；2. 每条 tool 结果 content 压到 contextTrimToolChars。
// 不删消息：删 tool 消息会破坏 assistant.tool_calls / tool 配对，续跑会被端点 400。
func trimHistoryForContext(msgs []Message) []Message {
	out := make([]Message, len(msgs))
	copy(out, msgs)
	for i := range out {
		out[i].Reasoning = ""
		if out[i].Role == roleTool {
			out[i].Content = truncate(strings.TrimSpace(out[i].Content), getContextTrimToolChars(), "…[context_trim]")
		}
	}
	return out
}

// stripStaleReasoning 主动清空非最近 keepN 条 assistant 消息的 Reasoning（P1）。
// 思考模型单次 reasoning 常达正文数倍，每轮原样回灌是隐性 token 大户；而模型下一步决策几乎不需
// 回看更早的思考——结论已凝结在当时的 assistant 正文 / tool_calls 里。Reasoning 与 tool_calls/tool
// 配对无关（配对要求的是 tool_calls ↔ tool 一一对应），故清空不破坏配对、不丢任何可见事实。
//
// 与 trimHistoryForContext（被动、撞上限才清全部）互补：本函数主动、常态化、仅清「非最近 N 条」，
// 保留当前推理上下文。仅改 context 侧拷贝——入参 msgs/newMsgs/session 不被改动（持久化仍留原 reasoning）。
// keepN<0 视作 0；无非空 reasoning 可清、或全部在保留窗口内时原样返回（零拷贝）。
func stripStaleReasoning(msgs []Message, keepN int) []Message {
	if keepN < 0 {
		keepN = 0
	}
	nAssistant := 0
	hasReasoning := false
	for _, m := range msgs {
		if m.Role == roleAssistant {
			nAssistant++
			if m.Reasoning != "" {
				hasReasoning = true
			}
		}
	}
	if !hasReasoning || nAssistant <= keepN {
		return msgs // 无可清 / 全在保留窗口内
	}
	out := make([]Message, len(msgs))
	copy(out, msgs)
	// 从后往前保留最近 keepN 条 assistant 的 reasoning，更早的清空。
	kept := 0
	for i := range slices.Backward(out) {
		if out[i].Role != roleAssistant {
			continue
		}
		if kept < keepN {
			kept++
			continue
		}
		out[i].Reasoning = ""
	}
	return out
}

// truncateKeptReasoning（P7）对保留窗口内（最近 keepN 条 assistant）的超长 Reasoning 做头尾分段截断。
// 与 stripStaleReasoning 互补：后者清空「非保留」条（减条数），本函数裁「保留」条的单条体积——
// 思考模型单条 Reasoning 常达数千~数万 token，是单条最大的上下文项；stripStaleReasoning 的「全有/全无」
// 逻辑使保留的那条仍逐字全量回灌（wire.go 的 reasoning_content 字段），P1–P4 完全没减这部分体积。
//
// 仅当 Reasoning 超 threshold（rune）才截：复用 truncateHeadTail 的头 1/4 + 尾 3/4 比例——两端高价值
// （开头建立问题框架、收尾收敛到结论/动作），中段多为发散探索/试错/自我纠正，参考价值最低。threshold<=0
// 视作关闭原样返回；未超阈值、或保留窗口内无超长项时零拷贝原样返回。仅改 context 侧拷贝——Reasoning 是
// string（不可变），改 out[i].Reasoning 不污染调用方输入/newMsgs/session；不动正文/tool_calls/配对。
func truncateKeptReasoning(msgs []Message, keepN, threshold int) []Message {
	if threshold <= 0 || keepN <= 0 {
		return msgs
	}
	// 预扫：从后往前遍历保留窗口（最近 keepN 条 assistant），检查是否有超长 Reasoning。
	kept := 0
	hasOversized := false
	for i := range slices.Backward(msgs) {
		if msgs[i].Role != roleAssistant {
			continue
		}
		if kept >= keepN {
			break
		}
		kept++
		if len([]rune(msgs[i].Reasoning)) > threshold {
			hasOversized = true
			break
		}
	}
	if !hasOversized {
		return msgs
	}
	out := make([]Message, len(msgs))
	copy(out, msgs)
	kept = 0
	for i := range slices.Backward(out) {
		if out[i].Role != roleAssistant {
			continue
		}
		if kept >= keepN {
			break
		}
		kept++
		if len([]rune(out[i].Reasoning)) > threshold {
			out[i].Reasoning = truncateHeadTail(out[i].Reasoning, threshold, "…[推理中段已省略]")
		}
	}
	return out
}

// stripStaleToolArgs（P4）对非最近 keepN 条 assistant 的 write/edit tool_call，把超大 Args
// （content/old_string/new_string）压缩为「前缀 + 省略标记」。与 stripStaleReasoning 同构：
// 仅改 context 侧拷贝，不碰 newMsgs/session，不动 ToolCallID（tool_calls/tool 配对完整）。
//
// 现状盲区：handleToolCalls 把 tool_call.Args 原样入历史、每轮全量回灌，而 tool 结果有 trimForHistory
// 三级裁剪、tool 参数零裁剪——不对称。write 的 content（≤10MiB）、edit 的 old/new_string 写成功后
// 已落盘或已被替换（old_string 在文件中已不存在），历史里那份纯占位重发；保留 path 让模型知道改过
// 哪个文件。仅当某字段超 toolArgsCompressThreshold 才压（小改动不动）；read/grep 等小 args 工具不压。
// 无可压缩项（非 write/edit 会话、或大 args 全在保留窗口内）时原样返回（零拷贝）。
func stripStaleToolArgs(msgs []Message, keepN int) []Message {
	if keepN < 0 {
		keepN = 0
	}
	// 预扫：从后往前跳过最近 keepN 条 assistant，检查更早的 assistant 是否有可压缩 tool_call。
	kept := 0
	hasCompressible := false
	for i := range slices.Backward(msgs) {
		if msgs[i].Role != roleAssistant {
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
	out := make([]Message, len(msgs))
	copy(out, msgs)
	kept = 0
	for i := range slices.Backward(out) {
		if out[i].Role != roleAssistant {
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
		calls := make([]ToolCall, len(out[i].ToolCalls))
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
		return truncate(s, toolArgsKeepChars, "…[参数已省略]"), true
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

// ─── P6 / P8' / P9b：tool 结果与 tool_call args 的跨消息去重/折叠 ───
//
// 三者均仅改 context 侧拷贝（不碰 newMsgs/session），复用 keepToolArgs 保留窗口
// （最近 N 条 assistant 及其 tool 消息不动），与 stripStaleToolArgs 同窗口语义。
// 裁剪触发条件统一为「同 key 更晚的出现已取代更早的」——更早的纯占位重发，压成占位零/低损失。

// windowStartOf 返回从后往前第 keepN 条 assistant 的 index（保留窗口起点：该 index 及之后不动）。
// keepN<=0 视作 0；assistant 不足 keepN 时返回 0（全部在窗口内，无可裁剪）。
func windowStartOf(msgs []Message, keepN int) int {
	if keepN <= 0 {
		return 0
	}
	seen := 0
	for i := range slices.Backward(msgs) {
		if msgs[i].Role == roleAssistant {
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

// dedupReadResults（P6）对保留窗口外的 read 结果按 (path,offset) 去重：每组保留时间序最后一次
// 的原文，更早的压成占位。覆盖两类（v5 §2.1）：连续同 (path,offset) read（P6a，零损失）与
// read→edit/write→read 验证（P6b，消除 stale 旧内容）。无需 IsError——保留最后一次即正确语义
// （失败 read 的错误内容被后续成功 read 覆盖天然合理）。不同 offset 不合并。无可压项零拷贝。
func dedupReadResults(msgs []Message, keepN int) []Message {
	readKeyOf := map[string]string{}
	for _, m := range msgs {
		if m.Role != roleAssistant {
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
		if m.Role != roleTool {
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
		if msgs[i].Role != roleTool {
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

// successWriteEditPaths 扫描 msgs，返回 path → 成功 write/edit 的 assistant msgIdx 升序列表。
// 成功 = 对应 tool 消息 IsError=false（失败写入不改文件，不计入）。供 P8'/P11 判定
// 「同 path 是否存在更晚的成功写入」——P8' 据此折叠更早的 write/edit args，P11 折叠更早的 read 结果。
func successWriteEditPaths(msgs []Message) map[string][]int {
	isErrOf := map[string]bool{}
	for _, m := range msgs {
		if m.Role == roleTool && m.ToolCallID != "" {
			isErrOf[m.ToolCallID] = m.IsError
		}
	}
	succ := map[string][]int{}
	for i, m := range msgs {
		if m.Role != roleAssistant {
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
func foldStaleWriteEditArgs(msgs []Message, keepN int) []Message {
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
		if m.Role != roleAssistant {
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
	out := make([]Message, len(msgs))
	copy(out, msgs)
	dirty := map[int]bool{}
	for fk := range toFold {
		dirty[fk.msgIdx] = true
	}
	for i := range out {
		if !dirty[i] {
			continue
		}
		calls := make([]ToolCall, len(out[i].ToolCalls))
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
func dedupShellCommands(msgs []Message, keepN int) []Message {
	shellKeyOf := map[string]string{}
	for _, m := range msgs {
		if m.Role != roleAssistant {
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
		if m.Role != roleAssistant {
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
	out := make([]Message, len(msgs))
	copy(out, msgs)
	dirty := map[int]bool{}
	for fk := range toFold {
		dirty[fk.msgIdx] = true
	}
	for i := range out {
		if !dirty[i] {
			continue
		}
		calls := make([]ToolCall, len(out[i].ToolCalls))
		copy(calls, out[i].ToolCalls)
		for j := range calls {
			if toFold[foldKey{i, j}] {
				calls[j].Args = `{"command":"…[此前的同类 shell 命令已被后续执行取代]"}`
			}
		}
		out[i].ToolCalls = calls
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
		if m.Role != roleAssistant {
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
		if m.Role != roleTool || i >= windowStart {
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
