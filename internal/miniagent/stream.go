package miniagent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// chatCompletionChunk 是流式响应的单个 SSE 载荷（OpenAI chat.completion.chunk）。
// tool_calls 是 index-based 增量：首个 chunk 带 id/name，后续只带 arguments 片段。
type chatCompletionChunk struct {
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			Reasoning        string `json:"reasoning"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	// Error：部分 provider/网关在中途以 {"error":{"message":...}} chunk 报错（内容过滤、
	// 上游故障）。标准 chat.completion.chunk schema 无此字段，出现即视为流级错误（P1-3）。
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Delta 是推给消费方的流式增量。
type Delta struct {
	Kind DeltaKind
	Text string
}

// DoStream 流式调用 POST /v1/chat/completions（stream=true），onDelta 实时推增量，返回聚合 Response。
// 重试：pre-delta 阶段（client.Do 失败或非 200，尚未流出 delta）复用 Do 的
// shouldRetryStatus+退避+Retry-After；进入 parseSSE（200，已流 delta）即不可撤回不重试（P2-4）。
func (c *HTTPClient) DoStream(ctx context.Context, req Request, onDelta func(Delta)) (Response, error) {
	if c.APIKey == "" {
		return Response{}, errors.New("miniagent: api_key is empty")
	}
	// URL 走 chatEndpoint（懒解析+缓存）；http.Client 用 streamHTTPClient：流式不能设
	// http.Client.Timeout（覆盖 body 读取会砍断长生成，P2-5），总时长交由 ctx 控制。
	_, u, err := c.chatEndpoint(120 * time.Second)
	if err != nil {
		return Response{}, err
	}
	req.Stream = true
	body, err := buildChatBody(req)
	if err != nil {
		return Response{}, fmt.Errorf("build request body: %w", err)
	}
	client := c.streamHTTPClient()
	if c.Logger != nil {
		c.Logger.Debug("llm stream request", "url", u.String(), "model", req.Model, "messages", len(req.Messages))
	}
	backoff := retryBaseDelay
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return Response{}, err
		}
		// 每次重建 httpReq：body reader 上一轮已被消费，复用会发空 body。
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
		if err != nil {
			return Response{}, fmt.Errorf("build request: %w", err)
		}
		httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
		httpReq.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(httpReq)
		if err != nil {
			if c.Logger != nil {
				c.Logger.Warn("llm stream request failed", "error", err, "failed_attempt", attempt+1)
			}
			if attempt == maxRetries {
				return Response{}, fmt.Errorf("after %d retries: llm request: %w", attempt, err)
			}
			if waitErr := sleepCtx(ctx, capRetryDelay(backoff, -1)); waitErr != nil {
				return Response{}, waitErr
			}
			backoff *= 2
			continue
		}
		if resp.StatusCode != http.StatusOK {
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxChatBodyBytes+1))
			_ = resp.Body.Close()
			// 400 context 超限 / thinking：沿用非流式判定供 Run 降级，不重试；attempt>0 加 "after N retries" 前缀（P3 排错）。
			prefix := ""
			if attempt > 0 {
				prefix = fmt.Sprintf("after %d retries: ", attempt)
			}
			if resp.StatusCode == http.StatusBadRequest && isContextLengthError(raw) {
				return Response{}, fmt.Errorf("%s%w: %s", prefix, ErrContextLength, truncate(string(raw), 500, "…"))
			}
			if resp.StatusCode == http.StatusBadRequest && isThinkingError(raw) {
				return Response{}, fmt.Errorf("%s%w: %s", prefix, ErrThinkingUnsupported, truncate(string(raw), 500, "…"))
			}
			if !shouldRetryStatus(resp.StatusCode) || attempt == maxRetries {
				return Response{}, errors.New(prefix + fmt.Sprintf("llm returned %d: %s", resp.StatusCode, truncate(string(raw), 500, "…")))
			}
			if c.Logger != nil {
				c.Logger.Warn("llm stream non-200, retrying", "status", resp.StatusCode, "failed_attempt", attempt+1)
			}
			if waitErr := sleepCtx(ctx, capRetryDelay(backoff, parseRetryAfter(resp.Header))); waitErr != nil {
				return Response{}, waitErr
			}
			backoff *= 2
			continue
		}
		// HTTP 200：进入 SSE 解析（开始流出 delta），此后不可重试。
		res, perr := parseSSE(resp.Body, onDelta)
		_ = resp.Body.Close()
		return res, perr
	}
	return Response{}, errors.New("llm stream retry loop exited unexpectedly")
}

// client 返回非流式 http.Client。c.HTTP!=nil 沿用注入；否则懒构造并缓存带 defaultTimeout 的 client（P3-5）。
func (c *HTTPClient) client(defaultTimeout time.Duration) *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	c.defaultClientOnce.Do(func() { c.defaultClient = &http.Client{Timeout: defaultTimeout} })
	return c.defaultClient
}

// streamHTTPClient 流式 client：注入的若有总 Timeout 会砍断 body（P2-5/P1-A），改用其 Transport 另造无 Timeout client（保留代理，#2）；未注入则缓存。
func (c *HTTPClient) streamHTTPClient() *http.Client {
	if c.HTTP != nil {
		if c.HTTP.Timeout > 0 {
			return &http.Client{Transport: c.HTTP.Transport}
		}
		return c.HTTP
	}
	c.streamClientOnce.Do(func() { c.streamClient = &http.Client{} })
	return c.streamClient
}

// capRetryDelay：显式 Retry-After（>=0，含 0=立即）优先于指数 backoff，再封顶 retryMaxDelay。
// retryAfter<0 表示未提供。Do/DoStream 重试循环共用（P2-4）。
func capRetryDelay(backoff, retryAfter time.Duration) time.Duration {
	if retryAfter >= 0 {
		backoff = retryAfter
	}
	if backoff > retryMaxDelay {
		backoff = retryMaxDelay
	}
	return backoff
}

// sleepCtx 等 delay 或 ctx 取消，ctx 先就绪返回 ctx.Err()。Do/DoStream 重试循环共用（P2-4）。
func sleepCtx(ctx context.Context, delay time.Duration) error {
	select {
	case <-time.After(delay):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// streamToolCall 累积单个 tool_call 的分片（按 delta.index 路由）。
type streamToolCall struct {
	id, name string
	args     strings.Builder
}

// streamAccum 把多个 chunk 聚合成完整 Response。
type streamAccum struct {
	text      strings.Builder
	reasoning strings.Builder
	calls     map[int]*streamToolCall // key = delta.index
	callOrder []int                   // index 出现顺序，稳定聚合
	finish    string
	usage     Usage
	sawChoice bool // 是否见过含 choices 的 chunk（空响应判定，P1-2）
}

// parseSSE 读 SSE 流：每个 content/reasoning 片段调 onDelta，结束时返回聚合 Response。
// 行格式：以 "data: " 开头，载荷为 JSON；"data: [DONE]" 结束；空行/":" 注释忽略。
func parseSSE(r io.Reader, onDelta func(Delta)) (Response, error) {
	acc := &streamAccum{calls: map[int]*streamToolCall{}}
	sc := bufio.NewScanner(r)
	// 放宽默认 64KB 行长到 4MB（与 maxChatBodyBytes 同级），避免超大单行 chunk 触发 ErrTooLong（P3-2）。
	sc.Buffer(make([]byte, 64*1024), 4<<20)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var chunk chatCompletionChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return Response{}, fmt.Errorf("parse sse chunk: %w", err)
		}
		// provider/网关以 error chunk 报错（内容过滤/上游故障）：上抛而非吞掉当成功（P1-3）。
		if chunk.Error != nil {
			return Response{}, fmt.Errorf("stream error from provider: %s", chunk.Error.Message)
		}
		acc.apply(chunk, onDelta)
	}
	if err := sc.Err(); err != nil {
		return Response{}, fmt.Errorf("read sse: %w", err)
	}
	res := acc.response()
	// 从未见过 choices（截断/空流/仅 usage）：报错而非伪装成空回答，与 parseChatResponse 对齐（P1-2）。
	if !acc.sawChoice && res.FinishReason == "" && res.Text == "" && len(res.ToolCalls) == 0 {
		return Response{}, errors.New("stream ended without any choices")
	}
	return res, nil
}

func (a *streamAccum) apply(ch chatCompletionChunk, onDelta func(Delta)) {
	// usage 通常只在末 chunk（stream_options.include_usage）出现，无 choices。
	if ch.Usage != nil {
		a.usage = Usage{InputTokens: ch.Usage.PromptTokens, OutputTokens: ch.Usage.CompletionTokens}
	}
	if len(ch.Choices) == 0 {
		return
	}
	a.sawChoice = true
	c := ch.Choices[0]
	if c.FinishReason != "" {
		a.finish = c.FinishReason
	}
	if c.Delta.Content != "" {
		a.text.WriteString(c.Delta.Content)
		if onDelta != nil {
			onDelta(Delta{Kind: DeltaText, Text: c.Delta.Content})
		}
	}
	// reasoning 双兼容：DeepSeek reasoning_content，其他 reasoning；前者优先。
	rc := c.Delta.ReasoningContent
	if rc == "" {
		rc = c.Delta.Reasoning
	}
	if rc != "" {
		a.reasoning.WriteString(rc)
		if onDelta != nil {
			onDelta(Delta{Kind: DeltaReasoning, Text: rc})
		}
	}
	for _, tc := range c.Delta.ToolCalls {
		acc, ok := a.calls[tc.Index]
		if !ok {
			acc = &streamToolCall{}
			a.calls[tc.Index] = acc
			a.callOrder = append(a.callOrder, tc.Index)
		}
		if tc.ID != "" {
			acc.id = tc.ID
		}
		if tc.Function.Name != "" {
			acc.name = tc.Function.Name
		}
		if tc.Function.Arguments != "" {
			acc.args.WriteString(tc.Function.Arguments)
		}
	}
}

// response 按 index 升序聚合 tool_calls，与 handleToolCalls 顺序匹配契约一致。
func (a *streamAccum) response() Response {
	out := Response{
		Text:         a.text.String(),
		Reasoning:    a.reasoning.String(),
		FinishReason: a.finish,
		Usage:        a.usage,
	}
	idxs := append([]int(nil), a.callOrder...)
	sort.Ints(idxs)
	for _, i := range idxs {
		tc := a.calls[i]
		out.ToolCalls = append(out.ToolCalls, ToolCall{ID: tc.id, Name: tc.name, Args: tc.args.String()})
	}
	return out
}
