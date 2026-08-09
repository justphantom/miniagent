//go:build windows

package miniagent

import (
	"fmt"
	"os"
)

// OpenNoFollow falls back to Lstat+OpenFile on Windows, rejecting the final component when it is a symlink.
// Note: Windows has no O_NOFOLLOW-equivalent syscall, so there is a theoretical TOCTOU between Lstat and
// OpenFile (an attacker could replace path with a symlink in between). The main platforms Linux/macOS use a
// single O_NOFOLLOW syscall and have no such issue; Windows is a secondary fallback platform (see the
// platform scope in .golangci.yml), so this limitation is accepted here. A complete fix would require
// FILE_FLAG_OPEN_REPARSE_POINT + reparse point inspection (high cost, narrow scenario, not implemented).
// Kept in core: shared by the session subpackage (withSessionLock) and the config subpackage (ReadFileLimited).
func OpenNoFollow(path string, flag int, perm os.FileMode) (*os.File, error) {
	fi, err := os.Lstat(path)
	if err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%q is a symlink", path)
	}
	return os.OpenFile(path, flag, perm)
}
