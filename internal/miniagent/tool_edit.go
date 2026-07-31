package miniagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

const maxEditFileBytes = 10 << 20

type editFileArgs struct {
	Path      string `json:"path"`
	OldString string `json:"old_string"`
	NewString string `json:"new_string"`
}

// EditFileTool returns an edit tool bound to workspaceRoot.
func EditFileTool(workspaceRoot string) Tool {
	return Tool{
		Name:        "edit",
		Description: "精确替换文件中的一段文本。old_string 必须在文件中唯一出现（精确匹配，含缩进和换行）。出现 0 次或多次均失败。拒绝编辑符号链接。先 read 查看内容再编辑。",
		Parameters: object(map[string]any{
			"path":       map[string]any{"type": "string", "description": "要编辑的文件路径，相对 workdir 或绝对路径"},
			"old_string": map[string]any{"type": "string", "description": "要被替换的原文（必须与文件中的内容精确匹配，含缩进和换行）"},
			"new_string": map[string]any{"type": "string", "description": "替换后的新文本"},
		}, "path", "old_string", "new_string"),
		Call: func(ctx context.Context, args string) ToolResult {
			if err := ctx.Err(); err != nil {
				return ToolResult{IsError: true, Output: "已取消：" + err.Error()}
			}
			runCtx, cancel := context.WithTimeout(ctx, fileOpTimeout)
			defer cancel()
			done := make(chan ToolResult, 1)
			go func() { done <- runEditFile(workspaceRoot, args) }()
			select {
			case r := <-done:
				return r
			case <-runCtx.Done():
				return ToolResult{IsError: true, Output: "编辑超时或已取消：" + runCtx.Err().Error()}
			}
		},
	}
}

func runEditFile(workspaceRoot, args string) ToolResult {
	a, err := parseEditArgs(args)
	if err != nil {
		return ToolResult{IsError: true, Output: err.Error()}
	}
	full := resolveToolPath(workspaceRoot, a.Path)
	info, err := os.Lstat(full)
	if err != nil {
		return ToolResult{IsError: true, Output: fmt.Sprintf("读取 %q 失败：%v", a.Path, err)}
	}
	if !info.Mode().IsRegular() {
		// 拒绝非普通文件：FIFO/字符设备会让 openNoFollow 在无写者时永久阻塞，
		// 目录/socket 同理；与 read 工具对齐。
		return ToolResult{IsError: true, Output: fmt.Sprintf("%q 不是普通文件（mode=%s），仅支持 regular file", a.Path, info.Mode().String())}
	}
	if info.Size() > maxEditFileBytes {
		return ToolResult{IsError: true, Output: fmt.Sprintf("文件 %q 超过最大编辑限制 %d 字节", a.Path, maxEditFileBytes)}
	}
	return applyEdit(full, info, a)
}

func parseEditArgs(args string) (editFileArgs, error) {
	var a editFileArgs
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return editFileArgs{}, fmt.Errorf("参数解析失败：%w（收到 %q）", err, args)
	}
	if a.Path == "" {
		return editFileArgs{}, errors.New("参数缺失：path")
	}
	if a.OldString == "" {
		return editFileArgs{}, errors.New("参数缺失：old_string（不能为空）")
	}
	if a.OldString == a.NewString {
		return editFileArgs{}, errors.New("old_string 与 new_string 相同，无需替换")
	}
	return a, nil
}

func applyEdit(full string, info os.FileInfo, a editFileArgs) ToolResult {
	// openNoFollow 仅拒绝最终路径分量是符号链接（O_NOFOLLOW）；中间目录
	// 不做解析校验，不构成路径边界（free 模式，见 README）。
	f, err := openNoFollow(full, os.O_RDONLY, 0)
	if err != nil {
		return ToolResult{IsError: true, Output: fmt.Sprintf("读取 %q 失败：%v", a.Path, err)}
	}
	defer func() { _ = f.Close() }()
	// 读取量封顶：Lstat 的 Size 与 ReadAll 间存在 TOCTOU（文件被并发替换为更大
	// 内容或字符设备），以实际读取字节数兜底防无界分配；与 read 工具对齐。
	data, err := io.ReadAll(io.LimitReader(f, maxEditFileBytes+1))
	if err != nil {
		return ToolResult{IsError: true, Output: fmt.Sprintf("读取 %q 失败：%v", a.Path, err)}
	}
	if int64(len(data)) > maxEditFileBytes {
		return ToolResult{IsError: true, Output: fmt.Sprintf("文件 %q 读取超过最大编辑限制 %d 字节", a.Path, maxEditFileBytes)}
	}
	content := string(data)
	count := strings.Count(content, a.OldString)
	switch count {
	case 0:
		return ToolResult{IsError: true, Output: fmt.Sprintf("old_string 在 %q 中未找到。文件可能已被修改，请先 read 查看当前内容。", a.Path)}
	case 1:
	default:
		return ToolResult{IsError: true, Output: fmt.Sprintf("old_string 在 %q 中出现 %d 次。请提供更多上下文（扩大 old_string 范围）使其唯一匹配。", a.Path, count)}
	}
	updated := strings.Replace(content, a.OldString, a.NewString, 1)
	mode := os.FileMode(0o644)
	if info != nil {
		mode = info.Mode().Perm()
	}
	if err := writeFileAtomic(full, []byte(updated), mode); err != nil {
		return ToolResult{IsError: true, Output: fmt.Sprintf("写入 %q 失败：%v", a.Path, err)}
	}
	return ToolResult{Output: fmt.Sprintf("已替换 %q 中的 1 处文本（%d → %d 字节）", a.Path, len(content), len(updated))}
}
