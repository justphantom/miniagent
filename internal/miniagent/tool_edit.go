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
	Path       string `json:"path"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all"`
}

// EditFileTool returns an edit tool bound to workspaceRoot.
func EditFileTool(workspaceRoot string) Tool {
	return Tool{
		Name:        "edit",
		Description: "精确替换文件中的一段文本。old_string 须与文件内容精确匹配（含缩进和换行）。缺省要求 old_string 唯一出现（出现 0 次或多次均失败）；设 replace_all=true 则替换全部匹配处。拒绝编辑符号链接与非普通文件。先 read 查看内容再编辑。",
		Parameters: object(map[string]any{
			"path":        map[string]any{"type": "string", "description": "要编辑的文件路径，相对 workdir 或绝对路径"},
			"old_string":  map[string]any{"type": "string", "description": "要被替换的原文（必须与文件中的内容精确匹配，含缩进和换行）"},
			"new_string":  map[string]any{"type": "string", "description": "替换后的新文本"},
			"replace_all": map[string]any{"type": "boolean", "description": "true 时替换所有匹配处；缺省（false）要求 old_string 唯一匹配"},
		}, "path", "old_string", "new_string"),
		ResultLimit: maxFileResultInHistory,
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

// applyOne 在 content 上应用一次替换，返回新 content 与命中处数。纯内存，不写盘。
// 0 处返回 (content, 0, error)；非 replaceAll 且多处返回 (content, n, error)。
// 调用方据 count 区分「未找到」与「多次匹配」给出具体提示。multi_edit 复用此做事务。
func applyOne(content, old, newText string, replaceAll bool) (string, int, error) {
	count := strings.Count(content, old)
	if count == 0 {
		return content, 0, errors.New("未找到")
	}
	if !replaceAll && count > 1 {
		return content, count, fmt.Errorf("出现 %d 次", count)
	}
	if replaceAll {
		return strings.ReplaceAll(content, old, newText), count, nil
	}
	return strings.Replace(content, old, newText, 1), count, nil
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
	updated, count, err := applyOne(content, a.OldString, a.NewString, a.ReplaceAll)
	if err != nil {
		// 保留具体提示：未找到 vs 多次匹配，附文件名与 read 建议。
		if count == 0 {
			return ToolResult{IsError: true, Output: fmt.Sprintf("old_string 在 %q 中未找到。文件可能已被修改，请先 read 查看当前内容。", a.Path)}
		}
		return ToolResult{IsError: true, Output: fmt.Sprintf("old_string 在 %q 中出现 %d 次。请提供更多上下文（扩大 old_string 范围）使其唯一匹配，或设 replace_all=true 全部替换。", a.Path, count)}
	}
	mode := os.FileMode(0o644)
	if info != nil {
		mode = info.Mode().Perm()
	}
	if err := writeFileAtomic(full, []byte(updated), mode); err != nil {
		return ToolResult{IsError: true, Output: fmt.Sprintf("写入 %q 失败：%v", a.Path, err)}
	}
	return ToolResult{Output: fmt.Sprintf("已替换 %q 中的 %d 处文本（%d → %d 字节）", a.Path, count, len(content), len(updated))}
}

// MultiEditTool 对同一文件的多处文本顺序替换（事务：全部成功才写盘，任一失败不改）。
func MultiEditTool(workspaceRoot string) Tool {
	return Tool{
		Name:        "multi_edit",
		Description: "对同一文件的多处文本顺序精确替换（事务：全部成功才写盘，任一失败不改文件）。edits 按序应用，每处基于前一处结果匹配。old_string 须精确匹配；replace_all 默认 false（要求唯一）。先 read 查看内容。",
		Parameters: object(map[string]any{
			"path": map[string]any{"type": "string", "description": "要编辑的文件路径，相对 workdir 或绝对路径"},
			"edits": map[string]any{
				"type":        "array",
				"description": "替换列表，按序应用",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"old_string":  map[string]any{"type": "string", "description": "要被替换的原文（精确匹配）"},
						"new_string":  map[string]any{"type": "string", "description": "替换后的新文本"},
						"replace_all": map[string]any{"type": "boolean", "description": "true 替换该处全部匹配；缺省要求唯一"},
					},
					"required": []string{"old_string", "new_string"},
				},
			},
		}, "path", "edits"),
		ResultLimit: maxFileResultInHistory,
		Call: func(ctx context.Context, args string) ToolResult {
			if err := ctx.Err(); err != nil {
				return ToolResult{IsError: true, Output: "已取消：" + err.Error()}
			}
			runCtx, cancel := context.WithTimeout(ctx, fileOpTimeout)
			defer cancel()
			done := make(chan ToolResult, 1)
			go func() { done <- runMultiEdit(workspaceRoot, args) }()
			select {
			case r := <-done:
				return r
			case <-runCtx.Done():
				return ToolResult{IsError: true, Output: "编辑超时或已取消：" + runCtx.Err().Error()}
			}
		},
	}
}

