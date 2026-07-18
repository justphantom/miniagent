//go:build windows

package miniagent

import (
	"os/exec"
	"syscall"
)

// setPGid is a no-op on Windows: there is no fork/exec process group model
// matching POSIX. cmd.Process.Kill already targets the direct child.
func setPGid(_ *exec.Cmd) {}

// killPGid falls back to killing the single child PID. Windows job objects
// would be the correct tool for full tree cleanup, but that is out of scope.
func killPGid(pid int) {
	_ = syscall.Kill(pid, syscall.SIGKILL)
}
