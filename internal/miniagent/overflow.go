package miniagent

import "regexp"

// overflowPatterns 是编译好的多厂商 context 超限错误正则集合（移植 pi overflow.ts:37-63 的 24 条，
// §P1-C）。包级 var，init 期一次性编译。Go RE2 语法支持 (?:...)、\d、[\d,]+、?、*、+，
// 与 JS 原版语义等价；统一加 (?i) case-insensitive 标志（等价 JS /i）。
var overflowPatterns = compilePatterns(
	`prompt is too long`,
	`request_too_large`,
	`input is too long for requested model`,
	`exceeds the context window`,
	`exceeds (?:the )?(?:model'?s )?maximum context length(?: of [\d,]+ tokens?|\s*\([\d,]+\))`,
	`input token count.*exceeds the maximum`,
	`maximum prompt length is \d+`,
	`reduce the length of the messages`,
	`maximum context length is \d+ tokens`,
	`exceeds (?:the )?maximum allowed input length of [\d,]+ tokens?`,
	`input \(\d+ tokens\) is longer than the model'?s context length \(\d+ tokens\)`,
	`exceeds the limit of \d+`,
	`exceeds the available context size`,
	`greater than the context length`,
	`context window exceeds limit`,
	`exceeded model token limit`,
	`too large for model with \d+ maximum context length`,
	`prompt has [\d,]+ tokens?, but the configured context size is [\d,]+ tokens?`,
	`model_context_window_exceeded`,
	`prompt too long; exceeded (?:max )?context length`,
	`range of input length should be`,
	`context[_ ]length[_ ]exceeded`,
	`too many tokens`,
	`token limit exceeded`,
)

// nonOverflowPatterns 是排除正则（移植 pi overflow.ts:74-78，§P1-C），防 throttling/rate-limit
// 被通用「too many tokens」误命中（典型 Bedrock「ThrottlingException: Too many tokens」）。
// 注：pi 原版仅 `^(Throttling error|Service unavailable):` 不足以排除 Bedrock 的「ThrottlingException」，
// 故补 `throttling`（throttling 恒非 context 超限），对齐 §P1-C C.5 反例「must be false」的意图。
var nonOverflowPatterns = compilePatterns(
	`^(Throttling error|Service unavailable):`,
	`throttling`,
	`rate limit`,
	`too many requests`,
)

// compilePatterns 批量编译正则并统一加 case-insensitive 标志（等价 JS /i）。
// MustCompile 使模式写错在 init 期 fail-fast。
func compilePatterns(patterns ...string) []*regexp.Regexp {
	out := make([]*regexp.Regexp, len(patterns))
	for i, p := range patterns {
		out[i] = regexp.MustCompile("(?i)" + p)
	}
	return out
}

// IsContextLengthError 识别 context 超限的错误响应体（§P1-C：从 4 个 marker 升级为 24 正则 + 4 排除）。
// 先经 nonOverflowPatterns 排除 throttling/rate-limit，再经 overflowPatterns 命中。
// 签名 (raw []byte) bool 保持不变 → client.go/stream.go 两调用点零签名改动，仅状态门从仅 400 放宽到 400||413。
// 误判的最坏后果 = 一次无谓收紧重试，与旧版语义一致。
func IsContextLengthError(raw []byte) bool {
	s := string(raw)
	for _, p := range nonOverflowPatterns {
		if p.MatchString(s) {
			return false
		}
	}
	for _, p := range overflowPatterns {
		if p.MatchString(s) {
			return true
		}
	}
	return false
}
