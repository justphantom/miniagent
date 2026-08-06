package miniagent

import (
	"encoding/json"
	"strings"
	"sync/atomic"
	"time"
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

// nowMs 统一取 Unix 毫秒时间戳供 Message.Ts 用；集中一处便于审查（loop.go 无 time import，
// 故落 history_util.go）。供 appendMsg 打戳与 compactWithSummary 给 summaryMsg 打新戳。
func nowMs() int64 {
	return time.Now().UnixMilli()
}

// countCharsLocal 统计 s 的 non-CJK / CJK rune 数（与 estimateTokens 同口径），供
// estimateMessageTokensLocal 复用。
func countCharsLocal(s string) (nonCJK, cjk int) {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) ||
			unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r) {
			cjk++
		} else {
			nonCJK++
		}
	}
	return nonCJK, cjk
}

// estimateMessageTokensLocal 单条消息本地 token 估算（CJK≈1/2、其他≈1/4），
// 仅计 Content+Reasoning+ToolCalls.Args，不计 system/schema/envelope。
// 供 estimateTokensFromUsage 的 trailing 段补估（锚点 usage 已含信封真实体积，trailing 不重复计）。
func estimateMessageTokensLocal(m Message) int {
	var nonCJK, cjk int
	n, c := countCharsLocal(m.Content)
	nonCJK, cjk = nonCJK+n, cjk+c
	n, c = countCharsLocal(m.Reasoning)
	nonCJK, cjk = nonCJK+n, cjk+c
	for _, tc := range m.ToolCalls {
		n, c = countCharsLocal(tc.Args)
		nonCJK, cjk = nonCJK+n, cjk+c
	}
	return nonCJK/4 + cjk/2
}

// contextTokensFromUsage 把单次响应 usage 折算成「该次请求前缀+输出 token 总量」，
// 等价 pi calculateContextTokens。miniagent 无 cache 字段，InputTokens 已含 OpenAI
// prompt_tokens 全量（含缓存命中）。与 silent-usage-overflow 的 usageFootprint 口径一致。
func contextTokensFromUsage(u Usage) int {
	return u.InputTokens + u.OutputTokens
}

// lastApplicableUsageIndex 返回最近一条「usage 适用于当前前缀」的 assistant 索引，无则 -1。
//
// 防陈旧判定：仅 KindSummary（摘要）重定义前缀——它替换掉之前的旧历史，使其前 assistant 的
// usage（描述的是压缩前的大前缀）不再适用于当前前缀。普通 user/tool 尾随消息不重定义前缀，
// 其 token 由 estimateTokensFromUsage 的 trailing 段补估。故锚点判定用 latestSummaryTs
// （最新 KindSummary 的 Ts，无摘要则 0），取最后一条 Ts>=latestSummaryTs 且 usage 非零的 assistant。
//
// 注意：本判定刻意不用「全局最大 Ts」等纯 Ts 比较方案。后者在 miniagent 工具密集轨迹下
// （msgs 末尾常是 tool 消息、其 Ts >= 同步 assistant）会把最后 assistant 误判陈旧、几乎永不采纳
// 真实 usage，使「真实 usage 优先」特性失效。仅 KindSummary 才是真正的前缀重定义事件。
func lastApplicableUsageIndex(msgs []Message) int {
	var latestSummaryTs int64
	for _, m := range msgs {
		if m.Kind == KindSummary && m.Ts > latestSummaryTs {
			latestSummaryTs = m.Ts
		}
	}
	idx := -1
	for i, m := range msgs {
		if m.Role == RoleAssistant && m.Usage != nil &&
			(m.Usage.InputTokens > 0 || m.Usage.OutputTokens > 0) &&
			m.Ts >= latestSummaryTs {
			idx = i
		}
	}
	return idx
}

