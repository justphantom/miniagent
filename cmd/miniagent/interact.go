package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/justphantom/miniagent/internal/miniagent"
)

// runInteractive 在交互模式下循环读取 prompt（每行一个，空行跳过，EOF 退出）。有 -session
// 时以 session 文件为唯一真源：每轮 LoadSession → 单轮 Run → AppendNewMessages/RewriteMessages，
// 不在内存累积、不在外层过滤（过滤统一在 Run 入口，审查 v2 #3 + v3 #3）。无 -session 退化为内存累积。
// 单轮错误不退出会话（emit error 后继续），仅 EOF/空输入/信号取消/跨轮预算超限退出。
// 返回退出码：0=正常 EOF，1=跨轮预算超限，130=信号取消（POSIX SIGINT 习惯，审查 P3）。
func runInteractive(ctx context.Context, llm *miniagent.HTTPClient, baseCfg miniagent.LoopConfig, sessPath string, meta miniagent.SessionMeta, hooks miniagent.LoopHooks, logger *slog.Logger, reader *bufio.Reader) int {
	var (
		memHistory []miniagent.Message // 仅无 session 时用
		totalUsage miniagent.Usage     // 跨轮累计，超 MaxTotalTokens 停止交互（审查 P2 交互无跨轮预算）
	)
	for {
		// 循环顶检查信号取消：readTurn 阻塞在 stdin 时不响应 ctx，下轮顶部兜底捕获 SIGINT。
		if errors.Is(ctx.Err(), context.Canceled) {
			return 130
		}
		prompt, eof := readTurn(reader)
		if prompt == "" && eof {
			return 0
		}
		if sessPath != "" {
			_, h, err := miniagent.LoadSession(sessPath)
			if err != nil {
				if eerr := miniagent.EmitError(os.Stdout, err.Error()); eerr != nil {
					logger.Warn("emit error failed", "error", eerr)
				}
				if eof {
					return 0
				}
				continue
			}
			baseCfg.History = h
		} else {
			baseCfg.History = memHistory
		}
		result, err := miniagent.Run(ctx, llm, baseCfg, prompt, hooks, logger)
		if err != nil {
			// 信号取消（SIGINT/SIGTERM）走码 130 干净退出，不 emit error（审查 P3 SIGINT 退出码）。
			if errors.Is(err, context.Canceled) {
				return 130
			}
			if eerr := miniagent.EmitError(os.Stdout, err.Error()); eerr != nil {
				logger.Warn("emit error failed", "error", eerr)
			}
			if eof {
				return 0
			}
			continue
		}
		if err := miniagent.EmitResult(os.Stdout, result, baseCfg.Model); err != nil {
			logger.Warn("emit result failed", "error", err)
		}
		// thinking 跨轮固化：本轮降级则清 baseCfg，避免下轮重传原值再撞 400（审查 P2 thinking 跨轮）。
		if result.ThinkingDowngraded {
			baseCfg.ThinkingLevel = ""
		}
		// 跨轮累计 usage：超 MaxTotalTokens 停止交互循环（单轮上限仍由 Run 管）。
		totalUsage.InputTokens += result.Usage.InputTokens
		totalUsage.OutputTokens += result.Usage.OutputTokens
		// session 持久化：Compacted 时 rewrite 全量 transcript 真正丢弃被屏障中段（审查 P2 session
		// 文件永不压缩）；否则 append-only 追加 NewMessages。save 失败经 NDJSON 让消费方感知（P2 交互 save 静默）。
		if sessPath != "" {
			var saveErr error
			if result.Compacted {
				saveErr = miniagent.RewriteMessages(sessPath, meta, result.Messages)
			} else {
				saveErr = miniagent.AppendMessages(sessPath, meta, result.NewMessages)
			}
			if saveErr != nil {
				if eerr := miniagent.EmitError(os.Stdout, "save session: "+saveErr.Error()); eerr != nil {
					logger.Warn("emit error failed", "error", eerr)
				}
			}
		} else {
			memHistory = result.Messages
		}
		// 跨轮预算总闸：超限 emit error 后停止（与单轮 ErrBudgetExceeded 语义对齐，exit 1）。
		if baseCfg.MaxTotalTokens > 0 && totalUsage.InputTokens+totalUsage.OutputTokens > baseCfg.MaxTotalTokens {
			msg := fmt.Sprintf("跨轮累计 token 超限：input=%d output=%d > %d（停止交互）",
				totalUsage.InputTokens, totalUsage.OutputTokens, baseCfg.MaxTotalTokens)
			if eerr := miniagent.EmitError(os.Stdout, msg); eerr != nil {
				logger.Warn("emit error failed", "error", eerr)
			}
			return 1
		}
		if eof {
			return 0
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
