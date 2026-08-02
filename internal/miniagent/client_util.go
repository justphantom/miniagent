package miniagent

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func shouldRetryStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests, // 429
		http.StatusInternalServerError, // 500
		http.StatusBadGateway,          // 502
		http.StatusServiceUnavailable,  // 503
		http.StatusGatewayTimeout:      // 504
		return true
	}
	return false
}

// isContextLengthError 启发式识别 context 超限的 400 响应：不同厂商措辞不一
// （OpenAI: "maximum context length" / "context_length_exceeded"；其他: "context window"）。
// 小写匹配命中任一即认定。误判的最坏后果是触发一次无谓的历史收紧重试，可接受。
func isContextLengthError(raw []byte) bool {
	lower := strings.ToLower(string(raw))
	for _, marker := range []string{"context_length", "context length", "maximum context", "context window"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// isThinkingError 启发式识别 thinking 参数（reasoning_effort 等）不被支持的 400：跨供应商
// 措辞不一（"reasoning_effort"/"unknown parameter"/"unrecognized"）。宽松识别——误判只会触发
// 一次无 thinking 重试，无害（审查 v2 #7）。
func isThinkingError(raw []byte) bool {
	lower := strings.ToLower(string(raw))
	for _, marker := range []string{"reasoning_effort", "reasoning_effort_level", "unknown parameter", "unrecognized", "unexpected argument"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

// parseRetryAfter 解析 Retry-After 头：秒数（RFC 7231 §7.1.3）或 HTTP-date。
// 未提供或解析失败返回 -1（哨兵），以区分显式 "Retry-After: 0"——后者语义为立即重试。
// 返回值不做上限封顶（封顶在调用处）。
func parseRetryAfter(h http.Header) time.Duration {
	v := strings.TrimSpace(h.Get("Retry-After"))
	if v == "" {
		return -1
	}
	if sec, err := strconv.Atoi(v); err == nil && sec >= 0 {
		return time.Duration(sec) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
		// HTTP-date 已成过去：语义等同"立即可重试"，返回 0（区别于 -1 走 backoff）。P3-3。
		return 0
	}
	return -1
}

// capRetryDelay：显式 Retry-After（>=0，含 0=立即）优先于指数 backoff，再封顶 retryMaxDelay。
// retryAfter<0 表示未提供。ChatClient.Do 与 StreamClient.DoStream 重试循环共用（P2-4）。
func capRetryDelay(backoff, retryAfter time.Duration) time.Duration {
	if retryAfter >= 0 {
		backoff = retryAfter
	}
	if backoff > retryMaxDelay {
		backoff = retryMaxDelay
	}
	return backoff
}

// sleepCtx 等 delay 或 ctx 取消，ctx 先就绪返回 ctx.Err()。ChatClient.Do 与 StreamClient.DoStream 重试循环共用。
func sleepCtx(ctx context.Context, delay time.Duration) error {
	select {
	case <-time.After(delay):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
