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
// 单轮错误不退出会话（emit error 后继续），仅 EOF/空输入/信号取消/-max-duration 到期/跨轮预算超限退出。
// 返回退出码：0=干净 EOF（无任一轮 Run 失败）；1=跨轮预算超限/-max-duration 到期/EOF 前存在 Run 失败轮；
// 130=信号取消（POSIX SIGINT 习惯，审查 P3）。
func runInteractive(ctx context.Context, llm *miniagent.HTTPClient, baseCfg miniagent.LoopConfig, sessPath string, meta miniagent.SessionMeta, hooks miniagent.LoopHooks, logger *slog.Logger, reader *bufio.Reader) int {
	var (
		memHistory []miniagent.Message // 仅无 session 时用
		totalUsage miniagent.Usage     // 跨轮累计，超 MaxTotalTokens 停止交互（审查 P2 交互无跨轮预算）
		hadErr     bool                // 任一轮 Run 失败即置位；EOF 时据此返码 1（审查 P3 eof-on-error）
	)
	for {
		// 循环顶检查 ctx：readTurn 阻塞在 stdin 时不响应 ctx，下轮顶部兜底捕获 SIGINT(130)
		// 与 -max-duration 到期(1)（审查 P3 -max-duration 识别 DeadlineExceeded）。
		if code, ok := ctxExitCode(ctx.Err()); ok {
			return code
		}
		prompt, eof := readTurn(reader)
		if prompt == "" && eof {
			return eofExitCode(hadErr)
		}
		if sessPath != "" {
			_, h, err := miniagent.LoadSession(sessPath)
			if err != nil {
				if eerr := miniagent.EmitError(os.Stdout, err.Error()); eerr != nil {
					logger.Warn("emit error failed", "error", eerr)
				}
				if eof {
					return eofExitCode(hadErr)
				}
				continue
			}
			baseCfg.History = h
		} else {
			baseCfg.History = memHistory
		}
		result, err := miniagent.Run(ctx, llm, baseCfg, prompt, hooks, logger)
		// 无条件消费 result 的 usage/thinking：Run 经 defer 在所有返回路径（含 err）写满这些字段，
		// 故失败轮的花销也入跨轮预算、thinking 降级也在失败轮固化到 baseCfg（审查 P3 error 路径消费
		// result——跨轮预算不再漏算失败轮花销）。Save（AppendMessages/RewriteMessages）仍只在 err==nil 做。
		if result.ThinkingDowngraded {
			baseCfg.ThinkingLevel = ""
		}
		totalUsage.InputTokens += result.Usage.InputTokens
		totalUsage.OutputTokens += result.Usage.OutputTokens
		if err != nil {
			hadErr = true
			// ctx 取消/超时走干净退出，不 emit error：Canceled(SIGINT)→130，DeadlineExceeded
			// (-max-duration)→1（审查 P3 -max-duration 识别 DeadlineExceeded，与 main.go 非 -interactive 对齐）。
			if code, ok := ctxExitCode(ctx.Err()); ok {
				return code
			}
			if eerr := miniagent.EmitError(os.Stdout, err.Error()); eerr != nil {
				logger.Warn("emit error failed", "error", eerr)
			}
			if eof {
				return eofExitCode(hadErr)
			}
			continue
		}
		if err := miniagent.EmitResult(os.Stdout, result, baseCfg.Model); err != nil {
			logger.Warn("emit result failed", "error", err)
		}
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
		// 失败轮的 usage 已在上面无条件累加，故预算含失败轮花销。
		if baseCfg.MaxTotalTokens > 0 && totalUsage.InputTokens+totalUsage.OutputTokens > baseCfg.MaxTotalTokens {
			msg := fmt.Sprintf("跨轮累计 token 超限：input=%d output=%d > %d（停止交互）",
				totalUsage.InputTokens, totalUsage.OutputTokens, baseCfg.MaxTotalTokens)
			if eerr := miniagent.EmitError(os.Stdout, msg); eerr != nil {
				logger.Warn("emit error failed", "error", eerr)
			}
			return 1
		}
		if eof {
			return eofExitCode(hadErr)
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

// ctxExitCode 把 ctx 错误映射成交互循环退出码。第二返回值 false 表示 ctx 无错误（调用方继续
// 循环）；true 表示应立即返回该码：Canceled（SIGINT/SIGTERM）→130（POSIX 干净信号退出），
// DeadlineExceeded（-max-duration 到期）→1。循环顶与 Run 返回后统一用此判定，避免 -max-duration
// 到期时每次输入报 deadline error 却不退循环（审查 P3 -max-duration 识别 DeadlineExceeded）。
func ctxExitCode(ctxErr error) (int, bool) {
	if ctxErr == nil {
		return 0, false
	}
	if errors.Is(ctxErr, context.Canceled) {
		return 130, true
	}
	return 1, true // DeadlineExceeded 或其他 ctx 错误统一退 1
}

// eofExitCode 决定 EOF 时的退出码：曾有任一轮 Run 失败（hadErr）则 1，否则 0（干净 EOF）。
// 中间轮失败被 continue 也累积 hadErr，最终 EOF 时据此返码（审查 P3 eof-on-error 退出码）。
func eofExitCode(hadErr bool) int {
	if hadErr {
		return 1
	}
	return 0
}
