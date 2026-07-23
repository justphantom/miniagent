package miniagent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

const maxWriteFileBytes = 10 << 20

type writefileArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// WriteFileTool returns a write_file tool bound to workspaceRoot.
func WriteFileTool(workspaceRoot string, unrestricted bool) Tool {
	return Tool{
		Name:        "write_file",
		Description: "把 content 写入 workspace_root 内的文件（覆盖已有内容；自动创建父目录）。path 可相对 workspace_root 或绝对。",
		Parameters: object(map[string]any{
			"path":    map[string]any{"type": "string", "description": "要写入的文件路径，相对 workspace_root 或绝对路径"},
			"content": map[string]any{"type": "string", "description": "要写入的完整文件内容"},
		}, "path", "content"),
		Call: func(ctx context.Context, args string) ToolResult {
			if err := ctx.Err(); err != nil {
				return ToolResult{IsError: true, Output: "已取消：" + err.Error()}
			}
			var a writefileArgs
			if err := json.Unmarshal([]byte(args), &a); err != nil {
				return ToolResult{IsError: true, Output: fmt.Sprintf("参数解析失败：%v（收到 %q）", err, args)}
			}
			if a.Path == "" {
				return ToolResult{IsError: true, Output: "参数缺失：path"}
			}
			if len(a.Content) > maxWriteFileBytes {
				return ToolResult{IsError: true, Output: fmt.Sprintf("content 超过最大限制 %d 字节", maxWriteFileBytes)}
			}
			full, err := resolveToolPath(workspaceRoot, a.Path, unrestricted)
			if err != nil {
				return ToolResult{IsError: true, Output: err.Error()}
			}
			if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
				return ToolResult{IsError: true, Output: fmt.Sprintf("创建父目录失败：%v", err)}
			}
			// MkdirAll 之后再走一次解析：此前父目录可能不存在导致 EvalSymlinks
			// 走 NotExist 分支，依赖字符串层兜底。此刻目录已落盘，可对最终路径
			// 做文件系统层校验，关闭"先检查后创建"窗口下的符号链接逃逸。
			finalPath, err := resolveToolPath(workspaceRoot, a.Path, unrestricted)
			if err != nil {
				return ToolResult{IsError: true, Output: err.Error()}
			}
			full = finalPath
			mode := os.FileMode(0o644)
			if info, err := os.Lstat(full); err == nil {
				mode = info.Mode().Perm()
			}
			if err := writeFileAtomic(full, []byte(a.Content), mode); err != nil {
				return ToolResult{IsError: true, Output: fmt.Sprintf("写入 %q 失败：%v", a.Path, err)}
			}
			return ToolResult{Output: fmt.Sprintf("已写入 %d 字节到 %s", len(a.Content), a.Path)}
		},
	}
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".write-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	return os.Rename(tmpName, path)
}
