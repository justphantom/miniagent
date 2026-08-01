package miniagent

import (
	"context"
	"fmt"
)

// callLLMOnce 构造 Request（含 thinking）并单次调用 Do/DoStream，不含降级/重试逻辑。
//
// 放在 loop_extra.go 是为了携带 panic 兜底（审查 P3 LLM panic 兜底）：Do/DoStream 的响应
// 解析路径（parseChatResponse / parseSSE）在畸形 payload 上可能 panic，无兜底则单次坏响应
// 就崩进程——与防 tool panic 的 safeCall 不对称。recover 转为 error，使 callLLMWithDowngrade
// 沿正常 error 路径处理（降级/重试逻辑仍适用，误判无害）。命名返回值让 defer 能在 panic 后赋值 err。
func callLLMOnce(ctx context.Context, llm *HTTPClient, cfg LoopConfig, step int, msgs []Message, hooks LoopHooks) (resp Response, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("llm call panicked: %v", r)
		}
	}()
	req := Request{
		Model:         cfg.Model,
		System:        cfg.System,
		Messages:      msgs,
		MaxTokens:     cfg.MaxTokens,
		Tools:         cfg.Tools,
		ThinkingLevel: cfg.ThinkingLevel,
		Thinking:      cfg.Thinking,
	}
	if cfg.Stream {
		return llm.DoStream(ctx, req, func(d Delta) {
			if hooks.OnDelta != nil {
				_ = hooks.OnDelta(step, d.Kind, d.Text)
			}
		})
	}
	return llm.Do(ctx, req)
}
