package compaction

import (
	"github.com/justphantom/miniagent/internal/miniagent"
	"github.com/justphantom/miniagent/internal/text"
)

import "log/slog"

// CompactionOptions 是 NewCompaction 的参数——把原散布在 LoopConfig 的上下文压缩策略集中到一处，
// 让压缩成为可插拔的「外挂」而非核心配置。<=0/空 的字段在引擎内回落内置默认（见 context.go）。
type CompactionOptions struct {
	// Chat 是摘要压缩用的 client（可与主 chat 不同 provider）；nil 时调用方须自行注入 Summarize，
	// 否则 NewCompaction 用它构造 summarizeMiddle 回调。
	Chat miniagent.Doer
	// MaxTokens 是主请求的单次输出上限，供 isUsageOverflow 判定静默溢出（对标原 cfg.MaxTokens）。
	MaxTokens int
	// ContextWindow 是模型 context 上限（tokens）；<=0 关闭主动压缩（仅保留 ErrContextLength 被动重试）。
	ContextWindow int
	// Model 是主模型 id；CompactionModel 空时回落此值做摘要。
	Model string
	// CompactionModel 是摘要专用模型 id（可跨 provider）；空回落 Model。
	CompactionModel string
	System          string
	Tools           []miniagent.Tool
	KeepRecent      int // compactWithSummary 保留的最近轮数（<=0 用内置默认）。
	// KeepReasoning/KeepToolArgs/KeepReasoningChars 是主动裁剪参数（P1/P4/P7，<=0/0 用内置默认）。
	KeepReasoning      int
	KeepToolArgs       int
	KeepReasoningChars int
	SummarizerPrompt   string // 非空则全量 override 摘要 system prompt。
	SummaryMaxChars    int
	// SummaryMaxTokens 限制摘要请求输出 token 数（默认 summaryMaxTokens=1024）；<=0 用默认。
	SummaryMaxTokens     int
	PreserveRecentTokens int
	UseRealUsage         bool
	// Auto 控制静默用量溢出检测（AfterLLM 采真实 usage 判溢出、置下步 Force 压缩）；false=关闭。
	Auto     bool
	Reserved int // 从 ContextWindow 预留的 token 缓冲（<=0 回落内置默认）。
	// SessionID 透传给 OnCompacting（CompactingInput.SessionID），空串兼容无 session。
	SessionID string
	// OnCompacting 是每次摘要前的钩子（注入 context / 一次性替换 summarizerPrompt），nil=不启用。
	OnCompacting CompactingHook
	Logger       *slog.Logger
}

// 本文件是压缩子系统的「历史工具」部分（从 history_util.go 拆出），仅压缩引擎使用：
// splitRounds/flatten/compactHistory/trimRecentRounds（轮次切分与有损裁剪）、
// estimateTokensFromUsage 系列（真实 usage 优先 + 防陈旧的阈值估算）、
// isUsageOverflow/usableTokens/compactionReserve（静默溢出判定）。
// 与 context.go / history_*.go 同属压缩簇，将一并外迁到 internal/miniagent/compaction。

// estimateMessageTokensLocal 单条消息本地 token 估算（CJK≈1/2、其他≈1/4），计 Content+Reasoning+
// ToolCalls.Args + 该消息入请求的边际开销（信封 EnvelopePerMsgTokens + tool_call 包装 EnvelopePerToolCallTokens）。
// 不计 system/schema 全局开销（请求级常量，由 EstimateTokens 的 SystemOverheadTokens 兜底）。
// 此前漏算信封/tool_call 包装，致 estimateTokensFromUsage 在长会话系统性低估（envelope 累积），压缩偏晚。
func estimateMessageTokensLocal(m miniagent.Message) int {
	var nonCJK, cjk int
	n, c := text.CountCharsLocal(m.Content)
	nonCJK, cjk = nonCJK+n, cjk+c
	n, c = text.CountCharsLocal(m.Reasoning)
	nonCJK, cjk = nonCJK+n, cjk+c
	for _, tc := range m.ToolCalls {
		n, c = text.CountCharsLocal(tc.Args)
		nonCJK, cjk = nonCJK+n, cjk+c
	}
	return nonCJK/4 + cjk/2 + miniagent.EnvelopePerMsgTokens + miniagent.EnvelopePerToolCallTokens*len(m.ToolCalls)
}

// contextTokensFromUsage 把单次响应 usage 折算成「该次请求前缀+输出 token 总量」。
// miniagent 无 cache 字段，InputTokens 已含 OpenAI prompt_tokens 全量（含缓存命中）。
func contextTokensFromUsage(u miniagent.Usage) int {
	return u.InputTokens + u.OutputTokens
}

