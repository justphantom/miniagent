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

type readFileArgs struct {
	Path   string `json:"path"`
	Offset int    `json:"offset,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

// ReadFileTool returns a read tool bound to workspaceRoot.
func ReadFileTool(workspaceRoot string) Tool {
	return Tool{
		Name:        "read",
		Description: "读取文本文件内容。支持 offset/limit 按行范围读取，输出带行号标注。path 可以是绝对路径或相对 workdir 的路径。",
		Parameters: object(map[string]any{
			"path":   map[string]any{"type": "string", "description": "要读取的文件路径，相对 workdir 或绝对路径"},
			"offset": map[string]any{"type": "integer", "description": "起始行号（1-based），默认 1（从头开始）"},
			"limit":  map[string]any{"type": "integer", "description": "最多返回的行数，默认全部"},
		}, "path"),
		Call: func(ctx context.Context, args string) ToolResult {
			if err := ctx.Err(); err != nil {
				return ToolResult{IsError: true, Output: "已取消：" + err.Error()}
			}
			return runReadFile(workspaceRoot, args)
		},
	}
}

func runReadFile(workspaceRoot, args string) ToolResult {
	a, err := parseReadArgs(args)
	if err != nil {
		return ToolResult{IsError: true, Output: err.Error()}
	}
	full := resolveToolPath(workspaceRoot, a.Path)
	info, err := os.Stat(full)
	if err != nil {
		return ToolResult{IsError: true, Output: fmt.Sprintf("读取 %q 失败：%v", a.Path, err)}
	}
	if !info.Mode().IsRegular() {
		// 拒绝非普通文件：FIFO/设备/socket 会让 openNoFollow 阻塞（无写者的
		// FIFO 永久卡住）或读出非文本字节流；只允许 regular 文件。
		return ToolResult{IsError: true, Output: fmt.Sprintf("%q 不是普通文件（mode=%s），仅支持 regular file", a.Path, info.Mode().String())}
	}
	content, err := readFileContent(full)
	if err != nil {
		return ToolResult{IsError: true, Output: fmt.Sprintf("读取 %q 失败：%v", a.Path, err)}
	}
	// 二进制兜底：含 NUL 字节几乎必为二进制（UTF-8/常见编码均不使用 NUL），
	// 让 LLM 读乱码会污染上下文 + 浪费 token。检测前 8 KiB 足够低成本拦截。
	scanLimit := min(len(content), 8192)
	if strings.IndexByte(content[:scanLimit], 0) >= 0 {
		return ToolResult{IsError: true, Output: fmt.Sprintf("%q 是二进制文件（含 NUL 字节），read 仅支持文本", a.Path)}
	}
	// 始终带行号：edit 需要精确匹配，行号帮助 LLM 定位 offset。
	formatted, err := formatLines(content, a.Offset, a.Limit)
	if err != nil {
		return ToolResult{IsError: true, Output: err.Error()}
	}
	return ToolResult{Output: truncate(formatted, maxReadFileChars, "…")}
}

func parseReadArgs(args string) (readFileArgs, error) {
	var a readFileArgs
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return readFileArgs{}, fmt.Errorf("参数解析失败：%w（收到 %q）", err, args)
	}
	if a.Path == "" {
		return readFileArgs{}, errors.New("参数缺失：path")
	}
	if a.Offset < 0 {
		a.Offset = 0
	}
	return a, nil
}

// LimitReader 天然处理超大文件：超过 maxReadFileBytes 的部分直接丢弃，
// 无需按 size 预分支。
func readFileContent(full string) (string, error) {
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

// formatLines 给 content 加 "N │ line" 前缀，按 [offset, offset+limit-1] 范围
// 截取。offset<=0 视作 1；limit<=0 或越界视作读到末尾；limit>maxLineLimit 截断。
// offset 超过文件行数时返回 error（让调用方标记 IsError，而非静默空输出）。
// 空文件（content==""）直接返回空串，避免输出"1 │ "伪空行。
func formatLines(content string, offset, limit int) (string, error) {
	if content == "" {
		return "", nil
	}
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
		return "", fmt.Errorf("offset %d 超出文件行数（共 %d 行）", start, len(lines))
	}
	var sb strings.Builder
	width := len(strconv.Itoa(end))
	for i := start; i <= end; i++ {
		fmt.Fprintf(&sb, "%*d │ %s\n", width, i, lines[i-1])
	}
	return sb.String(), nil
}
