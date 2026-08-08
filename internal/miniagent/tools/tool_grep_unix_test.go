//go:build !windows

package tools

import (
	"context"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// M3-3：FIFO（无写者）不能阻塞 grep——read/edit 有 IsRegular 守卫，grep 漏网时 os.Open(FIFO) 永久
// 阻塞至 fileOpTimeout 且泄漏 OS 线程。补 IsRegular 后 grepWalk 跳过 FIFO，正常搜到 regular 文件。
func TestGrepTool_SkipsFIFO(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo unsupported: %v", err)
	}
	writeTree(t, dir, map[string]string{"real.go": "foo\n"})
	// 小 timeout 兜底：即便守卫失效也快速返回而非挂 fileOpTimeout(30s)。
	res := GrepTool(dir, 2*time.Second, 0, 0).Call(context.Background(), `{"pattern":"foo"}`)
	if res.IsError {
		t.Fatalf("grep should skip FIFO and find real.go, got error: %s", res.Output)
	}
	if !strings.Contains(res.Output, "real.go:1:foo") {
		t.Errorf("real.go hit missing (FIFO not skipped?): %s", res.Output)
	}
}
