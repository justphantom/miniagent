//go:build !windows

package miniagent

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

// setPGID puts the child in a new process group so kill(-pgid) can reach
// the whole tree spawned by sh -c (otherwise grandchildren go orphan on
// timeout). 注意：子进程若再调 setsid 自立新会话会脱离该进程组，超时 kill(-pgid)
// 杀不到这类孤儿；彻底兜底需 cgroup 或命名空间隔离（审查 P3-6）。
func setPGID(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
}

// killProcessGroup kills every process in the child's process group.
// Falls back to direct PID kill if the group call fails.
func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	pgid := cmd.Process.Pid
	if e := syscall.Kill(-pgid, syscall.SIGKILL); e != nil {
		_ = syscall.Kill(pgid, syscall.SIGKILL)
	}
}

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
// （非固定 retry 次数）避免 time.Sleep 粒度累积致实际远超 5s。flock 是 inode 级 advisory
// lock，跨进程互斥；同进程内不互斥（POSIX 语义，单进程多 goroutine 并发写仍靠 O_APPEND
// 单次 write 原子性兜底，不依赖此锁）。
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
