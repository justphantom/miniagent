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
