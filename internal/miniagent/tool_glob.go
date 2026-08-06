package miniagent

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/justphantom/miniagent/internal/text"
)

const maxGlobEntries = 500

type globArgs struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path,omitempty"`
}

// GlobTool 递归列举匹配通配的文件路径，每行一个相对 workdir 的路径。
// filepath.Match 通配（*, ?, [...]），不跨 /、不支持 **——需递归用 grep 或 shell。
// timeout<=0 用默认 fileOpTimeout。
func GlobTool(workspaceRoot string, timeout time.Duration, maxOutputChars int) Tool {
	if timeout <= 0 {
		timeout = fileOpTimeout
	}
	if maxOutputChars <= 0 {
		maxOutputChars = maxShellOutputChars
	}
	return Tool{
		Name:        "glob",
		Description: "递归列举匹配通配的文件路径，每行一个（相对 workdir）。filepath.Match 通配（* ? [...]，不跨 /、无 **）。排除 .git。命中上限 " + strconv.Itoa(maxGlobEntries) + "。",
		Parameters: object(map[string]any{
			"pattern": map[string]any{"type": "string", "description": "filepath.Match 通配模式，如 *.go 或 *_test.go"},
			"path":    map[string]any{"type": "string", "description": "根目录，相对 workdir 或绝对，默认 workdir"},
		}, "pattern"),
		ResultLimit: maxToolResultInHistory,
		Call: func(ctx context.Context, args string) ToolResult {
			if err := ctx.Err(); err != nil {
				return ToolResult{IsError: true, Output: "已取消：" + err.Error()}
			}
			runCtx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			done := make(chan ToolResult, 1)
			go func() { done <- runGlob(workspaceRoot, args, maxOutputChars) }()
			select {
			case r := <-done:
				return r
			case <-runCtx.Done():
				return ToolResult{IsError: true, Output: "列举超时或已取消：" + runCtx.Err().Error()}
			}
		},
	}
}

func runGlob(workspaceRoot, args string, maxOutputChars int) ToolResult {
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
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
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
	if walkErr != nil {
		return ToolResult{IsError: true, Output: fmt.Sprintf("列举 %q 失败：%v", a.Path, walkErr)}
	}
	if len(paths) == 0 {
		return ToolResult{Output: "无匹配"}
	}
	out := text.Truncate(strings.Join(paths, "\n"), maxOutputChars, "…[glob 输出已截断]")
	if truncated {
		out += fmt.Sprintf("\n…（超过 %d 条，已停止收集）", maxGlobEntries)
	}
	return ToolResult{Output: out}
}
