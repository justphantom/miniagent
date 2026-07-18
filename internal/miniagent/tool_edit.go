package miniagent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

type editfileArgs struct {
	Path      string `json:"path"`
	OldString string `json:"old_string"`
	NewString string `json:"new_string"`
}

// EditFileTool returns an edit_file tool bound to workspaceRoot.
func EditFileTool(workspaceRoot string, unrestricted bool) Tool {
	return Tool{
		Name:        "edit_file",
		Description: "精确替换文件中的一段文本。old_string 必须在文件中唯一出现（精确匹配，含缩进和换行）。出现 0 次或多次均失败。先 read_file 查看内容再编辑。",
		Parameters: object(map[string]any{
			"path":       map[string]any{"type": "string", "description": "要编辑的文件路径，相对 workspace_root 或绝对路径"},
			"old_string": map[string]any{"type": "string", "description": "要被替换的原文（必须与文件中的内容精确匹配，含缩进和换行）"},
			"new_string": map[string]any{"type": "string", "description": "替换后的新文本"},
		}, "path", "old_string", "new_string"),
		Call: func(_ context.Context, args string) ToolResult {
			var a editfileArgs
			if err := json.Unmarshal([]byte(args), &a); err != nil {
				return ToolResult{IsError: true, Output: fmt.Sprintf("参数解析失败：%v（收到 %q）", err, args)}
			}
			if a.Path == "" {
				return ToolResult{IsError: true, Output: "参数缺失：path"}
			}
			if a.OldString == "" {
				return ToolResult{IsError: true, Output: "参数缺失：old_string（不能为空）"}
			}
			if a.OldString == a.NewString {
				return ToolResult{IsError: true, Output: "old_string 与 new_string 相同，无需替换"}
			}
			full, err := resolveToolPath(workspaceRoot, a.Path, unrestricted)
			if err != nil {
				return ToolResult{IsError: true, Output: err.Error()}
			}
			data, err := os.ReadFile(full)
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
			if err := os.WriteFile(full, []byte(updated), 0o644); err != nil {
				return ToolResult{IsError: true, Output: fmt.Sprintf("写入 %q 失败：%v", a.Path, err)}
			}
			return ToolResult{Output: fmt.Sprintf("已替换 %q 中的 1 处文本（%d → %d 字节）", a.Path, len(content), len(updated))}
		},
	}
}
