package miniagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
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
		Call: func(ctx context.Context, args string) ToolResult {
			if err := ctx.Err(); err != nil {
				return ToolResult{IsError: true, Output: "已取消：" + err.Error()}
			}
			return runReadFile(workspaceRoot, unrestricted, args)
		},
	}
}

func runReadFile(workspaceRoot string, unrestricted bool, args string) ToolResult {
	a, err := parseReadArgs(args)
	if err != nil {
		return ToolResult{IsError: true, Output: err.Error()}
	}
	full, info, err := resolveReadTarget(workspaceRoot, a.Path, unrestricted)
	if err != nil {
		return ToolResult{IsError: true, Output: err.Error()}
	}
	content, err := readFileContent(full, info)
	if err != nil {
		return ToolResult{IsError: true, Output: fmt.Sprintf("读取 %q 失败：%v", a.Path, err)}
	}
	return ToolResult{Output: truncate(formatReadOutput(content, a.Offset, a.Limit), maxReadFileChars, "…")}
}

func parseReadArgs(args string) (readfileArgs, error) {
	var a readfileArgs
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return readfileArgs{}, fmt.Errorf("参数解析失败：%w（收到 %q）", err, args)
	}
	if a.Path == "" {
		return readfileArgs{}, errors.New("参数缺失：path")
	}
	if a.Offset < 0 {
		a.Offset = 0
	}
	return a, nil
}

func resolveReadTarget(workspaceRoot, path string, unrestricted bool) (string, os.FileInfo, error) {
	full, err := resolveToolPath(workspaceRoot, path, unrestricted)
	if err != nil {
		return "", nil, err
	}
	info, err := os.Stat(full)
	if err != nil {
		return "", nil, err
	}
	if info.IsDir() {
		return "", nil, fmt.Errorf("%q 是目录，不是文件", path)
	}
	return full, info, nil
}

func readFileContent(full string, info os.FileInfo) (string, error) {
	if info.Size() > maxReadFileBytes {
		return readFileLimited(full), nil
	}
	f, err := openNoFollow(full, os.O_RDONLY, 0)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxReadFileBytes))
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func formatReadOutput(content string, offset, limit int) string {
	if offset == 0 && limit == 0 {
		return content
	}
	return formatLines(content, offset, limit)
}

func readFileLimited(path string) string {
	f, err := openNoFollow(path, os.O_RDONLY, 0)
	if err != nil {
		return fmt.Sprintf("读取 %q 失败：%v", path, err)
	}
	defer func() { _ = f.Close() }()
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
	start := max(offset, 1)
	end := len(lines)
	if limit > 0 && start+limit-1 < end {
		end = start + limit - 1
	}
	if start > len(lines) {
		return fmt.Sprintf("offset %d 超出文件行数（共 %d 行）", start, len(lines))
	}
	var sb strings.Builder
	width := len(strconv.Itoa(end))
	for i := start; i <= end; i++ {
		fmt.Fprintf(&sb, "%*d │ %s\n", width, i, lines[i-1])
	}
	return sb.String()
}
