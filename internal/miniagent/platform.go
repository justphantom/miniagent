//go:build !windows

package miniagent

import (
	"os"
	"os/exec"
	"syscall"
)

// setPGID puts the child in a new process group so kill(-pgid) can reach
// the whole tree spawned by sh -c (otherwise grandchildren go orphan on
// timeout).
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
