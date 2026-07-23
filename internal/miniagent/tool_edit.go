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

type editfileArgs struct {
	Path      string `json:"path"`
	OldString string `json:"old_string"`
	NewString string `json:"new_string"`
}

// EditFileTool returns an edit_file tool bound to workspaceRoot.
func EditFileTool(workspaceRoot string) Tool {
	return Tool{
		Name:        "edit_file",
		Description: "精确替换文件中的一段文本。old_string 必须在文件中唯一出现（精确匹配，含缩进和换行）。出现 0 次或多次均失败。拒绝编辑符号链接。先 read_file 查看内容再编辑。",
		Parameters: object(map[string]any{
			"path":       map[string]any{"type": "string", "description": "要编辑的文件路径，相对 workspace_root 或绝对路径"},
			"old_string": map[string]any{"type": "string", "description": "要被替换的原文（必须与文件中的内容精确匹配，含缩进和换行）"},
			"new_string": map[string]any{"type": "string", "description": "替换后的新文本"},
		}, "path", "old_string", "new_string"),
		Call: func(ctx context.Context, args string) ToolResult {
			if err := ctx.Err(); err != nil {
				return ToolResult{IsError: true, Output: "已取消：" + err.Error()}
			}
			return runEditFile(workspaceRoot, args)
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
	if info.Size() > maxEditFileBytes {
		return ToolResult{IsError: true, Output: fmt.Sprintf("文件 %q 超过最大编辑限制 %d 字节", a.Path, maxEditFileBytes)}
	}
	return applyEdit(full, info, a)
}

func parseEditArgs(args string) (editfileArgs, error) {
	var a editfileArgs
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return editfileArgs{}, fmt.Errorf("参数解析失败：%w（收到 %q）", err, args)
	}
	if a.Path == "" {
		return editfileArgs{}, errors.New("参数缺失：path")
	}
	if a.OldString == "" {
		return editfileArgs{}, errors.New("参数缺失：old_string（不能为空）")
	}
	if a.OldString == a.NewString {
		return editfileArgs{}, errors.New("old_string 与 new_string 相同，无需替换")
	}
	return a, nil
}

func applyEdit(full string, info os.FileInfo, a editfileArgs) ToolResult {
	// openNoFollow 拒绝最终路径是符号链接的情形（O_NOFOLLOW），防止
	// 通过符号链接把外部文件覆盖式编辑。
	f, err := openNoFollow(full, os.O_RDONLY, 0)
	if err != nil {
		return ToolResult{IsError: true, Output: fmt.Sprintf("读取 %q 失败：%v", a.Path, err)}
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(f)
	if err != nil {
		return ToolResult{IsError: true, Output: fmt.Sprintf("读取 %q 失败：%v", a.Path, err)}
	}
	content := string(data)
	count := strings.Count(content, a.OldString)
	switch count {
	case 0:
		return ToolResult{IsError: true, Output: fmt.Sprintf("old_string 在 %q 中未找到。文件可能已被修改，请先 read_file 查看当前内容。", a.Path)}
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
