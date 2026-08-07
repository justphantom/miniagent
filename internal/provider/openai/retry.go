package openai

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// 重试：仅对瞬时故障（429/5xx + 网络错误）生效，maxRetries 次。端点 429/503 抖动
// 数秒内自愈，2 次覆盖典型尖刺；避免真故障下放大下游压力（雪崩）。
const (
	maxRetries     = 2
	retryBaseDelay = 500 * time.Millisecond
	retryMaxDelay  = 8 * time.Second // 单次退火上限，含 Retry-After 解析值
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

// isContextLengthError 的识别在 core（overflow.go，miniagent.IsContextLengthError）：
// 24 正则 + 3 排除（§P1-C）。本包经 miniagent.IsContextLengthError 调用，避免分叉。

// isThinkingError 启发式识别 thinking 参数（reasoning_effort 等）不被支持的 400：跨供应商
// 措辞不一（"reasoning_effort"/"unknown parameter"/"unrecognized"）。强信号（字段名）直接命中；
// 弱信号需同时含 thinking/reasoning 语义 + 参数未识别措辞——防「unrecognized tool name」「unknown model」
// 等无关 400 被误判为 thinking 不支持（错误归因 + 烧 2 次请求）。误判只会触发一次无 thinking 重试（审查 v2 #7）。
func isThinkingError(raw []byte) bool {
	lower := strings.ToLower(string(raw))
	if strings.Contains(lower, "reasoning_effort") || strings.Contains(lower, "reasoning_effort_level") {
		return true
	}
	hasThinking := strings.Contains(lower, "reasoning") || strings.Contains(lower, "thinking")
	hasUnknown := strings.Contains(lower, "unknown parameter") || strings.Contains(lower, "unrecognized") || strings.Contains(lower, "unexpected argument")
	return hasThinking && hasUnknown
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
