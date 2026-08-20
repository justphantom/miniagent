//go:build !windows

package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/justphantom/miniagent/miniagent"
)

// lockSession takes an exclusive advisory lock on the session file. LOCK_EX|LOCK_NB is non-blocking + short
// polling: a hung lock holder will not make this process block forever (review P2 flock blocking); polling once
// per 100ms within 5s tolerates transient cross-process contention, and on timeout returns a clear error for
// AppendMessages/RewriteMessages to handle. A deadline check (instead of a fixed retry count) avoids time.Sleep
// granularity accumulating so the actual wait far exceeds 5s. flock is an inode-level advisory lock, bound to
// the open file description rather than the process: mutually exclusive across processes, and two independently
// open()ed fds within the same process are also mutually exclusive (unlike the process-level semantics of POSIX
// fcntl). CLI session persistence is a sequential single writer with no intra-process concurrency, so this mutual
// exclusion does not affect the normal path; Windows uses a LockFileEx byte-range lock, which is not mutually
// exclusive within the same process (see lock_windows.go).
func lockSession(f *os.File) error {
	deadline := time.Now().Add(lockSessionTotal)
	for time.Now().Before(deadline) {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return nil
		}
		// Only EWOULDBLOCK (LOCK_NB failed to acquire, i.e. real contention) is retried; permanent errors such
		// as EBADF/ENOSYS/EFAULT are returned immediately — to avoid busy-polling for 5s on a filesystem/container
		// without flock support (ENOSYS) (aligned with the windows version's distinction of permanent errors).
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return fmt.Errorf("lock session: %w", err)
		}
		time.Sleep(lockSessionInterval)
	}
	return errors.New("session lock busy: another process holds it and did not release within 5s")
}

const (
	lockSessionTotal    = 5 * time.Second // upper bound for waiting on cross-process lock contention
	lockSessionInterval = 100 * time.Millisecond
)

// unlockSession releases the lock held by lockSession; must be called before Close.
func unlockSession(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}

// withSessionLock opens path (O_NOFOLLOW + 0o600), takes a flock, and runs fn before unlocking and closing.
// Shared by AppendMessages (O_APPEND) and RewriteMessages (O_WRONLY rename while holding the lock): unified
// lockSession failure handling (non-blocking poll timeout returns a clear error) + MkdirAll 0o700 to prevent
// group-writable directories + O_NOFOLLOW to reject a symlink in the final path component (review P3 session
// hardening + P2 flock blocking). OpenNoFollow is kept in core (shared by session/config).
func withSessionLock(path string, flag int, fn func(*os.File) error) error {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create session directory: %w", err)
		}
	}
	f, err := miniagent.OpenNoFollow(path, flag, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if err := lockSession(f); err != nil {
		return fmt.Errorf("lock session %q: %w", path, err)
	}
	defer func() { _ = unlockSession(f) }()
	return fn(f)
}
