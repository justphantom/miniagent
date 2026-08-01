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
	"net/url"
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
}

// Delta 是推给消费方的流式增量。
type Delta struct {
	Kind DeltaKind
	Text string
}

// DoStream 流式调用 POST /v1/chat/completions（stream=true）。onDelta 实时推增量；
// 返回聚合 Response（与 Do 同构）。重试不同于 Do：连接/HTTP 错误直接上抛——一旦
// 开始流出 delta 即不可撤回，故不重试（瞬时抖动由调用方重跑或退回非流式 Do）。
func (c *HTTPClient) DoStream(ctx context.Context, req Request, onDelta func(Delta)) (Response, error) {
	client, u, body, err := c.prepareStream(req)
	if err != nil {
		return Response{}, err
	}
	if c.Logger != nil {
		c.Logger.Debug("llm stream request", "url", u.String(), "model", req.Model, "messages", len(req.Messages))
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return Response{}, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(httpReq)
	if err != nil {
		if c.Logger != nil {
			c.Logger.Warn("llm stream request failed", "error", err)
		}
		return Response{}, fmt.Errorf("llm request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxChatBodyBytes+1))
		// context 超限在流出首 chunk 前以 400 返回，沿用非流式判定供 Run 降级重试。
		if resp.StatusCode == http.StatusBadRequest && isContextLengthError(raw) {
			return Response{}, fmt.Errorf("%w: %s", ErrContextLength, truncate(string(raw), 500, "…"))
		}
		if resp.StatusCode == http.StatusBadRequest && isThinkingError(raw) {
			return Response{}, fmt.Errorf("%w: %s", ErrThinkingUnsupported, truncate(string(raw), 500, "…"))
		}
		return Response{}, fmt.Errorf("llm returned %d: %s", resp.StatusCode, truncate(string(raw), 500, "…"))
	}
	return parseSSE(resp.Body, onDelta)
}

func (c *HTTPClient) prepareStream(req Request) (*http.Client, *url.URL, []byte, error) {
	if c.APIKey == "" {
		return nil, nil, nil, errors.New("miniagent: api_key is empty")
	}
	client, u, err := c.chatEndpoint(120 * time.Second)
	if err != nil {
		return nil, nil, nil, err
	}
	req.Stream = true
	body, err := buildChatBody(req)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("build request body: %w", err)
	}
	return client, u, body, nil
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
}

// parseSSE 读 SSE 流：每个 content/reasoning 片段调 onDelta，结束时返回聚合 Response。
// 行格式：以 "data: " 开头，载荷为 JSON；"data: [DONE]" 结束；空行/":" 注释忽略。
func parseSSE(r io.Reader, onDelta func(Delta)) (Response, error) {
	acc := &streamAccum{calls: map[int]*streamToolCall{}}
	sc := bufio.NewScanner(r)
	// arguments 片段可能很长（大 JSON 参数），放宽默认 64KB 行长到 1MB。
	sc.Buffer(make([]byte, 64*1024), 1<<20)
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
		acc.apply(chunk, onDelta)
	}
	if err := sc.Err(); err != nil {
		return Response{}, fmt.Errorf("read sse: %w", err)
	}
	return acc.response(), nil
}

func (a *streamAccum) apply(ch chatCompletionChunk, onDelta func(Delta)) {
	// usage 通常只在末 chunk（stream_options.include_usage）出现，无 choices。
	if ch.Usage != nil {
		a.usage = Usage{InputTokens: ch.Usage.PromptTokens, OutputTokens: ch.Usage.CompletionTokens}
	}
	if len(ch.Choices) == 0 {
		return
	}
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
