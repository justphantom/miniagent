package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/justphantom/miniagent/internal/miniagent"
)

// runInteractive 在交互模式下循环读取 prompt（每行一个，空行跳过，EOF 退出），每轮
// Run 并把返回的 Messages 作为下轮 History 累积；每轮成功后增量写回 session。单轮
// 错误不退出会话（emit error 后继续），仅 EOF/空输入退出。reader 由调用方传入，与
// checkApprove 共享，避免两 reader 竞争 stdin。
func runInteractive(ctx context.Context, llm *miniagent.HTTPClient, f *cliFlags, history []miniagent.Message, tools []miniagent.Tool, hooks miniagent.LoopHooks, logger *slog.Logger, reader *bufio.Reader) {
	for {
		prompt, eof := readTurn(reader)
		if prompt == "" && eof {
			return
		}
		result, err := miniagent.Run(ctx, llm, miniagent.LoopConfig{
			Model:          *f.model,
			System:         *f.system,
			MaxTokens:      *f.maxTokens,
			Tools:          tools,
			History:        history,
			MaxIterations:  *f.maxIterations,
			MaxTotalTokens: *f.maxTokensTotal,
			Stream:         *f.stream,
			ContextWindow:  *f.contextWindow,
		}, prompt, hooks, logger)
		if err != nil {
			if eerr := miniagent.EmitError(os.Stdout, err.Error()); eerr != nil {
				logger.Warn("emit error failed", "error", eerr)
			}
			if eof {
				return
			}
			continue
		}
		if err := miniagent.EmitResult(os.Stdout, result, *f.model); err != nil {
			logger.Warn("emit result failed", "error", err)
		}
		history = result.Messages
		if *f.session != "" {
			if err := miniagent.SaveSession(*f.session, history); err != nil {
				fmt.Fprintf(os.Stderr, "miniagent: save session: %v\n", err)
			}
		}
		if eof {
			return
		}
	}
}

// readTurn 按行读取一个 prompt：非空行即作为一个 turn 返回，空行跳过；EOF 时 eof=true。
// 每行一 prompt 的简模型，便于管道驱动与测试；长 prompt 可用 -session 接续。
func readTurn(r *bufio.Reader) (string, bool) {
	for {
		line, err := r.ReadString('\n')
		eof := err == io.EOF
		line = strings.TrimRight(line, "\n")
		if line != "" {
			return line, eof
		}
		if eof {
			return "", true
		}
	}
}

// checkApprove 按 mode 决定是否放行工具：all（默认）全放行；dangerous 仅 shell/write/edit
// 需确认；always 全部确认。确认从共享 reader 读一行（"y" 放行，其他/EOF 拒绝）——交互
// 模式用户输入，非交互（stdin 已被 prompt 读光）EOF 即拒绝危险工具，不静默放行。
func checkApprove(mode, name, input string, r *bufio.Reader) error {
	switch mode {
	case "", "all":
		return nil
	case "dangerous":
		if name != "shell" && name != "write" && name != "edit" {
			return nil
		}
	case "always":
	default:
		return nil
	}
	fmt.Fprintf(os.Stderr, "miniagent: approve %s %s? [y/N] ", name, input)
	line, err := r.ReadString('\n')
	if err != nil || strings.TrimSpace(strings.ToLower(line)) != "y" {
		return miniagent.ErrToolDenied
	}
	return nil
}

// maxPromptBytes 是 stdin prompt 的大小上限：无上限读取既撑内存，写回 session 后又会
// 撞 LoadSession 的 maxSessionBytes 上限，导致会话永久无法接续。取值小于 session 上限。
const maxPromptBytes = 1 << 20

func mustReadPrompt(r io.Reader) []byte {
	prompt, err := io.ReadAll(io.LimitReader(r, maxPromptBytes+1))
	if err != nil {
		fmt.Fprintf(os.Stderr, "miniagent: read stdin: %v\n", err)
		os.Exit(1)
	}
	if len(prompt) > maxPromptBytes {
		fmt.Fprintf(os.Stderr, "miniagent: stdin prompt 超过大小上限 %d 字节\n", maxPromptBytes)
		os.Exit(1)
	}
	if len(prompt) == 0 {
		fmt.Fprintln(os.Stderr, "miniagent: stdin is empty (send prompt via pipe or redirect)")
		os.Exit(1)
	}
	return prompt
}
