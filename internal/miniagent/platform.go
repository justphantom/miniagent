//go:build !windows

package miniagent

import (
	"os"
	"os/exec"
	"syscall"
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

// lockSession 对 session 文件加排他 advisory 锁（LOCK_EX），防两个 miniagent 进程
// 并发 append 同一 session 致 bufio.Flush 的多次 write(2) 在行边界交织产生中间非法
// JSON 行（LoadSession 把中间损坏当硬错误，整文件不可用）（审查 P2-13）。Linux/macOS
// 通用；flock 是 inode 级 advisory lock，跨进程互斥，同进程内不互斥。
func lockSession(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
}

// unlockSession 释放 lockSession 持有的锁，须在 Close 前调用。
func unlockSession(f *os.File) error {
	return syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
}
