//go:build windows

package miniagent

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
	"unsafe"
)

// setPGID 在 Windows 上把子进程放入新进程组，以便超时 kill 能传递到整个子进程树。
func setPGID(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags = syscall.CREATE_NEW_PROCESS_GROUP
}

// killProcessGroup 终止子进程及其后代。
func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	pid := cmd.Process.Pid
	_ = exec.Command("taskkill", "/T", "/F", "/PID", fmt.Sprintf("%d", pid)).Run()
	_ = cmd.Process.Kill()
}

// openNoFollow 在 Windows 上回退为 os.OpenFile 并拒绝最终分量为符号链接。
func openNoFollow(path string, flag int, perm os.FileMode) (*os.File, error) {
	fi, err := os.Lstat(path)
	if err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%q is a symlink", path)
	}
	return os.OpenFile(path, flag, perm)
}

var (
	kernel32         = syscall.NewLazyDLL("kernel32.dll")
	procLockFileEx   = kernel32.NewProc("LockFileEx")
	procUnlockFileEx = kernel32.NewProc("UnlockFileEx")
)

const (
	lockfileExclusiveLock   = 0x00000002
	lockfileFailImmediately = 0x00000001
)

// lockSession 在 Windows 上对 session 文件加排他字节区间锁。
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
	return errors.New("session 锁繁忙：另一进程持有且 5s 内未释放")
}

const (
	lockSessionTotal    = 5 * time.Second
	lockSessionInterval = 100 * time.Millisecond
)

// unlockSession 释放 lockSession 持有的锁。
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

// withSessionLock 打开 path 并加锁，执行 fn 后解锁关闭。
func withSessionLock(path string, flag int, fn func(*os.File) error) error {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("创建 session 目录：%w", err)
		}
	}
	f, err := openNoFollow(path, flag, 0o600)
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
