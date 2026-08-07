package main

import (
	"context"
	"fmt"
	"io"
	"os"
)

// maxPromptBytes 是 stdin prompt 的大小上限：无上限读取既撑内存，写回 session 后又会
// 撞 LoadSession 的 maxSessionBytes 上限，导致会话永久无法接续。取值小于 session 上限。
const maxPromptBytes = 1 << 20

// mustReadPrompt 读取 stdin prompt。读取放进 goroutine 并 select ctx：io.ReadAll 不可被信号中断，
// 否则交互式无管道或慢 producer 时 Ctrl+C 无法打断（signal.NotifyContext 的 channel 满后还会吞后续信号）。
// ctx 取消（SIGINT/SIGTERM）走码 130 干净退出，与主 Run 取消路径一致。读 goroutine 在进程退出后由 OS 回收。
func mustReadPrompt(ctx context.Context, r io.Reader) []byte {
	type readResult struct {
		prompt []byte
		err    error
	}
	done := make(chan readResult, 1)
	go func() {
		p, err := io.ReadAll(io.LimitReader(r, maxPromptBytes+1))
		done <- readResult{p, err}
	}()
	select {
	case <-ctx.Done():
		os.Exit(130)
	case res := <-done:
		if res.err != nil {
			fmt.Fprintf(os.Stderr, "miniagent: read stdin: %v\n", res.err)
			os.Exit(1)
		}
		if len(res.prompt) > maxPromptBytes {
			fmt.Fprintf(os.Stderr, "miniagent: stdin prompt 超过大小上限 %d 字节\n", maxPromptBytes)
			os.Exit(1)
		}
		if len(res.prompt) == 0 {
			fmt.Fprintln(os.Stderr, "miniagent: stdin is empty (send prompt via pipe or redirect)")
			os.Exit(1)
		}
		return res.prompt
	}
	// unreachable：select 两分支各经 os.Exit/return 终止；os.Exit 不被编译器识别为终止，故须安抚。
	return nil
}
