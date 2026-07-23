//go:build !windows

package miniagent

import (
	"os"
	"os/exec"
	"syscall"
)

// setPGid puts the child in a new process group so kill(-pgid) can reach
// the whole tree spawned by sh -c (otherwise grandchildren go orphan on
// timeout).
func setPGid(cmd *exec.Cmd) {
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

// openNoFollow 以 O_NOFOLLOW 打开 path，拒绝最终路径是符号链接的情形，
// 防止经符号链接编辑/读取 workspace 之外的文件。
func openNoFollow(path string, flag int, perm os.FileMode) (*os.File, error) {
	fd, err := syscall.Open(path, flag|syscall.O_NOFOLLOW, uint32(perm))
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}
