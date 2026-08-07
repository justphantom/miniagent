package miniagent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/justphantom/miniagent/internal/text"
)

// maxToolResultInHistory：单条 tool 结果入历史字符上限，平衡可读性与上下文预算。
const maxToolResultInHistory = 4000

// maxParallelTools：同一步并行工具上限，防耗尽 FD/连接或触发目标限流。
const maxParallelTools = 8

func safeCall(ctx context.Context, logger *slog.Logger, tool Tool, name, args string) (res ToolResult) {
	defer func() {
		if r := recover(); r != nil {
			if logger != nil {
				logger.Error("tool panic recovered", "tool", name, "panic", r)
			}
			res = ToolResult{IsError: true, ExitCode: exitCodeNotSet, Output: fmt.Sprintf("工具 %q 内部错误", name)}
		}
	}()
	return tool.Call(ctx, args)
}

func buildToolIndex(tools []Tool, logger *slog.Logger) map[string]Tool {
	toolByName := make(map[string]Tool, len(tools))
	for _, t := range tools {
		if _, dup := toolByName[t.Name]; dup && logger != nil {
			// 重名静默覆盖会让前者不可达且无任何线索，路由歧义极难排查。
			logger.Warn("duplicate tool name, last wins", "tool", t.Name)
		}
		toolByName[t.Name] = t
	}
	return toolByName
}

func handleToolCalls(ctx context.Context, cfg LoopConfig, step int, resp Response, toolByName map[string]Tool, msgs []Message, newMsgs *[]Message, hooks LoopHooks, logger *slog.Logger) ([]Message, error) {
	calls := make([]ToolCall, len(resp.ToolCalls))
	for i, tc := range resp.ToolCalls {
		calls[i] = tc
		if calls[i].ID == "" {
			calls[i].ID = fmt.Sprintf("synth_%d_%d", step, i)
		}
	}
	// 思考链随 assistant 消息入历史（回灌 reasoning 模型所需）。§P0-B：附真实 usage 供后续防陈旧估算。
	// Content: resp.Text —— 模型常在 tool_call 前附说明文本（Claude 经 OpenAI 代理、部分开源模型），
	// 入历史保多轮连贯；最终文本(loop.go:166)/总结(:102)路径均设 Content，此处对齐（R4-2，原被丢弃）。
	appendMsg(&msgs, newMsgs, Message{Role: RoleAssistant, Content: resp.Text, Reasoning: resp.Reasoning, ToolCalls: calls, Usage: &resp.Usage})

	// 先按序通知本轮全部 tool_use：消费方尽早看到完整工具计划，且顺序确定。
	// OnToolUse 返回 ErrToolDenied 表示拒绝该工具（如危险命令未确认）：记录后继续
	// 通知其余工具，runToolsParallel 跳过被拒者；其他 error 仍终止循环。
	denied := make(map[string]bool)
	if hooks.OnToolUse != nil {
		for _, tc := range calls {
			if err := hooks.OnToolUse(tc.Name, tc.Args); err != nil {
				if errors.Is(err, ErrToolDenied) {
					denied[tc.ID] = true
					continue
				}
				// 配对补全：assistant 消息（含全部 tool_calls）已在 :52 append，此时 runToolsParallel
				// 尚未执行，须为全部 calls 补占位 tool 消息保配对——与 OnToolResult(:89)/ShapeToolResult(:103)
				// 错误路径一致（彼处 i.. 或 i+1.. 视已执行范围），防续跑被端点 400。
				fillPlaceholderTail(&msgs, newMsgs, calls, 0)
				return msgs, err
			}
		}
	}

	// 同一步内 LLM 一次发起的多个 tool_call 相互独立，串行会让总耗时 = Σ 单工具
	// 耗时（shell 可达数十秒）。并行执行，结果按原 index 回填，保证历史消息
	// 与 assistant.tool_calls 一一对应（OpenAI 要求顺序匹配）。
	parallel := cfg.MaxParallelTools
	if parallel <= 0 {
		parallel = maxParallelTools
	}
	results := runToolsParallel(ctx, logger, calls, toolByName, denied, parallel)

	for i, tc := range calls {
		tres := results[i]
		if logger != nil {
			logger.Info("tool executed", "step", step, "tool", tc.Name, "is_error", tres.IsError, "output_len", len(tres.Output))
		}
		// 工具执行后通知消费方结果（含 ExitCode/is_error），供实时观察与校验。
		if hooks.OnToolResult != nil {
			if err := hooks.OnToolResult(tc.Name, tc.ID, tres); err != nil {
				// 配对补全：下游不可写，剩余 calls（含当前 i）结果无法提交；但 assistant.tool_calls 已入历史，
				// 须为每个补一条占位 tool 消息，否则 Messages 配对断裂、续跑被端点 400（P2-1）。
				fillPlaceholderTail(&msgs, newMsgs, calls, i)
				return msgs, err
			}
		}
		// 工具结果成型：经 ShapeToolResult 钩子外挂（截断/落盘/RAG 摘要等）。nil=核心透传原文、零成型
		// （极简模式）；默认实现 NewDefaultShapeToolResult 承载原 trimForHistory 截断 + 可选落盘。
		// 只改 tool 消息 content，不动 role/tool_call_id——配对不变量由核心保证。
		content := tres.Output
		if hooks.ShapeToolResult != nil {
			c, serr := hooks.ShapeToolResult(tc.Name, tc.ID, step, tres)
			if serr != nil {
				// 成型钩子抛错：当前 i 已执行（OnToolResult 已成功通知真实结果），用原始 Output 入历史
				// 保与消费方所见一致；剩余 calls 补占位保配对（区别于 OnToolResult 抛错：彼处消费方未确认，i 亦占位）。
				appendMsg(&msgs, newMsgs, Message{Role: RoleTool, ToolCallID: tc.ID, Content: tres.Output, IsError: tres.IsError})
				// i+1.. 工具已执行（runToolsParallel 全跑），其真实结果尽力通知 OnToolResult——两钩子关注点可能独立
				//（如 ShapeToolResult 落盘磁盘满，而 OnToolResult 的 stdout 管道仍可用），否则消费方漏看已执行工具的结果。
				// OnToolResult 再抛错则从该处补占位并返回该错（下游不可写，继续通知无意义）。
				if hooks.OnToolResult != nil {
					for j := i + 1; j < len(calls); j++ {
						if err := hooks.OnToolResult(calls[j].Name, calls[j].ID, results[j]); err != nil {
							// OnToolResult 在 j 处再抛错：i+1..j-1 虽已成功通知消费方，但 transcript 未 append
							// 任何 tool 消息（仅 :106 追加了 i）。故从 i+1 起全补占位（非 j），否则 i+1..j-1 缺配对。
							fillPlaceholderTail(&msgs, newMsgs, calls, i+1)
							return msgs, err
						}
					}
				}
				fillPlaceholderTail(&msgs, newMsgs, calls, i+1)
				return msgs, serr
			}
			if c != "" {
				content = c
			}
		}
		appendMsg(&msgs, newMsgs, Message{Role: RoleTool, ToolCallID: tc.ID, Content: content, IsError: tres.IsError})
	}
	return msgs, nil
}

