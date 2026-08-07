// replay.go 实现 -replay：读取指定 session 文件，离线（不调 LLM、不需 key）把落盘消息序列
// 重新翻译成与运行时同构的 NDJSON 事件流，如实重显整个 session 的 ReAct 过程。与 -session 的
// 区别：-session 加载历史继续新对话（消耗 token、产生新事件）；-replay 纯只读重显，播完即止。
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/justphantom/miniagent/internal/miniagent"
	"github.com/justphantom/miniagent/internal/miniagent/event"
)

// runReplay：id → path → load → replay。失败打 stderr + exit 1（错误口径与 resolveSessionForRun 一致）。
func runReplay(out io.Writer, sessionDir, id string, maxBytes int64) {
	sessPath, err := miniagent.ResolveSessionPath(id, sessionDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "miniagent: replay: %v\n", err)
		os.Exit(1)
	}
	meta, msgs, err := miniagent.LoadSession(sessPath, maxBytes)
	if err != nil {
		fmt.Fprintf(os.Stderr, "miniagent: load session: %v\n", err)
		os.Exit(1)
	}
	if meta.Type == "" {
		// LoadSession 容忍尾行半写；meta.Type=="" 表示文件不存在或首行缺失——与接续路径一致报错，
		// 防 typo 静默成功。
		fmt.Fprintf(os.Stderr, "miniagent: session %q 不存在\n", id)
		os.Exit(1)
	}
	if err := replaySession(out, meta, msgs); err != nil {
		fmt.Fprintf(os.Stderr, "miniagent: replay: %v\n", err)
		os.Exit(1)
	}
}

// replaySession 遍历 msgs 按 role 翻译成 NDJSON 事件：
//   - assistant：step+1，每个 tool_call 发 tool_use（并记 callID→name 供后续 tool 消息反查）；
//     reasoning 发 reasoning_delta，content 发 text_delta；累加该步用量。
//   - tool：发 tool_result（name 由 map 反查，tool 消息本身只存 tool_call_id 不存 name）。
//   - user/system：运行时本就不发事件，跳过。
//
// 末尾发 result 汇总（steps=assistant turn 数、用量累加、finish 近似 "stop"、model 取 session 元数据）。
//
// 已知精度边界（用户认可，非缺陷）：text/reasoning 为整串一次发（session 无逐块切分）；
// tool_result 经双重截断（落盘成型串 + EmitToolResult 的 2000 字符上限）且无 exit_code
// （session tool 消息未存退出码）；压缩过的 session 回放的是压缩后快照；finish 恒 "stop"。
func replaySession(w io.Writer, meta miniagent.SessionMeta, msgs []miniagent.Message) error {
	if err := event.EmitSession(w, meta); err != nil {
		return err
	}
	step := 0
	lastText := ""
	callName := map[string]string{}
	var inTok, outTok int
	for _, m := range msgs {
		switch m.Role {
		case miniagent.RoleAssistant:
			step++
			lastText = m.Content
			for _, c := range m.ToolCalls {
				callName[c.ID] = c.Name
				if err := event.EmitToolUse(w, c.Name, c.Args); err != nil {
					return err
				}
			}
			if m.Reasoning != "" {
				if err := event.EmitDelta(w, step, miniagent.DeltaReasoning, m.Reasoning); err != nil {
					return err
				}
			}
			if m.Content != "" {
				if err := event.EmitDelta(w, step, miniagent.DeltaText, m.Content); err != nil {
					return err
				}
			}
			if m.Usage != nil {
				inTok += m.Usage.InputTokens
				outTok += m.Usage.OutputTokens
			}
		case miniagent.RoleTool:
			r := miniagent.ToolResult{Output: m.Content, IsError: m.IsError}
			if err := event.EmitToolResult(w, callName[m.ToolCallID], m.ToolCallID, r); err != nil {
				return err
			}
		}
	}
	result := miniagent.Result{
		Text:   lastText,
		Steps:  step,
		Finish: "stop",
		Usage:  miniagent.Usage{InputTokens: inTok, OutputTokens: outTok},
	}
	return event.EmitResult(w, result, meta.Model)
}
