// sse.go 把 OpenAI chat completions 的 SSE 增量帧聚合成完整 Response。
// 恒流式专用：callLLM 只走 DoStream，故无非流式解析对偶。
package miniagent

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// sseAccum 聚合流式 chunk。content 增量拼接；tool_calls 按 index 合并
// （首个 chunk 带 id/name，后续只带 arguments 片段）；usage 取末尾带 usage
// 的 chunk——OpenAI 仅在 stream_options.include_usage 开启时由最后一个
// choices=[] 的 chunk 携带。
type sseAccum struct {
	textBuilder  strings.Builder
	toolByIndex  map[int]*ToolCall
	order        []int
	finishReason string
	usage        Usage
}

func newSSEAccum() *sseAccum {
	return &sseAccum{toolByIndex: make(map[int]*ToolCall)}
}

// streamChunk 对应一个 SSE data 帧的 JSON，只摘出聚合需要的字段。
type streamChunk struct {
	Choices []struct {
		FinishReason string `json:"finish_reason"`
		Delta        struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

func (a *sseAccum) addChunk(data []byte, onText func(string) error) error {
	var ch streamChunk
	if err := json.Unmarshal(data, &ch); err != nil {
		return err
	}
	for _, choice := range ch.Choices {
		if choice.Delta.Content != "" {
			a.textBuilder.WriteString(choice.Delta.Content)
			if onText != nil {
				if err := onText(choice.Delta.Content); err != nil {
					return err
				}
			}
		}
		for _, tc := range choice.Delta.ToolCalls {
			existing, ok := a.toolByIndex[tc.Index]
			if !ok {
				existing = &ToolCall{}
				a.toolByIndex[tc.Index] = existing
				a.order = append(a.order, tc.Index)
			}
			if tc.ID != "" {
				existing.ID = tc.ID
			}
			if tc.Function.Name != "" {
				existing.Name = tc.Function.Name
			}
			existing.Args += tc.Function.Arguments
		}
		if choice.FinishReason != "" {
			a.finishReason = choice.FinishReason
		}
	}
	if ch.Usage != nil {
		a.usage = Usage{InputTokens: ch.Usage.PromptTokens, OutputTokens: ch.Usage.CompletionTokens}
	}
	return nil
}

func (a *sseAccum) response() Response {
	out := Response{
		Text:         a.textBuilder.String(),
		Usage:        a.usage,
		FinishReason: a.finishReason,
	}
	for _, idx := range a.order {
		out.ToolCalls = append(out.ToolCalls, *a.toolByIndex[idx])
	}
	return out
}

// parseSSEStream 读 SSE 流，逐段 content delta 回调 onText，返回聚合结果。
// 帧格式：每事件一行 "data: <json>"，空行分隔；"data: [DONE]" 终止。
// onText 返回 error 时立即中止（用于下游消费者断开时停止烧 token）。
func parseSSEStream(r io.Reader, onText func(string) error) (*sseAccum, error) {
	accum := newSSEAccum()
	sc := bufio.NewScanner(r)
	// 单 chunk 不应超 64KB，保守上限 4MiB 以防个别厂商返回超长行。
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	for sc.Scan() {
		line := sc.Text()
		// SSE 规范：空行分隔事件，":" 开头为注释。
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		const prefix = "data:"
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, prefix))
		if data == "[DONE]" {
			break
		}
		if data == "" {
			continue
		}
		if err := accum.addChunk([]byte(data), onText); err != nil {
			return nil, fmt.Errorf("parse sse chunk: %w", err)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return accum, nil
}
