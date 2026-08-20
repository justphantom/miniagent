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

// M3-3: a FIFO (with no writer) must not block grep — read/edit have an IsRegular guard; when grep lacked it, os.Open(FIFO) blocked
// permanently until fileOpTimeout and leaked an OS thread. With the IsRegular guard added, grepWalk skips the FIFO and finds regular files normally.
func TestGrepTool_SkipsFIFO(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo unsupported: %v", err)
	}
	writeTree(t, dir, map[string]string{"real.go": "foo\n"})
	// Small-timeout fallback: even if the guard fails, it returns quickly instead of hanging for fileOpTimeout(30s).
	res := GrepTool(dir, 2*time.Second, 0, 0).Call(context.Background(), `{"pattern":"foo"}`)
	if res.IsError {
		t.Fatalf("grep should skip FIFO and find real.go, got error: %s", res.Output)
	}
	if !strings.Contains(res.Output, "real.go:1:foo") {
		t.Errorf("real.go hit missing (FIFO not skipped?): %s", res.Output)
	}
}
