package miniagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"log/slog"
)

// maxChatBodyBytes 是单次 chat completions 响应 body 的字节上限。
// 恰好达到上限不截断；多读 1 字节判定越界并报错，避免异常端点拖垮内存。
const maxChatBodyBytes = 4 << 20 // 4 MiB

// HTTPClient calls an OpenAI-compatible chat completions endpoint via net/http.
type HTTPClient struct {
	APIKey  string
	BaseURL string
	HTTP    *http.Client
	Logger  *slog.Logger
}

// Do 调用 POST {BaseURL}/v1/chat/completions（非流式），解析 choices[0] /
// usage / finish_reason。响应 body 上限 maxChatBodyBytes，越界报错。
func (c *HTTPClient) Do(ctx context.Context, req Request) (Response, error) {
	client, u, body, err := c.prepareDo(req)
	if err != nil {
		return Response{}, err
	}
	if c.Logger != nil {
		c.Logger.Debug("llm request", "url", u.String(), "model", req.Model, "messages", len(req.Messages))
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return Response{}, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
	httpReq.Header.Set("Content-Type", "application/json")

	callStart := time.Now()
	resp, err := client.Do(httpReq)
	callDur := time.Since(callStart)
	if err != nil {
		if c.Logger != nil {
			c.Logger.Warn("llm request failed", "error", err, "duration_ms", callDur.Milliseconds())
		}
		return Response{}, fmt.Errorf("llm request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, rerr := io.ReadAll(io.LimitReader(resp.Body, maxChatBodyBytes+1))
	if rerr != nil {
		return Response{}, fmt.Errorf("read response: %w", rerr)
	}
	if int64(len(raw)) > maxChatBodyBytes {
		return Response{}, fmt.Errorf("response exceeded %d bytes", maxChatBodyBytes)
	}
	if resp.StatusCode != http.StatusOK {
		return Response{}, fmt.Errorf("llm returned %d: %s", resp.StatusCode, truncate(string(raw), 500, "…"))
	}
	out, perr := parseChatResponse(raw)
	if perr != nil {
		return Response{}, perr
	}
	if c.Logger != nil {
		c.Logger.Info("llm call done", "duration_ms", callDur.Milliseconds(), "input_tokens", out.Usage.InputTokens, "output_tokens", out.Usage.OutputTokens, "tool_calls", len(out.ToolCalls), "finish_reason", out.FinishReason)
	}
	return out, nil
}

// endpoint 解析 BaseURL 并拼接 path；c.HTTP 为 nil 时用 defaultTimeout 的默认 client。
func (c *HTTPClient) endpoint(path string, defaultTimeout time.Duration) (*http.Client, *url.URL, error) {
	base, err := url.Parse(strings.TrimRight(c.BaseURL, "/"))
	if err != nil || base.Scheme == "" || base.Host == "" {
		return nil, nil, fmt.Errorf("miniagent: base_url %q is invalid", c.BaseURL)
	}
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: defaultTimeout}
	}
	return client, base.JoinPath(path), nil
}

func (c *HTTPClient) prepareDo(req Request) (*http.Client, *url.URL, []byte, error) {
	if c.APIKey == "" {
		return nil, nil, nil, errors.New("miniagent: api_key is empty")
	}
	client, u, err := c.endpoint("/v1/chat/completions", 120*time.Second)
	if err != nil {
		return nil, nil, nil, err
	}
	body, err := buildChatBody(req)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("build request body: %w", err)
	}
	return client, u, body, nil
}

// chatCompletionResponse 只摘出循环需要的字段：首条 choice 的 message
// （content + tool_calls）、finish_reason、usage。
type chatCompletionResponse struct {
	Choices []struct {
		Message struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

func parseChatResponse(raw []byte) (Response, error) {
	var v chatCompletionResponse
	if err := json.Unmarshal(raw, &v); err != nil {
		return Response{}, fmt.Errorf("parse response: %w", err)
	}
	out := Response{}
	if len(v.Choices) > 0 {
		ch := v.Choices[0]
		out.Text = ch.Message.Content
		out.FinishReason = ch.FinishReason
		for _, tc := range ch.Message.ToolCalls {
			out.ToolCalls = append(out.ToolCalls, ToolCall{
				ID:   tc.ID,
				Name: tc.Function.Name,
				Args: tc.Function.Arguments,
			})
		}
	}
	if v.Usage != nil {
		out.Usage = Usage{InputTokens: v.Usage.PromptTokens, OutputTokens: v.Usage.CompletionTokens}
	}
	return out, nil
}
