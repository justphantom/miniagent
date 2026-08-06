// Package openai 是 OpenAI 兼容 Chat Completions 的 provider 实现：请求序列化（wire）、
// 非流式 client（ChatClient）、流式 client（StreamClient）、SSE 解析（stream_parse）、
// models 列表与重试/退避（retry）。它是 core LLM/Doer 接口的默认实现——core 循环据此与
// 具体供应商解耦，自定义 provider 实现 miniagent.LLM 即可替换，核心零改动。
//
// wire.go 是 OpenAI Chat Completions schema 的序列化层。
// chatMessage / chatToolCall 与 miniagent.Message / miniagent.ToolCall 字段刻意重复：
// 上层 domain 类型不绑死特定厂商的 JSON 形状（嵌套 function 对象、snake_case
// 字段名），新增字段时需同步两处并保持与 OpenAI API 字段顺序、命名一致。
// chatMessage 不含 Kind：session 层标记不泄漏给 LLM（buildChatBody 独立构造）。
package openai

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/justphantom/miniagent/internal/miniagent"
)

type chatMessage struct {
	Role             string         `json:"role"`
	Content          string         `json:"content"`
	ReasoningContent string         `json:"reasoning_content,omitempty"`
	ToolCalls        []chatToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string         `json:"tool_call_id,omitempty"`
}

type chatToolCall struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Fn   struct {
		Name string `json:"name"`
		Args string `json:"arguments"`
	} `json:"function"`
}

// maxRequestBodyBytes 是 chat completion 请求体的字节上限，与响应上限对齐。
// 超过此值的请求直接拒绝，避免超大请求 OOM/烧钱。
const maxRequestBodyBytes = 4 << 20

// estimateRequestBodySize 粗略估算 buildChatBody 将生成的 JSON 字节数，用于 marshal 前
// 拦截超大请求，避免 OOM。按字符串总长度的 1.3 倍 + 固定信封开销估算，偏保守。
func estimateRequestBodySize(req miniagent.Request) int64 {
	size := int64(256) // 模型、max_tokens、stream、stream_options、tools 等固定字段开销
	size += int64(len(req.System))
	for _, m := range req.Messages {
		size += int64(len(m.Role) + len(m.Content) + len(m.Reasoning) + len(m.ToolCallID))
		for _, tc := range m.ToolCalls {
			size += int64(len(tc.ID) + len(tc.Name) + len(tc.Args))
		}
	}
	for _, t := range req.Tools {
		size += int64(len(t.Name) + len(t.Description))
		// parameters 已是不定类型，这里只按最小估计；真正大的是 messages。
		size += 64
	}
	return size * 13 / 10
}

func buildChatBody(req miniagent.Request) ([]byte, error) {
	size := estimateRequestBodySize(req)
	if size > maxRequestBodyBytes {
		return nil, fmt.Errorf("请求预估 %d 字节超过上限 %d", size, maxRequestBodyBytes)
	}
	msgs := make([]chatMessage, 0, len(req.Messages)+1)
	if req.System != "" {
		msgs = append(msgs, chatMessage{Role: miniagent.RoleSystem, Content: req.System})
	}
	for _, m := range req.Messages {
		// 回灌 reasoning：assistant 的思考链以 reasoning_content 发回（DeepSeek 兼容）。
		cm := chatMessage{Role: m.Role, Content: m.Content, ReasoningContent: m.Reasoning, ToolCallID: m.ToolCallID}
		for _, tc := range m.ToolCalls {
			ctc := chatToolCall{ID: tc.ID, Type: "function"}
			ctc.Fn.Name = tc.Name
			ctc.Fn.Args = tc.Args
			cm.ToolCalls = append(cm.ToolCalls, ctc)
		}
		msgs = append(msgs, cm)
	}
	payload := map[string]any{
		"model":    req.Model,
		"messages": msgs,
	}
	if req.MaxTokens > 0 {
		payload["max_tokens"] = req.MaxTokens
	}
	// 思考级别：空/ThinkingOff 不写入；默认字段 reasoning_effort，provider 的 ThinkingMapping
	// 可覆盖字段名与级别取值映射（跨供应商兼容，审查 v2 #7）。
	if req.ThinkingLevel != "" && req.ThinkingLevel != miniagent.ThinkingOff {
		field, val := "reasoning_effort", req.ThinkingLevel
		if req.Thinking != nil {
			if req.Thinking.Field != "" {
				field = req.Thinking.Field
			}
			if mapped, ok := req.Thinking.Map[req.ThinkingLevel]; ok {
				val = mapped
			}
		}
		payload[field] = val
	}
	if req.Stream {
		// stream_options.include_usage：让末 chunk 携带 usage（计费/熔断仍以它为准）。
		payload["stream"] = true
		payload["stream_options"] = map[string]any{"include_usage": true}
	}
	if len(req.Tools) > 0 {
		funcs := make([]map[string]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			funcs = append(funcs, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        t.Name,
					"description": t.Description,
					"parameters":  t.Parameters,
				},
			})
		}
		payload["tools"] = funcs
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maxRequestBodyBytes {
		return nil, fmt.Errorf("请求体 %d 字节超过上限 %d", len(body), maxRequestBodyBytes)
	}
	return body, nil
}

// chatCompletionResponse 只摘出循环需要的字段：首条 choice 的 message
// （content + tool_calls）、finish_reason、usage。
type chatCompletionResponse struct {
	Choices []struct {
		Message struct {
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content"`
			Reasoning        string `json:"reasoning"`
			ToolCalls        []struct {
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

func parseChatResponse(raw []byte) (miniagent.Response, error) {
	var v chatCompletionResponse
	if err := json.Unmarshal(raw, &v); err != nil {
		return miniagent.Response{}, fmt.Errorf("parse response: %w", err)
	}
	out := miniagent.Response{}
	if len(v.Choices) == 0 {
		// 空 choices 是端点异常（内容过滤/代理故障），静默零值会让上层把
		// 它当作"成功的空回答"（退出码 0、text 为空），必须报错。
		return miniagent.Response{}, errors.New("llm response has no choices")
	}
	ch := v.Choices[0]
	out.Text = ch.Message.Content
	// 双兼容：DeepSeek 系用 reasoning_content，OpenAI o 系用 reasoning；前者优先。
	out.Reasoning = ch.Message.ReasoningContent
	if out.Reasoning == "" {
		out.Reasoning = ch.Message.Reasoning
	}
	out.FinishReason = ch.FinishReason
	for _, tc := range ch.Message.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, miniagent.ToolCall{
			ID:   tc.ID,
			Name: tc.Function.Name,
			Args: tc.Function.Arguments,
		})
	}
	if v.Usage != nil {
		out.Usage = miniagent.Usage{InputTokens: v.Usage.PromptTokens, OutputTokens: v.Usage.CompletionTokens}
	}
	return out, nil
}
