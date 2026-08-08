//go:build windows

package miniagent

import (
	"fmt"
	"os"
)

// OpenNoFollow 在 Windows 上回退为 Lstat+OpenFile 并拒绝最终分量为符号链接。
// 注意：Windows 无 O_NOFOLLOW 等价 syscall，Lstat→OpenFile 间存在理论 TOCTOU（攻击者此时把 path
// 替换为 symlink）。主平台 Linux/macOS 用 O_NOFOLLOW 单次 syscall 无此问题；Windows 是次要 fallback
// 平台（见 .golangci.yml 平台范围），此处接受该限制。彻底修复需 FILE_FLAG_OPEN_REPARSE_POINT +
// reparse point 检查（成本高、场景窄，未实现）。
// 留核心：session 子包（withSessionLock）与 config 子包（ReadFileLimited）共用。
func OpenNoFollow(path string, flag int, perm os.FileMode) (*os.File, error) {
	fi, err := os.Lstat(path)
	if err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%q is a symlink", path)
	}
	return os.OpenFile(path, flag, perm)
}
