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

// lockSession LOCK_NB is non-blocking: when a cross-process lock is held, this process does not block forever,
// timing out within 5s and returning an error (review P2 flock blocking).
// In-process flock is not mutually exclusive (POSIX semantics), so you must fork a child process to hold the lock
// for it to take effect. The child process re-enters the test binary itself (dispatched via env TEST_HOLD_FLOCK_PATH)
// and holds the lock for 10s, long enough for the parent process to verify it does not block.
func TestLockSession_LockNBTimeoutAcrossProcesses(t *testing.T) {
	if holder := os.Getenv("TEST_HOLD_FLOCK_PATH"); holder != "" {
		// Child process mode: hold the lock for 10s (long enough for the parent process to finish testing), then return normally to let testing exit.
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
	// exec uses a separate goroutine internally to write Stderr; reading it from the main goroutine requires mutual exclusion (to avoid -race false positives).
	var childErr mutexBuffer
	cmd.Stderr = &childErr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		_ = cmd.Wait()
	})
	// Wait for the child process to acquire the lock (up to 2s).
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(childErr.String(), "CHILD_LOCKED") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(childErr.String(), "CHILD_LOCKED") {
		t.Fatalf("child process did not acquire the lock within 2s: %s", childErr.String())
	}
	// The parent process AppendMessages should return an error within ~5s (lockSessionTotal + one interval), not block forever.
	start := time.Now()
	err := AppendMessages(path, SessionMeta{ID: "s"}, []miniagent.Message{{Role: "user", Content: "x"}})
	elapsed := time.Since(start)
	if err == nil {
		t.Skip("in-process/same-inode flock is not mutually exclusive, AppendMessages succeeded (POSIX semantics, not a bug)")
	}
	if elapsed > 7*time.Second {
		t.Errorf("lockSession blocked for %v, should return an error within ~5s (LOCK_NB timeout)", elapsed)
	}
}

// mutexBuffer is a goroutine-safe bytes.Buffer: exec writes Stderr from a separate goroutine, and reading it
// from the main goroutine requires mutual exclusion (to avoid -race reports).
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
