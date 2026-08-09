//go:build !windows

package tools

import (
	"os"
	"os/exec"
	"syscall"
)

// setPGID puts the child in a new process group so kill(-pgid) can reach
// the whole tree spawned by sh -c (otherwise grandchildren go orphan on
// timeout). Note: if the child then calls setsid to start its own session it leaves this process group,
// and the timeout kill(-pgid) cannot reach such orphans; a complete fallback requires cgroup or namespace isolation (review P3-6).
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

// openNoFollow opens path with O_NOFOLLOW, rejecting only a symlink as the final path component.
// Intermediate directories may still be symlinks and do not constitute a path boundary; under free mode isolation is the caller's responsibility.
func openNoFollow(path string, flag int, perm os.FileMode) (*os.File, error) {
	fd, err := syscall.Open(path, flag|syscall.O_NOFOLLOW, uint32(perm))
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}
