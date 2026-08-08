//go:build !windows

package miniagent

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// openNoFollow 以 O_NOFOLLOW 打开 path，仅拒绝最终路径分量是符号链接。
// 中间目录仍可是符号链接，不构成路径边界；free 模式下隔离由调用方保证。
func openNoFollow(path string, flag int, perm os.FileMode) (*os.File, error) {
	fd, err := syscall.Open(path, flag|syscall.O_NOFOLLOW, uint32(perm))
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

// lockSession 对 session 文件加排他 advisory 锁。LOCK_EX|LOCK_NB 非阻塞 + 短轮询：
// 持锁进程挂死时不会让本进程永久阻塞（审查 P2 flock 阻塞），5s 内 100ms 一次容忍跨进程
// 瞬时竞争，超时返回明确 error 让 AppendMessages/RewriteMessages 处理。用 deadline 判定
// （非固定 retry 次数）避免 time.Sleep 粒度累积致实际远超 5s。flock 是 inode 级 advisory lock，
// 绑定 open file description 而非进程：跨进程互斥，同进程内两个独立 open() 的 fd 也互斥
// （区别于 POSIX fcntl 的进程级语义）。CLI 的 session 落盘是顺序单写者、无同进程并发，故此
// 互斥性不影响正常路径；Windows 用 LockFileEx 字节区间锁、同进程内不互斥（见 platform_windows.go）。
func lockSession(f *os.File) error {
	deadline := time.Now().Add(lockSessionTotal)
	for time.Now().Before(deadline) {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return nil
		}
		// 仅 EWOULDBLOCK（LOCK_NB 抢锁失败，即真竞争）才重试；EBADF/ENOSYS/EFAULT 等永久错误
		// 立即返回——避免在无 flock 支持的文件系统/容器（ENOSYS）上空轮询 5s（与 windows 版区分永久错误对齐）。
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return fmt.Errorf("lock session: %w", err)
		}
		time.Sleep(lockSessionInterval)
	}
	return errors.New("session 锁繁忙：另一进程持有且 5s 内未释放")
}

const (
	lockSessionTotal    = 5 * time.Second // 跨进程持锁竞争的等待上限
	lockSessionInterval = 100 * time.Millisecond
)

// unlockSession 释放 lockSession 持有的锁，须在 Close 前调用。
func unlockSession(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}

// withSessionLock 打开 path（O_NOFOLLOW + 0o600）并加 flock，执行 fn 后解锁关闭。AppendMessages
// （O_APPEND）与 RewriteMessages（O_WRONLY 持锁期间 rename）共用：统一 lockSession 失败处理
// （非阻塞轮询超时返回明确 error）+ MkdirAll 0o700 防 group-writable 目录 + O_NOFOLLOW 拒最终
// 分量 symlink（审查 P3 session 硬化 + P2 flock 阻塞）。
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
