package main

import (
	"fmt"
	"io"
	"os"
)

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
