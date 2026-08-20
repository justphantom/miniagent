package tools

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"
)

// utf8MaxSeq 是最长 UTF-8 序列（4 字节）：drainPipe 的 pending 缓冲上限与 utf8FullRune 的回扫上界。
const utf8MaxSeq = 4

// waitOutputTimeout 有界地等待 cmd.Wait：进程组被杀后，setsid 逃逸出组的孤孙子进程（守护
// 进程双 fork 的典型形态）仍持有管道 fd，exec 的 copier 不结束、cmd.Wait 会远超期限地阻塞。
// 按 timeout+2s（与 runWithTimeout 的宽限窗对齐）放弃等待，保证调用方有界返回；此后泄漏的
// 子进程/等待 goroutine 是已知残留（platform.go P3-6：根治需 cgroup/namespace 级隔离）。
func waitOutputTimeout(waitErr <-chan error, timeout time.Duration) error {
	select {
	case err := <-waitErr:
		return err
	case <-time.After(timeout + 2*time.Second):
		return errors.New("output pipe still held after process-group kill (daemonized grandchild?); giving up wait")
	}
}

// drainPipe 读 pr 至 EOF，经 pending 缓冲缝合跨块的多字节字符：32KB Read 可停在 rune 中间，
// 直接 string(buf[:n]) 会在滑动窗口起点留下半个 rune（首个可见字符成 U+FFFD）。cut 回退判据
// 是前缀 chunk[:cut] 是否终结于 rune 边界（旧实现误用 chunk[cut-1:]——该切片恒以 chunk 末字节
// 结尾，会把已完整 rune 的续字节也挂起，写出的 chunk 永远错位，窗口逐出后尾部以半个 rune 开头）。
// pending 至多保留 utf8MaxSeq 字节：非 UTF-8 流（二进制/GBK）尾部连续字节回扫找不到首字节，
// cut 会一路退到 0，若不设下限整个流滞留 pending、窗口永不逐出（每次 Read 全量重拷 + 全部
// 输出替换成 U+FFFD）。
func drainPipe(pr *io.PipeReader, accum *outputAccum) {
	var pending []byte // 上一次 Read 尾部的不完整 UTF-8 序列，与下一块拼接后再转 string
	buf := make([]byte, 32*1024)
	for {
		n, rerr := pr.Read(buf)
		if n > 0 {
			chunk := append(pending, buf[:n]...)
			cut := len(chunk)
			for cut > 0 && !utf8FullRune(chunk[:cut]) {
				cut--
			}
			if cut < len(chunk)-utf8MaxSeq {
				cut = len(chunk) - utf8MaxSeq
			}
			pending = append([]byte{}, chunk[cut:]...)
			// 非 UTF-8 流（二进制/GBK：cut 被钉在 4 字节下限）按十六进制转义呈现——逐字节
			// string() 会把每个无效字节变成 3 字节 U+FFFD，纯二进制输出把字符上限放大 3 倍
			// 且全是替换符，十六进制每字节 3 字符可见且无信息损失。
			_ = accum.write(escapeNonUTF8(chunk[:cut]))
		}
		if rerr != nil {
			break
		}
	}
	if len(pending) > 0 {
		// 收尾残片（命令以截断的多字节字符结束时）；被 4 字节下限钉住的残片同理走转义。
		_ = accum.write(escapeNonUTF8(pending))
	}
}

// escapeNonUTF8 把 b 中所有无效 UTF-8 字节序列按 "XX " 十六进制转义，有效部分原样保留：
// 二进制输出不再变成成片 U+FFFD（每字节 3 字节且不可读）。utf8.DecodeRune 在首个无效处
// 返回 RuneError，据此推进到下一字节继续找有效序列，有效文本不会被误转义。
func escapeNonUTF8(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	var sb strings.Builder
	for len(b) > 0 {
		r, size := utf8.DecodeRune(b)
		if r == utf8.RuneError && size == 1 {
			fmt.Fprintf(&sb, "%02X ", b[0])
			b = b[1:]
			continue
		}
		sb.Write(b[:size])
		b = b[size:]
	}
	return sb.String()
}

// utf8FullRune 报告 b 的前缀是否恰好终结于 rune 边界（判定对象是 chunk[:cut] 这类前缀，
// 而非任意子序列）：末字节为 ASCII 时必是边界；为多字节首字节时必不是（孤立首字节从不
// 构成完整 rune——曾在此误判为真，32KB Read 恰停在首字节后时残片被先行写入，窗口逐出后
// 尾部以半个 rune 开头）；为连续字节时向前找首字节比对期望长度（rune 至多 4 字节，故回扫
// 上界为 len(b)-4）。
func utf8FullRune(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	c := b[len(b)-1]
	if c < 0x80 {
		return true
	}
	if c&0xC0 != 0x80 {
		return false
	}
	for i := len(b) - 1; i >= 0 && i >= len(b)-utf8MaxSeq; i-- {
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