// estimateTokensFromUsage 两段式估算：lastApplicableUsageIndex 找最近未陈旧真实 usage 锚点，
// tokens = contextTokensFromUsage(usage) + Σ estimateMessageTokensLocal(msgs[idx+1:])。
// ok=false（无锚点）时调用方回落 estimateTokens。等价 pi estimate.ts estimateMessages。
// trailing 不重复计 system/schema/envelope（锚点 usage 已含信封真实体积）。
func estimateTokensFromUsage(msgs []Message) (tokens int, ok bool) {
	idx := lastApplicableUsageIndex(msgs)
	if idx < 0 {
		return 0, false
	}
	tokens = contextTokensFromUsage(*msgs[idx].Usage)
	for _, m := range msgs[idx+1:] {
		tokens += estimateMessageTokensLocal(m)
	}
	return tokens, true
}

// estimateThreshold 封装「优先真实 usage、回落本地」策略，供 FitHistory context.go:91 单点调用。
// useRealUsage=false（kill-switch）直接调 estimateTokens；否则先试 estimateTokensFromUsage，
// ok=false（无可用真实 usage）回落 estimateTokens。等价 pi estimate.ts 的「真实 usage 优先 + 防陈旧」。
func estimateThreshold(msgs []Message, system string, tools []Tool, useRealUsage bool) int {
	if useRealUsage {
		if t, ok := estimateTokensFromUsage(msgs); ok {
			return t
		}
	}
	return estimateTokens(msgs, system, tools)
}

// compactionBuffer 是为模型输出与未来轮次增长预留的 token 缓冲，对标 opencode COMPACTION_BUFFER=20000。
// §P1-B reserve = min(compactionBuffer, cfg.MaxTokens)（MaxTokens<=0 时取 compactionBuffer）。
const compactionBuffer = 20000

// usageFootprint 返回一次 LLM 调用的真实上下文占用（与 contextTokensFromUsage 同口径：input+output，
// OpenAI prompt_tokens 已含 cached 子集即完整足迹）。对标 opencode isOverflow 的 count。
func usageFootprint(u Usage) int {
	return contextTokensFromUsage(u)
}

// compactionReserve 返回从 ContextWindow 预留的 token 数（对标 opencode usable() 的 reserved）。
// reservedCfg>0 直接取；否则 maxTokens>0 取 min(compactionBuffer, maxTokens)；否则取 compactionBuffer。
func compactionReserve(maxTokens, reservedCfg int) int {
	if reservedCfg > 0 {
		return reservedCfg
	}
	if maxTokens > 0 {
		return min(compactionBuffer, maxTokens)
	}
	return compactionBuffer
}

// usableTokens 返回可用于历史填充的 token 预算（对标 opencode usable()）。
// contextWindow<=0 返回 0（未知窗口）；否则 max(0, contextWindow - compactionReserve(...))。
func usableTokens(contextWindow, maxTokens, reservedCfg int) int {
	if contextWindow <= 0 {
		return 0
	}
	return max(0, contextWindow-compactionReserve(maxTokens, reservedCfg))
}

// isUsageOverflow 判定上一步真实 usage 是否已撞上下文窗口（对标 opencode isOverflow，§P1-B）。
// auto=false 或 contextWindow<=0 返回 false；否则 usageFootprint(u) >= usableTokens(...)。
// 补 estimateTokens 对缓存内容零感知的盲区：撞 provider 前用真实 usage 先压，不再先吃一次 400。
func isUsageOverflow(u Usage, contextWindow, maxTokens, reservedCfg int, auto bool) bool {
	if !auto || contextWindow <= 0 {
		return false
	}
	return usageFootprint(u) >= usableTokens(contextWindow, maxTokens, reservedCfg)
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
		if m.Role == RoleTool && len(cur) > 0 {
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
		if out[i].Role == RoleTool {
			out[i].Content = truncate(strings.TrimSpace(out[i].Content), getContextTrimToolChars(), "…[context_trim]")
		}
	}
	return out
}
