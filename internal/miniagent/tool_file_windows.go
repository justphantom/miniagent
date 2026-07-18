//go:build windows

package miniagent

import "os"

func openNoFollow(path string, flag int, perm os.FileMode) (*os.File, error) {
	// Windows lacks O_NOFOLLOW; fall back to best-effort Lstat check.
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, &os.PathError{Op: "open", Path: path, Err: os.ErrPermission}
	}
	return os.OpenFile(path, flag, perm)
}
