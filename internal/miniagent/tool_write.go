package miniagent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const maxWriteFileBytes = 10 << 20

// writeOpTimeout 是 write 的内置操作超时：原子写入本身极快，此处兜底极端文件系统延迟。
const writeOpTimeout = 30 * time.Second

type writeFileArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// WriteFileTool returns a write tool bound to workspaceRoot. timeout<=0 用默认 writeOpTimeout。
func WriteFileTool(workspaceRoot string, timeout time.Duration) Tool {
	if timeout <= 0 {
		timeout = writeOpTimeout
	}
	desc := fmt.Sprintf("整体覆盖写入文件（自动建父目录、保留原文件权限）。content 上限 10MiB。path 相对 workdir 或绝对。仅用于新建文件或完整重写；局部改动用 edit。超时 %s。", timeout)
	return Tool{
		Name:        "write",
		Description: desc,
		Parameters: object(map[string]any{
			"path":    map[string]any{"type": "string", "description": "要写入的文件路径，相对 workdir 或绝对路径"},
			"content": map[string]any{"type": "string", "description": "要写入的完整文件内容"},
		}, "path", "content"),
		Call: func(ctx context.Context, args string) ToolResult {
			if err := ctx.Err(); err != nil {
				return ToolResult{IsError: true, Output: "已取消：" + err.Error()}
			}
			runCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			done := make(chan ToolResult, 1)
			go func() { done <- runWriteFile(workspaceRoot, args) }()
			select {
			case r := <-done:
				return r
			case <-runCtx.Done():
				return ToolResult{IsError: true, Output: "写入超时或已取消：" + runCtx.Err().Error()}
			}
		},
	}
}

func runWriteFile(workspaceRoot, args string) ToolResult {
	var a writeFileArgs
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return ToolResult{IsError: true, Output: fmt.Sprintf("参数解析失败：%v（收到 %q）", err, args)}
	}
	if a.Path == "" {
		return ToolResult{IsError: true, Output: "参数缺失：path"}
	}
	if len(a.Content) > maxWriteFileBytes {
		return ToolResult{IsError: true, Output: fmt.Sprintf("content 超过最大限制 %d 字节", maxWriteFileBytes)}
	}
	// P5：保留路径 "memory" → 追加项目记忆记录（特殊语义：追加而非覆盖）。
	if a.Path == memoryPathToken {
		return writeMemoryTool(workspaceRoot, a.Content)
	}
	full := resolveToolPath(workspaceRoot, a.Path)
	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		return ToolResult{IsError: true, Output: fmt.Sprintf("创建父目录失败：%v", err)}
	}
	mode := os.FileMode(0o644)
	if info, err := os.Lstat(full); err == nil {
		// 拒绝非普通文件：FIFO/字符设备会让后续 Rename 报含糊错误，目录会
		// EISDIR；与 edit 对齐明确报「不是普通文件」（审查 P3-7）。
		if !info.Mode().IsRegular() {
			return ToolResult{IsError: true, Output: fmt.Sprintf("%q 不是普通文件（mode=%s），仅支持 regular file", a.Path, info.Mode().String())}
		}
		mode = info.Mode().Perm()
	}
	if err := writeFileAtomic(full, []byte(a.Content), mode); err != nil {
		return ToolResult{IsError: true, Output: fmt.Sprintf("写入 %q 失败：%v", a.Path, err)}
	}
	return ToolResult{Output: fmt.Sprintf("已写入 %d 字节到 %s", len(a.Content), a.Path)}
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
