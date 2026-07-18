//go:build windows

package miniagent

import (
	"os/exec"
)

// setPGid is a no-op on Windows: the Windows process model has no POSIX
// process-group concept. Grouping the child tree via a Job Object is
// intentionally out of scope.
func setPGid(_ *exec.Cmd) {}

// killProcessGroup kills the direct child only. Windows grandchildren
// spawned by sh -c are NOT cleaned up here — proper tree cleanup needs
// Job Objects, which is a known limitation on this platform.
func killProcessGroup(cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}
