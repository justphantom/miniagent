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
		// 父目录不存在（写新文件场景，调用方稍后会 MkdirAll）允许通过，
		// 仅靠上面的字符串层 checkUnderRoot 兜底；其余失败（权限/循环链接等）
		// 视为可疑，直接拒绝以避免静默绕过符号链接逃逸检查。
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("路径 %q 父目录解析失败：%w", p, err)
		}
		return full, nil
	}
	realPath := filepath.Join(realParent, filepath.Base(full))
	if info, err := os.Lstat(realPath); err == nil && info.Mode()&os.ModeSymlink != 0 {
		linkTarget, err := os.Readlink(realPath)
		if err != nil {
			return "", fmt.Errorf("路径 %q 符号链接解析失败：%w", p, err)
		}
		if !filepath.IsAbs(linkTarget) {
			linkTarget = filepath.Join(realParent, linkTarget)
		}
		realPath = filepath.Clean(linkTarget)
	}
	if err := checkUnderRoot(root, realPath, p); err != nil {
		return "", err
	}
	return realPath, nil
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
		return "", fmt.Errorf("解析 workspace_root 失败：%w", err)
	}
	return resolveUnderRoot(root, p)
}

// openNoFollow is OS-specific. Defined in tool_file_*.go.

// object 构造 JSON Schema 的 object 描述。required 为空时省略键：JSON Schema
// 规范规定省略 required 等同空数组，所有合规后端都接受；而把 nil slice 写进
// map 会被序列化成 "required":null，触发严格后端（如 OpenAI）的 400。
func object(props map[string]any, required ...string) map[string]any {
	out := map[string]any{
		"type":       "object",
		"properties": props,
	}
	if len(required) > 0 {
		out["required"] = required
	}
	return out
}
