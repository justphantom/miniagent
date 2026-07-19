package miniagent

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
)

// DoStream 以 SSE 流式调用 chat completions，每收到一段文本 delta 回调 onText，
// 最终返回与非流式 Do 同构的 Response（tool_calls 的增量按 index 自动合并）。
//
// 重试边界：仅在收到首个 200 响应前重试（连接失败或可重试状态码）；
// 一旦进入 SSE 流读取，onText 已产生副作用，不再重试，直接返回 error。
//
// onText 返回 error 时立即中止流读——下游消费者（如 stdout 管道关闭）断开时
// 借此停止后续生成，避免浪费 token。nil onText 表示只聚合不回传增量。
//
// 超时注意：复用 c.HTTP 的 Timeout，它覆盖整个流读过程；长回答场景需配置
// 更长 Timeout 或改由 ctx 控制单步超时。
func (c *HTTPClient) DoStream(ctx context.Context, req Request, onText func(string) error) (Response, error) {
	req.Stream = true
	client, u, body, err := c.prepareDo(req)
	if err != nil {
		return Response{}, err
	}
	if c.Logger != nil {
		c.Logger.Debug("stream request", "url", u.String(), "model", req.Model, "messages", len(req.Messages))
	}
	url := u.String()
	var lastErr error
	for attempt := 0; ; attempt++ {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return Response{}, fmt.Errorf("build request: %w", err)
		}
		httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Accept", "text/event-stream")

		resp, err := client.Do(httpReq)
		if err != nil {
			// 首字节前的网络层错误：可重试。
			lastErr = fmt.Errorf("llm request: %w", err)
			if attempt >= len(retryDelays) {
				return Response{}, lastErr
			}
			if e := sleepRetry(ctx, attempt, 0, nil); e != nil {
				return Response{}, e
			}
			continue
		}

		if resp.StatusCode == http.StatusOK {
			// 进入流读，不再重试。defer 在返回前关闭 body。
			defer func() { _ = resp.Body.Close() }()
			accum, err := parseSSEStream(resp.Body, onText)
			if err != nil {
				return Response{}, fmt.Errorf("read stream: %w", err)
			}
			return accum.response(), nil
		}

		// 非 200：读 body（限 1MiB）决定是否可重试，与 Do 对齐。
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20+1))
		_ = resp.Body.Close()
		lastErr = formatDoErr(raw, resp.StatusCode, nil)
		if !retryableStatus(resp.StatusCode) || attempt >= len(retryDelays) {
			return Response{}, lastErr
		}
		raHeader := parseRetryAfterHeader(resp.Header.Get("Retry-After"))
		if e := sleepRetry(ctx, attempt, raHeader, raw); e != nil {
			return Response{}, e
		}
	}
}
