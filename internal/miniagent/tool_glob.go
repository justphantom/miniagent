package miniagent

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
)

const maxGlobEntries = 500

type globArgs struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path,omitempty"`
}

// GlobTool 递归列举匹配通配的文件路径，每行一个相对 workdir 的路径。
// filepath.Match 通配（*, ?, [...]），不跨 /、不支持 **——需递归用 grep 或 shell。
func GlobTool(workspaceRoot string) Tool {
	return Tool{
		Name:        "glob",
		Description: "递归列举匹配通配的文件路径，每行一个（相对 workdir）。filepath.Match 通配（* ? [...]，不跨 /、无 **）。排除 .git。命中上限 " + strconv.Itoa(maxGlobEntries) + "。",
		Parameters: object(map[string]any{
			"pattern": map[string]any{"type": "string", "description": "filepath.Match 通配模式，如 *.go 或 *_test.go"},
			"path":    map[string]any{"type": "string", "description": "根目录，相对 workdir 或绝对，默认 workdir"},
		}, "pattern"),
		Call: func(ctx context.Context, args string) ToolResult {
			if err := ctx.Err(); err != nil {
				return ToolResult{IsError: true, Output: "已取消：" + err.Error()}
			}
			runCtx, cancel := context.WithTimeout(ctx, fileOpTimeout)
			defer cancel()
			done := make(chan ToolResult, 1)
			go func() { done <- runGlob(workspaceRoot, args) }()
			select {
			case r := <-done:
				return r
			case <-runCtx.Done():
				return ToolResult{IsError: true, Output: "列举超时或已取消：" + runCtx.Err().Error()}
			}
		},
	}
}

func runGlob(workspaceRoot, args string) ToolResult {
	var a globArgs
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return ToolResult{IsError: true, Output: fmt.Sprintf("参数解析失败：%v（收到 %q）", err, args)}
	}
	if strings.TrimSpace(a.Pattern) == "" {
		return ToolResult{IsError: true, Output: "参数缺失：pattern"}
	}
	if _, err := filepath.Match(a.Pattern, "x"); err != nil {
		return ToolResult{IsError: true, Output: fmt.Sprintf("通配模式非法：%v", err)}
	}
	root := resolveToolPath(workspaceRoot, a.Path)
	var paths []string
	truncated := false
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil //nolint:nilerr // 不可访问的子树跳过，保留可访问部分的结果
		}
		if d.IsDir() {
			if d.Name() == ".git" || d.Type()&fs.ModeSymlink != 0 {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		ok, _ := filepath.Match(a.Pattern, d.Name())
		if !ok {
			return nil
		}
		if len(paths) >= maxGlobEntries {
			truncated = true
			return filepath.SkipAll
		}
		rel := path
		if r, err := filepath.Rel(root, path); err == nil {
			rel = r
		}
		paths = append(paths, rel)
		return nil
	})
	if len(paths) == 0 {
		return ToolResult{Output: "无匹配"}
	}
	out := truncate(strings.Join(paths, "\n"), maxShellOutputChars, "…[glob 输出已截断]")
	if truncated {
		out += fmt.Sprintf("\n…（超过 %d 条，已停止收集）", maxGlobEntries)
	}
	return ToolResult{Output: out}
}
