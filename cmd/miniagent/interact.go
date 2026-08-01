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

// runInteractive 在交互模式下循环读取 prompt（每行一个，空行跳过，EOF 退出）。有 -session
// 时以 session 文件为唯一真源：每轮 LoadSession → 单轮 Run → AppendNewMessages，不在内存
// 累积、不在外层过滤（过滤统一在 Run 入口，审查 v2 #3 + v3 #3）。无 -session 退化为内存累积。
// 单轮错误不退出会话（emit error 后继续），仅 EOF/空输入退出。
func runInteractive(ctx context.Context, llm *miniagent.HTTPClient, baseCfg miniagent.LoopConfig, sessPath string, meta miniagent.SessionMeta, hooks miniagent.LoopHooks, logger *slog.Logger, reader *bufio.Reader) {
	var memHistory []miniagent.Message // 仅无 session 时用
	for {
		prompt, eof := readTurn(reader)
		if prompt == "" && eof {
			return
		}
		if sessPath != "" {
			_, h, err := miniagent.LoadSession(sessPath)
			if err != nil {
				if eerr := miniagent.EmitError(os.Stdout, err.Error()); eerr != nil {
					logger.Warn("emit error failed", "error", eerr)
				}
				if eof {
					return
				}
				continue
			}
			baseCfg.History = h
		} else {
			baseCfg.History = memHistory
		}
		result, err := miniagent.Run(ctx, llm, baseCfg, prompt, hooks, logger)
		if err != nil {
			if eerr := miniagent.EmitError(os.Stdout, err.Error()); eerr != nil {
				logger.Warn("emit error failed", "error", eerr)
			}
			if eof {
				return
			}
			continue
		}
		if err := miniagent.EmitResult(os.Stdout, result, baseCfg.Model); err != nil {
			logger.Warn("emit result failed", "error", err)
		}
		if sessPath != "" {
			if err := miniagent.AppendMessages(sessPath, meta, result.NewMessages); err != nil {
				fmt.Fprintf(os.Stderr, "miniagent: save session: %v\n", err)
			}
		} else {
			memHistory = result.Messages
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
