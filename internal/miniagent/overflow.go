package miniagent

import "regexp"

// overflowPatterns is the compiled set of multi-vendor context-overflow error regexes (ported from pi
// overflow.ts:37-63, 24 patterns, §P1-C). Package-level var, compiled once at init. Go RE2 syntax supports
// (?:...), \d, [\d,]+, ?, *, +, semantically equivalent to the original JS; a (?i) case-insensitive flag is
// added uniformly (equivalent to JS /i).
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

// nonOverflowPatterns holds the exclusion regexes (ported from pi overflow.ts:74-78, §P1-C), preventing
// throttling/rate-limit responses from being mismatched by the generic "too many tokens" rule (typically
// Bedrock "ThrottlingException: Too many tokens").
// Note: the pi original only had `^(Throttling error|Service unavailable):`, which is not enough to exclude
// Bedrock's "ThrottlingException", so `throttling` is added (throttling is never a context overflow), aligning
// with the §P1-C C.5 counter-example intent of "must be false".
var nonOverflowPatterns = compilePatterns(
	`^(Throttling error|Service unavailable):`,
	`throttling`,
	`rate limit`,
	`too many requests`,
)

// compilePatterns compiles the regexes in bulk and adds the case-insensitive flag uniformly (equivalent to JS /i).
// MustCompile makes a malformed pattern fail-fast at init.
func compilePatterns(patterns ...string) []*regexp.Regexp {
	out := make([]*regexp.Regexp, len(patterns))
	for i, p := range patterns {
		out[i] = regexp.MustCompile("(?i)" + p)
	}
	return out
}

// IsContextLengthError identifies context-overflow error response bodies (§P1-C: upgraded from 4 markers to
// 24 regexes + 4 exclusions). It first runs nonOverflowPatterns to exclude throttling/rate-limit, then runs
// overflowPatterns for a match.
// The signature (raw []byte) bool is unchanged → the two call sites in client.go/stream.go need zero signature
// changes; only the status gate is relaxed from 400-only to 400||413.
// The worst case of a misjudgment = one pointless tightening retry, consistent with the old semantics.
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
