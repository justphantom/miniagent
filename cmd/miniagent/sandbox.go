package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/justphantom/miniagent/internal/miniagent"
)

// confineWrap 包装工具的 Call：执行前校验 args.path 落在 root 子树内，越界拒绝。
// 仅用于写工具（write/edit），两者 args 都含 path 字段。
//
// TOCTOU 取舍（审查 P2-11）：checkConfine 是纯词法校验（Clean+Abs+HasPrefix），与
// 后续 MkdirAll/Rename 之间存在窗口；runToolsParallel 并行执行时，shell 可在窗口内
// 把上级目录替换为软链，使最终 rename 落到 workdir 之外。default 模式本就不是安全
// 边界（shell 已是无限制写原语，README 已声明），能力不增——此处仅做误操作护栏，
// 不强行 EvalSymlinks（会改变 default 语义并引入新失败模式）。真隔离靠低权限用户
// + 容器 + OS 层（见 README「运行隔离」）。
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
// 额外检查从 root 到 target 的已存在路径分量均不得为符号链接，缩小 TOCTOU 窗口。
// 不做 EvalSymlinks 追检——default 是薄软约束，符号链接逃逸由调用方 OS 隔离兜底。
func checkConfine(root, p string) error {
	full := p
	if !filepath.IsAbs(p) {
		full = filepath.Join(root, p)
	}
	absTarget, _ := filepath.Abs(filepath.Clean(full))
	rootAbs, _ := filepath.Abs(filepath.Clean(root))
	sep := string(filepath.Separator)
	// 拒绝 path="." 或等于 workdir 绝对路径：rename 覆盖目录会 EISDIR（错误含糊），
	// 且若 MkdirAll/Rename 真生效将摧毁整个 workdir（审查 P3-8）。
	if absTarget == rootAbs {
		return fmt.Errorf("路径 %q 指向 workdir 根本身，不能覆盖", p)
	}
	if !strings.HasPrefix(absTarget+sep, rootAbs+sep) {
		return fmt.Errorf("路径 %q 越出 workdir（default 模式）", p)
	}
	// 检查已存在路径分量是否含符号链接：攻击者可能将某层目录替换为软链使最终
	// IO 落到 workdir 外。该检查在 IO 前执行，可缩小但无法完全消除 TOCTOU。
	rel, err := filepath.Rel(rootAbs, absTarget)
	if err != nil {
		return fmt.Errorf("路径 %q 相对 workdir 解析失败：%w", p, err)
	}
	current := rootAbs
	for part := range strings.SplitSeq(rel, sep) {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			if os.IsNotExist(err) {
				return nil // 剩余分量不存在，将由后续 MkdirAll/Create 创建
			}
			return fmt.Errorf("检查路径 %q 失败：%w", current, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("路径 %q 含符号链接 %q（default 模式）", p, current)
		}
	}
	return nil
}
