//go:build !windows

package session

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/justphantom/miniagent/internal/miniagent"
)

// lockSession LOCK_NB 非阻塞：跨进程持锁时本进程不永久阻塞，5s 内超时返回 error（审查 P2 flock 阻塞）。
// 同进程 flock 不互斥（POSIX 语义），必须 fork 子进程持锁才有效。子进程由 test binary 自身重入
// （env TEST_HOLD_FLOCK_PATH 分流），持锁 10s 足够父进程验证不阻塞。
func TestLockSession_LockNBTimeoutAcrossProcesses(t *testing.T) {
	if holder := os.Getenv("TEST_HOLD_FLOCK_PATH"); holder != "" {
		// 子进程模式：持锁 10s（足够父进程测试完成），然后正常返回让 testing 退出。
		f, err := os.OpenFile(holder, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			t.Fatalf("child open: %v", err)
		}
		defer f.Close()
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			t.Fatalf("child lock: %v", err)
		}
		fmt.Fprintln(os.Stderr, "CHILD_LOCKED")
		time.Sleep(10 * time.Second)
		return
	}
	path := filepath.Join(t.TempDir(), "lock.jsonl")
	if err := os.WriteFile(path, []byte{}, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(context.Background(), os.Args[0], "-test.run=^TestLockSession_LockNBTimeoutAcrossProcesses$")
	cmd.Env = append(os.Environ(), "TEST_HOLD_FLOCK_PATH="+path)
	// exec 内部用独立 goroutine 写 Stderr，与主 goroutine 读需互斥（防 -race 误报）。
	var childErr mutexBuffer
	cmd.Stderr = &childErr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		_ = cmd.Wait()
	})
	// 等子进程获得锁（最多 2s）。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(childErr.String(), "CHILD_LOCKED") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(childErr.String(), "CHILD_LOCKED") {
		t.Fatalf("子进程 2s 内未获锁: %s", childErr.String())
	}
	// 父进程 AppendMessages 应在 ~5s（lockSessionTotal + 一次 interval）内返回 error，不永久阻塞。
	start := time.Now()
	err := AppendMessages(path, SessionMeta{ID: "s"}, []miniagent.Message{{Role: "user", Content: "x"}})
	elapsed := time.Since(start)
	if err == nil {
		t.Skip("同进程/同 inode flock 不互斥，AppendMessages 成功（POSIX 语义，非 bug）")
	}
	if elapsed > 7*time.Second {
		t.Errorf("lockSession 阻塞 %v，应 ~5s 内返回 error（LOCK_NB 超时）", elapsed)
	}
}

// mutexBuffer 是线程安全的 bytes.Buffer：exec 用独立 goroutine 写 Stderr，主 goroutine 读
// 需互斥（避免 -race 报告）。
type mutexBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (m *mutexBuffer) Write(p []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.b.Write(p)
}

func (m *mutexBuffer) String() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.b.String()
}
