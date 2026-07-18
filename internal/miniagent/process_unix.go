//go:build !windows

package miniagent

import (
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

// killPGid kills every process in pgid. Fallback to direct PID kill on
// platforms where the group call fails.
func killPGid(pgid int) {
	if e := syscall.Kill(-pgid, syscall.SIGKILL); e != nil {
		_ = syscall.Kill(pgid, syscall.SIGKILL)
	}
}
