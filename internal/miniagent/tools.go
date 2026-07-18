package miniagent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// resolveUnderRoot cleans p and ensures the result stays under root, both on
// the string level and on the filesystem level (EvalSymlinks on the parent dir
// and on the final path if it exists).
func resolveUnderRoot(root, p string) (string, error) {
	clean := filepath.Clean(p)
	var full string
	if filepath.IsAbs(clean) {
		full = clean
	} else {
		full = filepath.Join(root, clean)
	}
	if err := checkUnderRoot(root, full, p); err != nil {
		return "", err
	}
	realParent, err := filepath.EvalSymlinks(filepath.Dir(full))
	if err != nil {
		return full, nil
	}
	real := filepath.Join(realParent, filepath.Base(full))
	if info, err := os.Lstat(real); err == nil && info.Mode()&os.ModeSymlink != 0 {
		linkTarget, err := os.Readlink(real)
		if err != nil {
			return "", fmt.Errorf("路径 %q 符号链接解析失败：%v", p, err)
		}
		if !filepath.IsAbs(linkTarget) {
			linkTarget = filepath.Join(realParent, linkTarget)
		}
		real = filepath.Clean(linkTarget)
	}
	if err := checkUnderRoot(root, real, p); err != nil {
		return "", err
	}
	return real, nil
}

func checkUnderRoot(root, full, original string) error {
	rel, err := filepath.Rel(root, full)
	if err != nil {
		return fmt.Errorf("路径 %q 不在 workspace_root 内", original)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("路径 %q 越出 workspace_root", original)
	}
	return nil
}

// truncate clamps s to n runes and appends marker when it truncated.
func truncate(s string, n int, marker string) string {
	if n <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + marker
}

// resolveToolPath resolves a tool path under workspaceRoot or rejects it.
func resolveToolPath(workspaceRoot, p string, unrestricted bool) (string, error) {
	if workspaceRoot == "" {
		return "", fmt.Errorf("未配置：workspace_root 为空")
	}
	if unrestricted {
		full := p
		if !filepath.IsAbs(p) {
			full = filepath.Join(workspaceRoot, p)
		}
		return full, nil
	}
	root, err := filepath.Abs(workspaceRoot)
	if err != nil {
		return "", fmt.Errorf("解析 workspace_root 失败：%v", err)
	}
	return resolveUnderRoot(root, p)
}

// openToolFile opens a file that has already been resolved under workspaceRoot.
// It refuses to follow symlinks to prevent TOCTOU escapes.
func openToolFile(path string, flag int, perm os.FileMode) (*os.File, error) {
	return openNoFollow(path, flag, perm)
}

// openNoFollow is OS-specific. Defined in tool_file_*.go.

func object(props map[string]any, required ...string) map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": props,
		"required":   required,
	}
}