func runMultiEdit(workspaceRoot, args string) ToolResult {
	var a struct {
		Path  string `json:"path"`
		Edits []struct {
			OldString  string `json:"old_string"`
			NewString  string `json:"new_string"`
			ReplaceAll bool   `json:"replace_all,omitempty"`
		} `json:"edits"`
	}
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return ToolResult{IsError: true, Output: fmt.Sprintf("参数解析失败：%v（收到 %q）", err, args)}
	}
	if a.Path == "" {
		return ToolResult{IsError: true, Output: "参数缺失：path"}
	}
	if len(a.Edits) == 0 {
		return ToolResult{IsError: true, Output: "参数缺失：edits（至少一处替换）"}
	}
	full := resolveToolPath(workspaceRoot, a.Path)
	info, err := os.Lstat(full)
	if err != nil {
		return ToolResult{IsError: true, Output: fmt.Sprintf("读取 %q 失败：%v", a.Path, err)}
	}
	if !info.Mode().IsRegular() {
		return ToolResult{IsError: true, Output: fmt.Sprintf("%q 不是普通文件（mode=%s），仅支持 regular file", a.Path, info.Mode().String())}
	}
	if info.Size() > maxEditFileBytes {
		return ToolResult{IsError: true, Output: fmt.Sprintf("文件 %q 超过最大编辑限制 %d 字节", a.Path, maxEditFileBytes)}
	}
	f, err := openNoFollow(full, os.O_RDONLY, 0)
	if err != nil {
		return ToolResult{IsError: true, Output: fmt.Sprintf("读取 %q 失败：%v", a.Path, err)}
	}
	data, err := io.ReadAll(io.LimitReader(f, maxEditFileBytes+1))
	_ = f.Close()
	if err != nil {
		return ToolResult{IsError: true, Output: fmt.Sprintf("读取 %q 失败：%v", a.Path, err)}
	}
	if int64(len(data)) > maxEditFileBytes {
		return ToolResult{IsError: true, Output: fmt.Sprintf("文件 %q 读取超过最大编辑限制 %d 字节", a.Path, maxEditFileBytes)}
	}
	updated := string(data)
	originLen := len(updated)
	totalMatches := 0
	for i, e := range a.Edits {
		if e.OldString == "" {
			return ToolResult{IsError: true, Output: fmt.Sprintf("第 %d 处 old_string 为空", i+1)}
		}
		if e.OldString == e.NewString {
			return ToolResult{IsError: true, Output: fmt.Sprintf("第 %d 处 old_string 与 new_string 相同", i+1)}
		}
		u, count, aerr := applyOne(updated, e.OldString, e.NewString, e.ReplaceAll)
		if aerr != nil {
			return ToolResult{IsError: true, Output: fmt.Sprintf("第 %d 处替换失败：%s（文件 %q，命中 %d 次）", i+1, aerr, a.Path, count)}
		}
		updated = u
		totalMatches += count
	}
	mode := os.FileMode(0o644)
	if info != nil {
		mode = info.Mode().Perm()
	}
	if err := writeFileAtomic(full, []byte(updated), mode); err != nil {
		return ToolResult{IsError: true, Output: fmt.Sprintf("写入 %q 失败：%v", a.Path, err)}
	}
	return ToolResult{Output: fmt.Sprintf("已在 %q 中应用 %d 处替换（共 %d 次匹配，%d → %d 字节）", a.Path, len(a.Edits), totalMatches, originLen, len(updated))}
}