// lastApplicableUsageIndex 返回最近一条「usage 适用于当前前缀」的 assistant 索引，无则 -1。
// 防陈旧判定：仅 miniagent.KindSummary（摘要）重定义前缀；取最后一条 Ts>=latestSummaryTs 且 usage 非零的 assistant。
func lastApplicableUsageIndex(msgs []miniagent.Message) int {
	var latestSummaryTs int64
	for _, m := range msgs {
		if m.Kind == miniagent.KindSummary && m.Ts > latestSummaryTs {
			latestSummaryTs = m.Ts
		}
	}
	idx := -1
	for i, m := range msgs {
		if m.Role == miniagent.RoleAssistant && m.Usage != nil &&
			(m.Usage.InputTokens > 0 || m.Usage.OutputTokens > 0) &&
			m.Ts >= latestSummaryTs {
			idx = i
		}
	}
	return idx
}

// estimateTokensFromUsage 两段式估算：lastApplicableUsageIndex 找最近未陈旧真实 usage 锚点，
// tokens = contextTokensFromUsage(usage) + Σ estimateMessageTokensLocal(msgs[idx+1:])。
// ok=false（无锚点）时调用方回落 miniagent.EstimateTokens。
func estimateTokensFromUsage(msgs []miniagent.Message) (tokens int, ok bool) {
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

// estimateThreshold 封装「优先真实 usage、回落本地」策略，供 FitHistory 单点调用。
func estimateThreshold(msgs []miniagent.Message, system string, tools []miniagent.Tool, useRealUsage bool) int {
	if useRealUsage {
		if t, ok := estimateTokensFromUsage(msgs); ok {
			return t
		}
	}
	return miniagent.EstimateTokens(msgs, system, tools)
}

// compactionBuffer 是为模型输出与未来轮次增长预留的 token 缓冲（对标 opencode COMPACTION_BUFFER=20000）。
const compactionBuffer = 20000

// usageFootprint 返回一次 LLM 调用的真实上下文占用（input+output）。
func usageFootprint(u miniagent.Usage) int {
	return contextTokensFromUsage(u)
}

// compactionReserve 返回从 ContextWindow 预留的 token 数（对标 opencode usable() 的 reserved）。
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
// reserve 上限 clamp 到 CW/5：防 compactionReserve（默认 min(20000,maxTokens)）在小 CW 下占 CW 过半，
// 致 isUsageOverflow（usage>=usable）阈值低于 FitHistory 门控（CW*4/5=80%），Force 路径过早主导。
func usableTokens(contextWindow, maxTokens, reservedCfg int) int {
	if contextWindow <= 0 {
		return 0
	}
	reserve := min(compactionReserve(maxTokens, reservedCfg), contextWindow/5)
	return max(0, contextWindow-reserve)
}

// isUsageOverflow 判定上一步真实 usage 是否已撞上下文窗口（对标 opencode isOverflow，§P1-B）。
func isUsageOverflow(u miniagent.Usage, contextWindow, maxTokens, reservedCfg int, auto bool) bool {
	if !auto || contextWindow <= 0 {
		return false
	}
	return usageFootprint(u) >= usableTokens(contextWindow, maxTokens, reservedCfg)
}

// 一轮（成组，保 tool_calls/tool 配对）；user 与无 tool_calls 的 assistant 各自独立成轮。
func splitRounds(msgs []miniagent.Message) [][]miniagent.Message {
	var rounds [][]miniagent.Message
	var cur []miniagent.Message
	flush := func() {
		if len(cur) > 0 {
			rounds = append(rounds, cur)
			cur = nil
		}
	}
	for _, m := range msgs {
		if m.Role == miniagent.RoleTool && len(cur) > 0 {
			cur = append(cur, m) // tool 归属当前开启的 assistant(tool_calls) 轮
			continue
		}
		flush()
		cur = []miniagent.Message{m}
		if len(m.ToolCalls) == 0 {
			flush() // user / 纯 assistant：独立成轮
		}
	}
	flush()
	return rounds
}

func flatten(rounds [][]miniagent.Message) []miniagent.Message {
	var out []miniagent.Message
	for _, r := range rounds {
		out = append(out, r...)
	}
	return out
}

// compactHistory 是摘要失败/无中段时的有损 fallback：保留「最早 1 轮 + 最近 keepRecent 轮」。
func compactHistory(msgs []miniagent.Message, keepRecent int) []miniagent.Message {
	rounds := splitRounds(msgs)
	if len(rounds) <= 1+keepRecent {
		return msgs
	}
	out := append([]miniagent.Message{}, rounds[0]...)
	out = append(out, flatten(rounds[len(rounds)-keepRecent:])...)
	return out
}

// trimRecentRounds 只保留最近 keepRecent 轮，是 compactHistory 仍超 window 时的最终有损裁剪。
func trimRecentRounds(msgs []miniagent.Message, keepRecent int) []miniagent.Message {
	rounds := splitRounds(msgs)
	if len(rounds) <= keepRecent {
		return msgs
	}
	return flatten(rounds[len(rounds)-keepRecent:])
}
