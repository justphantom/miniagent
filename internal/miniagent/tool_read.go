package miniagent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

const maxReadFileChars = 20000
const maxReadFileBytes = maxReadFileChars * 4

const maxLineLimit = 10000

type readfileArgs struct {
	Path   string `json:"path"`
	Offset int    `json:"offset,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

// ReadFileTool returns a read_file tool bound to workspaceRoot.
func ReadFileTool(workspaceRoot string, unrestricted bool) Tool {
	return Tool{
		Name:        "read_file",
		Description: "读取 workspace_root 内的文本文件内容。支持 offset/limit 按行范围读取，输出带行号标注。path 可以是绝对路径或相对 workspace_root 的路径。",
		Parameters: object(map[string]any{
			"path":   map[string]any{"type": "string", "description": "要读取的文件路径，相对 workspace_root 或绝对路径"},
			"offset": map[string]any{"type": "integer", "description": "起始行号（1-based），默认 1（从头开始）"},
			"limit":  map[string]any{"type": "integer", "description": "最多返回的行数，默认全部"},
		}, "path"),
		Call: func(_ context.Context, args string) ToolResult {
			var a readfileArgs
			if err := json.Unmarshal([]byte(args), &a); err != nil {
				return ToolResult{IsError: true, Output: fmt.Sprintf("参数解析失败：%v（收到 %q）", err, args)}
			}
			if a.Path == "" {
				return ToolResult{IsError: true, Output: "参数缺失：path"}
			}
			if a.Offset < 0 {
				a.Offset = 0
			}

			full, err := resolveToolPath(workspaceRoot, a.Path, unrestricted)
			if err != nil {
				return ToolResult{IsError: true, Output: err.Error()}
			}

			info, err := os.Stat(full)
			if err != nil {
				return ToolResult{IsError: true, Output: fmt.Sprintf("无法访问 %q：%v", a.Path, err)}
			}
			if info.IsDir() {
				return ToolResult{IsError: true, Output: fmt.Sprintf("%q 是目录，不是文件", a.Path)}
			}
			if info.Size() > maxReadFileBytes {
				return ToolResult{Output: truncate(readFileLimited(full), maxReadFileChars, "…")}
			}
			f, err := openToolFile(full, os.O_RDONLY, 0)
			if err != nil {
				return ToolResult{IsError: true, Output: fmt.Sprintf("读取 %q 失败：%v", a.Path, err)}
			}
			defer f.Close()
			data, err := io.ReadAll(io.LimitReader(f, maxReadFileBytes))
			if err != nil {
				return ToolResult{IsError: true, Output: fmt.Sprintf("读取 %q 失败：%v", a.Path, err)}
			}
			content := string(data)
			if a.Offset == 0 && a.Limit == 0 {
				return ToolResult{Output: truncate(content, maxReadFileChars, "…")}
			}
			return ToolResult{Output: truncate(formatLines(content, a.Offset, a.Limit), maxReadFileChars, "…")}
		},
	}
}

func readFileLimited(path string) string {
	f, err := openToolFile(path, os.O_RDONLY, 0)
	if err != nil {
		return fmt.Sprintf("读取 %q 失败：%v", path, err)
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxReadFileBytes))
	if err != nil {
		return fmt.Sprintf("读取 %q 失败：%v", path, err)
	}
	return string(data)
}

func formatLines(content string, offset, limit int) string {
	if limit < 0 || limit > maxLineLimit {
		limit = maxLineLimit
	}
	lines := strings.Split(content, "\n")
	start := offset
	if start < 1 {
		start = 1
	}
	end := len(lines)
	if limit > 0 && start+limit-1 < end {
		end = start + limit - 1
	}
	if start > len(lines) {
		return fmt.Sprintf("offset %d 超出文件行数（共 %d 行）", start, len(lines))
	}
	var sb strings.Builder
	width := len(fmt.Sprintf("%d", end))
	for i := start; i <= end; i++ {
		fmt.Fprintf(&sb, "%*d │ %s\n", width, i, lines[i-1])
	}
	return sb.String()
}
