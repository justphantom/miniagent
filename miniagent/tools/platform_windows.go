//go:build windows

package tools

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// setPGID on Windows places the child in a new process group so the timeout kill can propagate to the whole child process tree.
func setPGID(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags = syscall.CREATE_NEW_PROCESS_GROUP
}

// killProcessGroup terminates the child process and its descendants.
func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	pid := cmd.Process.Pid
	_ = exec.Command("taskkill", "/T", "/F", "/PID", fmt.Sprintf("%d", pid)).Run()
	_ = cmd.Process.Kill()
}

// openNoFollow on Windows falls back to Lstat+OpenFile and rejects a symlink as the final path component.
// Note: Windows has no O_NOFOLLOW-equivalent syscall, so there is a theoretical TOCTOU between Lstat and OpenFile (an attacker swaps path
// for a symlink in between). The main platforms Linux/macOS use a single O_NOFOLLOW syscall with no such issue; Windows is a secondary fallback
// platform, and this limitation is accepted here. A complete fix requires FILE_FLAG_OPEN_REPARSE_POINT + a reparse-point check (high cost, narrow scenario, not implemented).
func openNoFollow(path string, flag int, perm os.FileMode) (*os.File, error) {
	fi, err := os.Lstat(path)
	if err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%q is a symlink", path)
	}
	return os.OpenFile(path, flag, perm)
}
