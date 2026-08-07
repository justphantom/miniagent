package openai

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/justphantom/miniagent/internal/miniagent"
	"github.com/justphantom/miniagent/internal/text"
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

// Delta 类型在 core（miniagent.Delta，LLM.DoStream 契约引用）；本包经 miniagent.Delta 用。

// streamToolCall 累积单个 tool_call 的分片（按 delta.index 路由）。
type streamToolCall struct {
	id, name string
	args     strings.Builder
}

// maxStreamResponseBytes 限制单个流式响应累积的字节数，防止恶意/异常 provider 无限流导致 OOM。
const maxStreamResponseBytes = 4 << 20 // 4 MiB

// maxErrorChunkChars 截断 provider error chunk 的 message 字段，防恶意代理在 error message 回显
// 凭证/超大文本（与非 200 路径不回显 body 策略对齐）。
const maxErrorChunkChars = 256

// streamAccum 把多个 chunk 聚合成完整 Response。
type streamAccum struct {
	text       strings.Builder
	reasoning  strings.Builder
	calls      map[int]*streamToolCall // key = delta.index
	callOrder  []int                   // index 出现顺序，稳定聚合
	finish     string
	usage      miniagent.Usage
	sawChoice  bool // 是否见过含 choices 的 chunk（空响应判定，P1-2）
	totalBytes int  // 累积字节数上限防护
}

// parseSSE 读 SSE 流：每个 content/reasoning 片段调 onDelta，onDelta 返回 error 时中止；结束时返回聚合 Response。
// 行格式：以 "data: " 开头，载荷为 JSON；"data: [DONE]" 结束；空行/":" 注释忽略。
func parseSSE(r io.Reader, onDelta func(miniagent.Delta) error) (miniagent.Response, error) {
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
			return miniagent.Response{}, fmt.Errorf("parse sse chunk: %w", err)
		}
		// provider/网关以 error chunk 报错（内容过滤/上游故障）：上抛而非吞掉当成功（P1-3）。
		// 截断 message：与非 200 路径不回显 body 的策略对齐（防恶意代理在 error message 回显凭证/超大文本）。
		if chunk.Error != nil {
			return miniagent.Response{}, fmt.Errorf("stream error from provider: %s", text.Truncate(chunk.Error.Message, maxErrorChunkChars, "…"))
		}
		if err := acc.apply(chunk, onDelta); err != nil {
			return miniagent.Response{}, err
		}
	}
	if err := sc.Err(); err != nil {
		return miniagent.Response{}, fmt.Errorf("read sse: %w", err)
	}
	res := acc.response()
	// 从未见过 choices（截断/空流/仅 usage）：报错而非伪装成空回答，与 parseChatResponse 对齐（P1-2）。
	if !acc.sawChoice && res.FinishReason == "" && res.Text == "" && len(res.ToolCalls) == 0 {
		return miniagent.Response{}, errors.New("stream ended without any choices")
	}
	return res, nil
}

func (a *streamAccum) apply(ch chatCompletionChunk, onDelta func(miniagent.Delta) error) error {
	// usage 通常只在末 chunk（stream_options.include_usage）出现，无 choices。
	if ch.Usage != nil {
		a.usage = miniagent.Usage{InputTokens: ch.Usage.PromptTokens, OutputTokens: ch.Usage.CompletionTokens}
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
			if err := onDelta(miniagent.Delta{Kind: miniagent.DeltaText, Text: c.Delta.Content}); err != nil {
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
			if err := onDelta(miniagent.Delta{Kind: miniagent.DeltaReasoning, Text: rc}); err != nil {
				return err
			}
		}
	}
	for _, tc := range c.Delta.ToolCalls {
		acc, ok := a.calls[tc.Index]
		if ok && acc.id != "" && tc.ID != "" && tc.ID != acc.id {
			// 同 index 已归并了不同 id 的 tool_call：provider 漏发 index 致多个 tool_call 错误合并。
			// 缺 index 时无法可靠路由后续分片，明确报错而非静默合并（OpenAI 原生必带 index，兼容端点才漏）。
			return fmt.Errorf("流式 tool_call 缺失 index：%q 与 %q 归并到 index %d", acc.id, tc.ID, tc.Index)
		}
		if !ok {
			acc = &streamToolCall{}
			a.calls[tc.Index] = acc
			a.callOrder = append(a.callOrder, tc.Index)
		}
		if tc.ID != "" {
			if err := a.guardAdd(len(tc.ID)); err != nil {
				return err
			}
			acc.id = tc.ID
		}
		if tc.Function.Name != "" {
			if err := a.guardAdd(len(tc.Function.Name)); err != nil {
				return err
			}
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
func (a *streamAccum) response() miniagent.Response {
	out := miniagent.Response{
		Text:         a.text.String(),
		Reasoning:    a.reasoning.String(),
		FinishReason: a.finish,
		Usage:        a.usage,
	}
	idxs := append([]int(nil), a.callOrder...)
	sort.Ints(idxs)
	for _, i := range idxs {
		tc := a.calls[i]
		out.ToolCalls = append(out.ToolCalls, miniagent.ToolCall{ID: tc.id, Name: tc.name, Args: tc.args.String()})
	}
	return out
}