// fillPlaceholderTail 在下游不可写（OnToolResult/ShapeToolResult 抛错）时，为 calls[from:] 每条
// 补一条占位 tool 消息，保 Messages 配对完整——assistant.tool_calls 已入历史，缺配对会致续跑被端点 400。
func fillPlaceholderTail(msgs, newMsgs *[]Message, calls []ToolCall, from int) {
	for j := from; j < len(calls); j++ {
		appendMsg(msgs, newMsgs, Message{Role: RoleTool, ToolCallID: calls[j].ID, Content: "工具未提交结果：上游管道错误", IsError: true})
	}
}

// runToolsParallel 并行执行 calls，返回与 calls 同序的结果。
// 各 goroutine 写入 results 的不同下标，无内存竞争；wg.Wait 提供 happens-before。
// 未知工具在调度前短路，直接回填错误结果。每个 tool 的 panic 由 safeCall 兜底。
// 用 buffered chan 做信号量限制同时在途的工具数（默认 maxParallelTools，cfg.MaxParallelTools 可覆盖）。
func runToolsParallel(ctx context.Context, logger *slog.Logger, calls []ToolCall, toolByName map[string]Tool, denied map[string]bool, parallel int) []ToolResult {
	results := make([]ToolResult, len(calls))
	var wg sync.WaitGroup
	sem := make(chan struct{}, parallel)
	for i, tc := range calls {
		if denied[tc.ID] {
			results[i] = ToolResult{IsError: true, ExitCode: exitCodeNotSet, Output: "用户拒绝执行"}
			continue
		}
		tool, ok := toolByName[tc.Name]
		if !ok {
			// ExitCode=exitCodeNotSet 与 denied 一致：未知工具从未真正执行，零值 0 会被事件层误读为成功（P3-4）。
			results[i] = ToolResult{IsError: true, ExitCode: exitCodeNotSet, Output: fmt.Sprintf("未知工具 %q", tc.Name)}
			continue
		}
		wg.Add(1)
		go func(i int, tc ToolCall, tool Tool) {
			defer wg.Done()
			// 信号量获取联动 ctx：取消后排队中的调用直接放弃，不再等空位；
			// 否则一个不尊重 ctx 的阻塞工具会永久占位，wg.Wait 不返回、Run 挂死。
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				results[i] = ToolResult{IsError: true, ExitCode: exitCodeNotSet, Output: "已取消"}
				return
			}
			defer func() { <-sem }()
			results[i] = safeCall(ctx, logger, tool, tc.Name, tc.Args)
		}(i, tc, tool)
	}
	wg.Wait()
	return results
}

// trimForHistory 把工具结果裁到 limit 字符后入历史；limit<=0 用默认上限。
// split=true（shell/grep/script 等尾部关键的工具）走头尾分段截断，保留尾部错误结论；
// 否则 head-only（read/edit 等带行号的代码类工具，前截断符合分段读大文件语义）。
// C-2 的 context 降级复用同一裁剪语义（对更早的 tool content 用更小 limit 再裁）。
func trimForHistory(s string, limit int, split bool) string {
	if limit <= 0 {
		limit = maxToolResultInHistory
	}
	if split {
		return text.TruncateHeadTail(s, limit, "…[省略中间段]")
	}
	return text.Truncate(s, limit, "…[tool_result 已截断]")
}
