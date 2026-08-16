package tools

import (
	"context"
	"io"
	"os/exec"
)

// runLimitedOutput 是 git/go/npm/lint 共享的 exec 基建：合并 stdout/stderr 进滑动窗口累加器，
// ctx 结束杀进程组并关闭管道解阻塞读循环。跨块 UTF-8 边界由 pending 缓冲处理——32KB Read
// 可停在多字节字符中间，直接 string(buf[:n]) 会在窗口起点留下半个 rune（首个可见字符成 U+FFFD）。
func runLimitedOutput(ctx context.Context, cmd *exec.Cmd, maxOutputChars int) (string, error) {
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw
	if err := cmd.Start(); err != nil {
		_ = pw.Close()
		return "", err
	}
	waitErr := make(chan error, 1)
	go func() {
		waitErr <- cmd.Wait()
		_ = pw.Close()
	}()
	go func() {
		<-ctx.Done()
		killProcessGroup(cmd)
		_ = pw.Close()
	}()
	accum := newOutputAccum(maxOutputChars, 0, "", "miniagent_exec_")
	var pending []byte // 上一次 Read 尾部的不完整 UTF-8 序列，与下一块拼接后再转 string
	buf := make([]byte, 32*1024)
	for {
		n, rerr := pr.Read(buf)
		if n > 0 {
			chunk := append(pending, buf[:n]...)
			cut := len(chunk)
			for cut > 0 && !utf8FullRune(chunk[cut-1:]) {
				cut--
			}
			pending = append([]byte{}, chunk[cut:]...)
			_ = accum.write(string(chunk[:cut]))
		}
		if rerr != nil {
			break
		}
	}
	if len(pending) > 0 {
		_ = accum.write(string(pending)) // 收尾残片（命令以截断的多字节字符结束时）
	}
	_ = accum.closeSink()
	werr := <-waitErr
	killProcessGroup(cmd)
	return accum.finalize(maxOutputChars), werr
}

// utf8FullRune 报告 b（以某字符的尾字节结尾的切片）是否构成完整 rune：
// 尾字节为 ASCII 或多字节首字节时必完整；为连续字节时向前找首字节比对期望长度。
func utf8FullRune(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	c := b[len(b)-1]
	if c < 0x80 {
		return true
	}
	if c&0xC0 != 0x80 {
		return true
	}
	for i := len(b) - 1; i >= 0 && i >= len(b)-4; i-- {
		if b[i]&0xC0 != 0x80 {
			want := 1
			switch {
			case b[i]&0xE0 == 0xC0:
				want = 2
			case b[i]&0xF0 == 0xE0:
				want = 3
			case b[i]&0xF8 == 0xF0:
				want = 4
			}
			return len(b)-i == want
		}
	}
	return false
}
