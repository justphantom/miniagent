//go:build !windows

package miniagent

import (
	"os"
	"syscall"
)

// OpenNoFollow opens path with O_NOFOLLOW, rejecting only when the final path component is a symlink.
// Intermediate directories may still be symlinks and do not constitute a path boundary; in free mode,
// isolation is the caller's responsibility.
// Kept in core: shared by the session subpackage (withSessionLock) and the config subpackage (ReadFileLimited).
func OpenNoFollow(path string, flag int, perm os.FileMode) (*os.File, error) {
	fd, err := syscall.Open(path, flag|syscall.O_NOFOLLOW, uint32(perm))
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}
