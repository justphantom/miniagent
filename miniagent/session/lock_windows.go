//go:build windows

package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
	"unsafe"

	"github.com/justphantom/miniagent/miniagent"
)

var (
	kernel32         = syscall.NewLazyDLL("kernel32.dll")
	procLockFileEx   = kernel32.NewProc("LockFileEx")
	procUnlockFileEx = kernel32.NewProc("UnlockFileEx")
)

const (
	lockfileExclusiveLock   = 0x00000002
	lockfileFailImmediately = 0x00000001
)

// lockSession takes an exclusive byte-range lock on the session file on Windows.
func lockSession(f *os.File) error {
	h := syscall.Handle(f.Fd())
	var ov syscall.Overlapped
	deadline := time.Now().Add(lockSessionTotal)
	for time.Now().Before(deadline) {
		r1, _, err := procLockFileEx.Call(
			uintptr(h),
			uintptr(lockfileExclusiveLock|lockfileFailImmediately),
			0,
			1,
			0,
			uintptr(unsafe.Pointer(&ov)),
		)
		if r1 != 0 {
			return nil
		}
		if err != syscall.Errno(33) && err != syscall.ERROR_IO_PENDING {
			return err
		}
		time.Sleep(lockSessionInterval)
	}
	return errors.New("session lock busy: another process holds it and did not release within 5s")
}

const (
	lockSessionTotal    = 5 * time.Second
	lockSessionInterval = 100 * time.Millisecond
)

// unlockSession releases the lock held by lockSession.
func unlockSession(f *os.File) error {
	h := syscall.Handle(f.Fd())
	var ov syscall.Overlapped
	r1, _, err := procUnlockFileEx.Call(
		uintptr(h),
		0,
		1,
		0,
		uintptr(unsafe.Pointer(&ov)),
	)
	if r1 != 0 {
		return nil
	}
	return err
}

// withSessionLock opens path, takes a lock, and runs fn before unlocking and closing. OpenNoFollow is kept in
// core (shared by session/config).
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
