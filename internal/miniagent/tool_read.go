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
	"time"
)

// maxReadFileBytes 是 read 单文件读取上限：1MB 覆盖大文件（generated code、大数据常量），
// 同时防止超大日志/生成文件撑爆内存。可通过 SetMaxReadFileBytes 覆盖。n<=0 用默认。
const maxReadFileBytes = 1 << 20 // 1MB

// maxReadFileBytesOverride 允许测试/配置覆盖内置上限；nil 用常量默认。
var maxReadFileBytesOverride int

// SetMaxReadFileBytes 覆盖 read 单文件读取上限；测试用，正常流程由 Resolve 调用。
func SetMaxReadFileBytes(n int) {
	if n > 0 {
		maxReadFileBytesOverride = n
	}
}

func readFileBytes() int {
	if maxReadFileBytesOverride > 0 {
		return maxReadFileBytesOverride
	}
	return maxReadFileBytes
}

func readFileChars() int {
	return readFileBytes() / 4
}

// fileOpTimeout 是 read/edit 的内置操作超时：IsRegular 已切断 FIFO 阻塞主因，
// 此处兜底挂起的文件系统（NFS 服务端失联、坏盘、网络黑洞）。
//
// 已知取舍（P2-10，文档化而非重构）：select 命中 runCtx.Done 后主流程返回，但后台
// 读 goroutine 可能仍阻塞在不可中断的内核 read（D 态，如 NFS 服务端失联时 read 不响
// 应信号），ctx 无法取消 → goroutine 不可回收，长跑进程会累积。完整修复需非阻塞 IO
// + 周期 ctx 轮询的重构（成本高、收益低，默认 isolation 下进程短命），暂以注释明示。
// 彻底规避靠调用方按 README「运行隔离」配容器/低权限用户。
const fileOpTimeout = 30 * time.Second

const maxLineLimit = 10000

type readFileArgs struct {
	Path   string `json:"path"`
	Offset int    `json:"offset,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

// ReadFileTool returns a read tool bound to workspaceRoot. timeout<=0 用默认 fileOpTimeout。
func ReadFileTool(workspaceRoot string, timeout time.Duration) Tool {
	if timeout <= 0 {
		timeout = fileOpTimeout
	}
	return Tool{
		Name:        "read",
		Description: fmt.Sprintf("读取文本文件，输出带行号（N │ line，edit 据此定位 offset）。支持 offset/limit 分段读大文件。拒绝二进制（含 NUL）、最终分量符号链接（中间目录 symlink 仍跟随）与非普通文件。单文件 %d 字节、输出 %d 字符上限。path 相对 workdir 或绝对。", readFileBytes(), readFileChars()),
		Parameters: object(map[string]any{
			"path":   map[string]any{"type": "string", "description": "要读取的文件路径，相对 workdir 或绝对路径"},
			"offset": map[string]any{"type": "integer", "description": "起始行号（1-based），默认 1（从头开始）"},
			"limit":  map[string]any{"type": "integer", "description": "最多返回的行数，默认全部"},
		}, "path"),
		ResultLimit: maxFileResultInHistory,
		Call: func(ctx context.Context, args string) ToolResult {
			if err := ctx.Err(); err != nil {
				return ToolResult{IsError: true, Output: "已取消：" + err.Error()}
			}
			runCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			done := make(chan ToolResult, 1)
			go func() { done <- runReadFile(workspaceRoot, args) }()
			select {
			case r := <-done:
				return r
			case <-runCtx.Done():
				return ToolResult{IsError: true, Output: "读取超时或已取消：" + runCtx.Err().Error()}
			}
		},
	}
}

func runReadFile(workspaceRoot, args string) ToolResult {
	a, err := parseReadArgs(args)
	if err != nil {
		return ToolResult{IsError: true, Output: err.Error()}
	}
	// P5：保留路径 "memory" → 读项目记忆（.miniagent/memory.jsonl），绕过常规文件检查。
	if a.Path == memoryPathToken {
		return readMemoryTool(workspaceRoot)
	}
	full := resolveToolPath(workspaceRoot, a.Path)
	// Lstat 而非 Stat：与 edit/openNoFollow 对齐，对最终路径分量是符号链接直接拒
	// （Stat 会跟随软链读目标，与「拒绝符号链接」描述不符）。中间目录的 symlink
	// 仍跟随（read 本就无路径约束，仅最终分量由 O_NOFOLLOW/Lstat 兜底）。
	info, err := os.Lstat(full)
	if err != nil {
		return ToolResult{IsError: true, Output: fmt.Sprintf("读取 %q 失败：%v", a.Path, err)}
	}
	if !info.Mode().IsRegular() {
		// 拒绝非普通文件：FIFO/设备/socket 会让 openNoFollow 阻塞（无写者的
		// FIFO 永久卡住）或读出非文本字节流；symlink（mode 含 ModeSymlink）同样
		// 在此拦截，给出清晰错误而非落到 openNoFollow 的 O_NOFOLLOW errno。
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
	return ToolResult{Output: truncate(formatted, readFileChars(), "…")}
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
	data, err := io.ReadAll(io.LimitReader(f, int64(readFileBytes())))
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
