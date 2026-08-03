package miniagent

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
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

// streamToolCall 累积单个 tool_call 的分片（按 delta.index 路由）。
type streamToolCall struct {
	id, name string
	args     strings.Builder
}

// maxStreamResponseBytes 限制单个流式响应累积的字节数，防止恶意/异常 provider 无限流导致 OOM。
const maxStreamResponseBytes = 4 << 20 // 4 MiB

// streamAccum 把多个 chunk 聚合成完整 Response。
type streamAccum struct {
	text       strings.Builder
	reasoning  strings.Builder
	calls      map[int]*streamToolCall // key = delta.index
	callOrder  []int                   // index 出现顺序，稳定聚合
	finish     string
	usage      Usage
	sawChoice  bool // 是否见过含 choices 的 chunk（空响应判定，P1-2）
	totalBytes int  // 累积字节数上限防护
}

// parseSSE 读 SSE 流：每个 content/reasoning 片段调 onDelta，onDelta 返回 error 时中止；结束时返回聚合 Response。
// 行格式：以 "data: " 开头，载荷为 JSON；"data: [DONE]" 结束；空行/":" 注释忽略。
func parseSSE(r io.Reader, onDelta func(Delta) error) (Response, error) {
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
		if data == "" {
			continue // 空 data 行（代理 keepalive），不中断流
		}
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
		if err := acc.apply(chunk, onDelta); err != nil {
			return Response{}, err
		}
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

func (a *streamAccum) apply(ch chatCompletionChunk, onDelta func(Delta) error) error {
	// usage 通常只在末 chunk（stream_options.include_usage）出现，无 choices。
	if ch.Usage != nil {
		a.usage = Usage{InputTokens: ch.Usage.PromptTokens, OutputTokens: ch.Usage.CompletionTokens}
	}
	if len(ch.Choices) == 0 {
		return nil
	}
	a.sawChoice = true
	c := ch.Choices[0]
	if c.FinishReason != "" {
		a.finish = c.FinishReason
	}
	if c.Delta.Content != "" {
		if err := a.guardAdd(len(c.Delta.Content)); err != nil {
			return err
		}
		a.text.WriteString(c.Delta.Content)
		if onDelta != nil {
			if err := onDelta(Delta{Kind: DeltaText, Text: c.Delta.Content}); err != nil {
				return err
			}
		}
	}
	// reasoning 双兼容：DeepSeek reasoning_content，其他 reasoning；前者优先。
	rc := c.Delta.ReasoningContent
	if rc == "" {
		rc = c.Delta.Reasoning
	}
	if rc != "" {
		if err := a.guardAdd(len(rc)); err != nil {
			return err
		}
		a.reasoning.WriteString(rc)
		if onDelta != nil {
			if err := onDelta(Delta{Kind: DeltaReasoning, Text: rc}); err != nil {
				return err
			}
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
			if err := a.guardAdd(len(tc.Function.Arguments)); err != nil {
				return err
			}
			acc.args.WriteString(tc.Function.Arguments)
		}
	}
	return nil
}

// guardAdd 检查再增加 n 字节是否会超过单响应上限。
func (a *streamAccum) guardAdd(n int) error {
	if a.totalBytes+n > maxStreamResponseBytes {
		return fmt.Errorf("流式响应累积超过 %d 字节上限", maxStreamResponseBytes)
	}
	a.totalBytes += n
	return nil
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
