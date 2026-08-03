package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/justphantom/miniagent/internal/miniagent"
)

// runInteractive 在交互模式下循环读取 prompt（每行一个，空行跳过，EOF 退出）。有 -session
// 时以 session 文件为唯一真源：每轮 LoadSession → 单轮 Run → AppendNewMessages/RewriteMessages，
// 不在内存累积、不在外层过滤（过滤统一在 Run 入口，审查 v2 #3 + v3 #3）。无 -session 退化为内存累积。
// 单轮错误不退出会话（emit error 后继续），仅 EOF/空输入/信号取消/-max-duration 到期/跨轮预算超限退出。
// 返回退出码：0=干净 EOF（无任一轮 Run 失败）；1=跨轮预算超限/-max-duration 到期/EOF 前存在 Run 失败轮；
// 130=信号取消（POSIX SIGINT 习惯，审查 P3）。
// runCtx 由调用方提供（含 -max-duration 超时）；本函数内部再注册 SIGINT/SIGTERM 信号处理。
func runInteractive(runCtx context.Context, chat *miniagent.ChatClient, stream *miniagent.StreamClient, baseCfg miniagent.LoopConfig, sessPath string, meta miniagent.SessionMeta, hooks miniagent.LoopHooks, logger *slog.Logger, reader *bufio.Reader, mem *memoryExtractor) int {
	// 使用独立信号通道管理交互循环的取消，避免 main.go 的 NotifyContext 与 save 期间的
	// signal.Ignore/Notify 互相干扰。save 前 Ignore 阻止重复 SIGINT 杀掉保存过程；save 后
	// 重新 Notify 本通道，恢复信号取消能力。done 用于在 runInteractive 返回时结束监听 goroutine，
	// 避免无信号时 goroutine 永久阻塞泄漏。
	ctx, cancel := context.WithCancel(runCtx)
	sigCh := make(chan os.Signal, 1)
	done := make(chan struct{})
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer func() {
		close(done)
		signal.Stop(sigCh)
		cancel()
	}()
	go func() {
		select {
		case <-sigCh:
			cancel()
		case <-done:
		}
	}()
	var (
		memHistory []miniagent.Message // 仅无 session 时用
		totalUsage miniagent.Usage     // 跨轮累计，超 MaxTotalTokens 停止交互（审查 P2 交互无跨轮预算）
		hadErr     bool                // 任一轮 Run 失败即置位；EOF 时据此返码 1（审查 P3 eof-on-error）
		lastResult miniagent.Result    // 最近一次成功 Run 的结果，供 EOF 时抽取记忆
	)
	// 会话结束时（EOF/预算/超时）抽取一次记忆；用户信号中断（context.Canceled）不抽取。
	defer func() {
		if mem == nil {
			return
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			return
		}
		if len(lastResult.Messages) > 0 {
			mem.extract(ctx, lastResult.Messages)
		}
	}()
	for {
		// 循环顶检查 ctx：readTurn 阻塞在 stdin 时不响应 ctx，下轮顶部兜底捕获 SIGINT(130)
		// 与 -max-duration 到期(1)（审查 P3 -max-duration 识别 DeadlineExceeded）。
		if code, ok := ctxExitCode(ctx.Err()); ok {
			return code
		}
		prompt, eof, oversized := readTurn(reader)
		// P3-1：单行超 maxPromptBytes（无界读取 OOM 防护）—— emit 拒绝信号并跳过该轮，
		// 不把超长内容当 prompt 喂给 Run，不污染 session。对齐 LoadSession 失败的
		// error/continue 路径：emit NDJSON error 后 eof 则退出，否则 continue。
		if oversized {
			msg := fmt.Sprintf("交互输入单行超过大小上限 %d 字节（已跳过该轮）", maxPromptBytes)
			if eerr := miniagent.EmitError(os.Stdout, msg); eerr != nil {
				logger.Warn("emit error failed", "error", eerr)
			}
			if eof {
				return eofExitCode(hadErr)
			}
			continue
		}
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
		result, err := miniagent.Run(ctx, chat, stream, baseCfg, prompt, hooks, logger)
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
		// 文件永不压缩）；否则 append-only 追加 NewMessages。save 期间忽略信号，完成后重新 Notify
		// 本通道，恢复后续 loop 的信号响应。
		if sessPath != "" {
			signal.Ignore(syscall.SIGINT, syscall.SIGTERM)
			var saveErr error
			if result.Compacted {
				saveErr = miniagent.RewriteMessages(sessPath, meta, result.Messages)
			} else {
				saveErr = miniagent.AppendMessages(sessPath, meta, result.NewMessages)
			}
			signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
			if saveErr != nil {
				if eerr := miniagent.EmitError(os.Stdout, "save session: "+saveErr.Error()); eerr != nil {
					logger.Warn("emit error failed", "error", eerr)
				}
			}
		} else {
			memHistory = result.Messages
		}
		lastResult = result
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
//
// 单行长度读取过程中封顶 maxPromptBytes（与 mustReadPrompt 的 stdin 上限对称）：用
// (*bufio.Reader).ReadByte 逐字节累计进 strings.Builder，达上限仍未换行则置 oversized=true。
// 复用 maxPromptBytes 常量，不引入新魔数。必须在「读取过程中」封顶——先读完再查 len 则
// OOM 已发生（审查 P3-1：管道灌入超大无换行输入可致无界分配 OOM）。oversized=true 时该行
// 内容被丢弃（绝不当作 prompt 喂给 Run、不污染 session），由调用方 emit 拒绝信号并 continue。
func readTurn(r *bufio.Reader) (prompt string, eof bool, oversized bool) {
	for {
		// 逐字节读单行：遇 '\n' 结束；任何读错误（含 io.EOF）均视作读取终止。
		var sb strings.Builder
		over := false
		hitEOF := false
		for {
			b, err := r.ReadByte()
			if err != nil {
				hitEOF = true
				break
			}
			if b == '\n' {
				break
			}
			// 达上限仍未换行：置 over 停止写入 sb 防 OOM，但继续消费到行尾，
			// 丢弃超限字节以保持下一轮起点干净（避免残留被当成下一轮 prompt）。
			if !over && sb.Len() >= maxPromptBytes {
				over = true
			}
			if !over {
				sb.WriteByte(b)
			}
		}
		if over {
			// 超限行已读完丢弃；eof 反映是否在丢弃过程中同时触底，供调用方决定退出还是 continue。
			return "", hitEOF, true
		}
		line := sb.String()
		if line != "" {
			return line, hitEOF, false
		}
		if hitEOF {
			// 空行 + EOF（或读取起始即 EOF）：clean EOF。
			return "", true, false
		}
		// 空行：跳过继续读下一行。
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
