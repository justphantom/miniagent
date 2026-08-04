package miniagent

import (
	"encoding/json"
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
	for i := len(out) - 1; i >= 0; i-- {
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
	for i := len(msgs) - 1; i >= 0; i-- {
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
	for i := len(out) - 1; i >= 0; i-- {
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
	for i := len(msgs) - 1; i >= 0; i-- {
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
	for i := len(out) - 1; i >= 0; i-- {
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
