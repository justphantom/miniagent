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

// checkConfine 薄版路径校验：p（相对 root 或绝对）经 Clean+Abs 后须落在 root 子树内。
// 不做 EvalSymlinks 追检——default 是薄软约束，符号链接逃逸由调用方 OS 隔离兜底（审查 v3 §6.2）。
func checkConfine(root, p string) error {
	full := p
	if !filepath.IsAbs(p) {
		full = filepath.Join(root, p)
	}
	absTarget, _ := filepath.Abs(filepath.Clean(full))
	rootAbs, _ := filepath.Abs(filepath.Clean(root))
	sep := string(filepath.Separator)
	if absTarget != rootAbs && !strings.HasPrefix(absTarget+sep, rootAbs+sep) {
		return fmt.Errorf("路径 %q 越出 workdir（default 模式）", p)
	}
	return nil
}
