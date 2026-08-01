package main

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/justphantom/miniagent/internal/miniagent"
)

// confineWrap 包装工具的 Call：执行前校验 args.path 落在 root 子树内，越界拒绝。
// 仅用于写工具（write/edit/multi_edit），三者 args 都含 path 字段。
func confineWrap(tool miniagent.Tool, root string) miniagent.Tool {
	orig := tool.Call
	tool.Call = func(ctx context.Context, args string) miniagent.ToolResult {
		var p struct {
			Path string `json:"path"`
		}
		if json.Unmarshal([]byte(args), &p) == nil && p.Path != "" {
			if err := checkConfine(root, p.Path); err != nil {
				return miniagent.ToolResult{IsError: true, Output: err.Error()}
			}
		}
		return orig(ctx, args)
	}
	return tool
}

// checkConfine 校验 p（相对 root 或绝对）解析后落在 root 子树内。EvalSymlinks 防符号
// 链接逃逸；root 不存在或 p 解析失败均拒绝（防御）。注意 EvalSymlinks 与工具内 open
// 间存在 TOCTOU，free 模式下不构成安全边界，彻底防护靠调用方 OS 隔离。
func checkConfine(root, p string) error {
	full := p
	if !filepath.IsAbs(p) {
		full = filepath.Join(root, p)
	}
	rootReal, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("解析 workdir 失败：%w", err)
	}
	rootAbs, _ := filepath.Abs(rootReal)
	// target 若存在，EvalSymlinks 解析（防符号链接逃逸）；若不存在（新建文件），
	// 退化为 Abs+Clean 判断 .. 越界（不防 symlink，sandbox 仅软约束）。
	targetReal, err := filepath.EvalSymlinks(full)
	if err != nil {
		targetReal, _ = filepath.Abs(filepath.Clean(full))
	}
	absTarget, _ := filepath.Abs(targetReal)
	sep := string(filepath.Separator)
	if absTarget != rootAbs && !strings.HasPrefix(absTarget+sep, rootAbs+sep) {
		return fmt.Errorf("路径 %q 越出 workdir 沙箱", p)
	}
	return nil
}
