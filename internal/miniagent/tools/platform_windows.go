//go:build windows

package tools

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// setPGID 在 Windows 上把子进程放入新进程组，以便超时 kill 能传递到整个子进程树。
func setPGID(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags = syscall.CREATE_NEW_PROCESS_GROUP
}

// killProcessGroup 终止子进程及其后代。
func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	pid := cmd.Process.Pid
	_ = exec.Command("taskkill", "/T", "/F", "/PID", fmt.Sprintf("%d", pid)).Run()
	_ = cmd.Process.Kill()
}

// openNoFollow 在 Windows 上回退为 Lstat+OpenFile 并拒绝最终分量为符号链接。
// 注意：Windows 无 O_NOFOLLOW 等价 syscall，Lstat→OpenFile 间存在理论 TOCTOU（攻击者此时把 path
// 替换为 symlink）。主平台 Linux/macOS 用 O_NOFOLLOW 单次 syscall 无此问题；Windows 是次要 fallback
// 平台，此处接受该限制。彻底修复需 FILE_FLAG_OPEN_REPARSE_POINT + reparse point 检查（成本高、场景窄，未实现）。
func openNoFollow(path string, flag int, perm os.FileMode) (*os.File, error) {
	fi, err := os.Lstat(path)
	if err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%q is a symlink", path)
	}
	return os.OpenFile(path, flag, perm)
}
