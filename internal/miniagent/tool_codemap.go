package miniagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	maxCodemapEntries   = 500
	defaultCodemapDepth = 3
)

type codemapArgs struct {
	Path  string `json:"path,omitempty"`
	Depth int    `json:"depth,omitempty"`
}

// CodemapTool 以缩进树形文本返回目录结构概览（目录标注子条目数），填补
// glob（扁平列表）与 read（单文件全文）之间的结构感知缺口。
// 跳过 .git 与符号链接（防递归误入）；depth<=0 不限深度（仍受条目上限约束）。
// timeout<=0 用默认 fileOpTimeout。
func CodemapTool(workspaceRoot string, timeout time.Duration) Tool {
	if timeout <= 0 {
		timeout = fileOpTimeout
	}
	return Tool{
		Name: "codemap",
		Description: "返回目录树概览：缩进表示层级（每层 2 空格），目录标注直接子条目数。跳过 .git 与符号链接。条目上限 " +
			strconv.Itoa(maxCodemapEntries) + "。用于低成本了解仓库布局；要看文件内容用 read，按文件名过滤用 glob。",
		Parameters: object(map[string]any{
			"path":  map[string]any{"type": "string", "description": "根目录，相对 workdir 或绝对，默认 workdir"},
			"depth": map[string]any{"type": "integer", "description": "最大递归深度，默认 " + strconv.Itoa(defaultCodemapDepth) + "；<=0 不限（仍受条目上限约束）"},
		}),
		ResultLimit:   maxToolResultInHistory,
		SplitTruncate: true, // 条目上限提示在尾部，前截断会丢失
		Call: func(ctx context.Context, args string) ToolResult {
			return runWithTimeout(ctx, timeout, "遍历", func(rctx context.Context) ToolResult { return runCodemap(rctx, workspaceRoot, args) })
		},
	}
}

func runCodemap(ctx context.Context, workspaceRoot, args string) ToolResult {
	var a codemapArgs
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return ToolResult{IsError: true, Output: fmt.Sprintf("参数解析失败：%v（收到 %q）", err, args)}
	}
	if a.Depth == 0 {
		a.Depth = defaultCodemapDepth
	}
	root := resolveToolPath(workspaceRoot, a.Path)
	if fi, err := os.Stat(root); err != nil || !fi.IsDir() {
		if err == nil {
			err = errors.New("不是目录")
		}
		return ToolResult{IsError: true, Output: fmt.Sprintf("遍历 %q 失败：%v", a.Path, err)}
	}
	lines, truncated, err := codemapWalk(ctx, root, a.Depth)
	if err != nil {
		return ToolResult{IsError: true, Output: fmt.Sprintf("遍历 %q 失败：%v", a.Path, err)}
	}
	if len(lines) == 0 {
		return ToolResult{Output: "（空目录）"}
	}
	out := strings.Join(lines, "\n")
	if truncated {
		out += fmt.Sprintf("\n…（超过 %d 条，已停止收集）", maxCodemapEntries)
	}
	return ToolResult{Output: out}
}

// codemapWalk 遍历 root 生成树形行。目录名带 "/" 后缀并标注直接子条目数
// （.git/符号链接不计入）；超 depth 的目录只列名不进子树，其计数标注为 "?"。
// 条目数（含目录本身）达 maxCodemapEntries 即 SkipAll。
func codemapWalk(ctx context.Context, root string, maxDepth int) ([]string, bool, error) {
	var lines []string
	truncated := false
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return nil //nolint:nilerr // 不可访问的子树跳过，保留可访问部分（与 grep 一致）
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil //nolint:nilerr // Rel 对已遍历路径仅拦截器路径失败，跳过该条目即可
		}
		if rel == "." {
			return nil
		}
		if len(lines) >= maxCodemapEntries {
			truncated = true
			return filepath.SkipAll
		}
		depth := strings.Count(rel, string(filepath.Separator)) + 1
		indent := strings.Repeat("  ", depth-1)
		if !d.IsDir() {
			if d.Type()&fs.ModeSymlink != 0 {
				return nil
			}
			lines = append(lines, indent+d.Name())
			return nil
		}
		if d.Name() == ".git" || d.Type()&fs.ModeSymlink != 0 {
			return filepath.SkipDir
		}
		if maxDepth > 0 && depth >= maxDepth {
			// 深度边界：子树不展开，计数不可得，标注 "?" 避免谎报。
			lines = append(lines, indent+d.Name()+"/ (? items)")
			return filepath.SkipDir
		}
		lines = append(lines, fmt.Sprintf("%s%s/ (%d items)", indent, d.Name(), countDirItems(path)))
		return nil
	})
	return lines, truncated, err
}

// countDirItems 数 dir 的直接子条目（跳过 .git 与符号链接，与遍历口径一致）。
// 读取失败返回 0——目录行已列出，计数缺失不阻断整体遍历。
func countDirItems(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if e.Name() == ".git" || e.Type()&fs.ModeSymlink != 0 {
			continue
		}
		n++
	}
	return n
}
