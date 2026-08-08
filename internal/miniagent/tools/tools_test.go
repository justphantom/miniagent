package tools

import (
	miniagent "github.com/justphantom/miniagent/internal/miniagent"
	"context"
	"testing"
	"time"
)

// runWithTimeout 的内部 goroutine 须 recover fn 的 panic，转为 IsError 结果而非崩进程。
// 回归：此前 go func(){ done <- fn(runCtx) }() 无 recover，而 safeCall 的 recover 在调用方
// goroutine 捕获不到内部 goroutine 的 panic，任一文件工具内部 panic 即打穿进程。
func TestRunWithTimeout_RecoversPanic(t *testing.T) {
	r := runWithTimeout(context.Background(), time.Second, "测试", func(_ context.Context) miniagent.ToolResult {
		panic("boom")
	})
	if !r.IsError {
		t.Fatalf("内部 goroutine 的 panic 应被 recover 成 IsError 结果: %+v", r)
	}
	if r.ExitCode != miniagent.ExitCodeNotSet {
		t.Errorf("ExitCode = %d, want miniagent.ExitCodeNotSet", r.ExitCode)
	}
}
