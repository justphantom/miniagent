// Package text provides plain-text helpers shared by the core and addon subpackages
// (compaction/event/builtin/provider/openai): rune truncation (head / tail / head+tail split),
// CJK/non-CJK rune counting, Unix millisecond timestamps, and HTTP URL validation.
// All are pure functions independent of agent domain types, so they form a repo-level shared layer
// (internal/text), eliminating divergent truncation/counting implementations across packages.
// ValidateURL lives here (not in the config package) so provider implementations can share the exact
// same URL rules without importing the CLI config layer.
package text

import (
	"fmt"
	"net/url"
	"time"
	"unicode"
)

// NowMs returns a Unix millisecond timestamp for stamping messages (e.g. the compaction summaryMsg.Ts).
func NowMs() int64 {
	return time.Now().UnixMilli()
}

// CountCharsLocal counts the non-CJK / CJK runes in s (same basis as token estimation).
func CountCharsLocal(s string) (nonCJK, cjk int) {
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

// Truncate clamps s to n runes and appends marker when it truncated. n<=0 returns s as-is.
func Truncate(s string, n int, marker string) string {
	if n <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + marker
}

// TruncateTail keeps the last n runes of s, prefixing marker when it truncates.
// Serves the tail fallback (after the shell accumulator keeps the tail and drops the middle, the window tail is truncated to maxChars). n<=0 returns s as-is; len<=n is not truncated.
func TruncateTail(s string, n int, marker string) string {
	if n <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return marker + string(r[len(r)-n:])
}

// TruncateHeadTail truncates s to roughly n runes, keeping a head of headN + a tail of tailN joined by marker.
// Used for tool results whose key information is in the tail: head-only would drop compile/test error
// summaries, limit-hit notices, and other diagnostics.
// Head gets n/4 (leading context / command echo), tail gets 3n/4 (where error conclusions cluster). n<=0 returns s as-is; len<=n is not truncated.
// marker is placed at the middle omission (different semantics from Truncate's trailing marker).
func TruncateHeadTail(s string, n int, marker string) string {
	if n <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	headN := max(n/4, 1)
	tailN := max(n-headN, 1)
	if headN+tailN >= len(r) {
		return s // head+tail window already covers everything, no truncation needed (a marker would only add noise)
	}
	return string(r[:headN]) + marker + string(r[len(r)-tailN:])
}

// ValidateURL parses and validates raw as a legal http(s) URL (with scheme+host).
// Shared by the core config validation and the provider implementations, to avoid divergence.
func ValidateURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("url %q failed to parse: %w (expected http(s)://host[:port][/path])", raw, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("url %q is missing scheme or host (expected http(s)://host[:port])", raw)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("url %q scheme %q is not supported (http/https only)", raw, u.Scheme)
	}
	// Reject embedded userinfo (https://key@host): credentials embedded in the URL get logged in error bodies, and the Go transport may also
	// send them as Basic Auth. The API key must be injected via provider.key / $MINIAGENT_API_KEY, never in the URL.
	if u.User != nil {
		return nil, fmt.Errorf("url %q contains userinfo (user:pass@host) — forbidden, to prevent credentials embedded in the URL from being logged or leaked", raw)
	}
	return u, nil
}
