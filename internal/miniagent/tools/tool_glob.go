package tools

import (
	"github.com/justphantom/miniagent/internal/miniagent/policy"
	miniagent "github.com/justphantom/miniagent/internal/miniagent"
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
func GlobTool(workspaceRoot string, timeout time.Duration, maxOutputChars int) miniagent.Tool {
	if timeout <= 0 {
		timeout = fileOpTimeout
	}
	if maxOutputChars <= 0 {
		maxOutputChars = maxShellOutputChars
	}
	return miniagent.Tool{
		Name:        "glob",
		Description: "递归列举匹配通配的文件路径，每行一个（相对 workdir）。filepath.Match 通配（* ? [...]，不跨 /、无 **）。排除 .git。命中上限 " + strconv.Itoa(maxGlobEntries) + "。",
		Parameters: object(map[string]any{
			"pattern": map[string]any{"type": "string", "description": "filepath.Match 通配模式，如 *.go 或 *_test.go"},
			"path":    map[string]any{"type": "string", "description": "根目录，相对 workdir 或绝对，默认 workdir"},
		}, "pattern"),
		ResultLimit: policy.MaxToolResultInHistory,
		Call: func(ctx context.Context, args string) miniagent.ToolResult {
			return runWithTimeout(ctx, timeout, "列举", func(rctx context.Context) miniagent.ToolResult { return runGlob(rctx, workspaceRoot, args, maxOutputChars) })
		},
	}
}

func runGlob(ctx context.Context, workspaceRoot, args string, maxOutputChars int) miniagent.ToolResult {
	var a globArgs
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("参数解析失败：%v（收到 %q）", err, args)}
	}
	if strings.TrimSpace(a.Pattern) == "" {
		return miniagent.ToolResult{IsError: true, Output: "参数缺失：pattern"}
	}
	if _, err := filepath.Match(a.Pattern, "x"); err != nil {
		return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("通配模式非法：%v", err)}
	}
	root := resolveToolPath(workspaceRoot, a.Path)
	var paths []string
	truncated := false
	walkErr := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		if err != nil {
			if path == root {
				return err // 根目录不可访问：真实错误上抛（codemap 走 Stat 预检，glob 在此区分）
			}
			return nil //nolint:nilerr // 子树不可访问跳过，保留可访问部分（与 grep/codemap 一致）
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
		return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("列举 %q 失败：%v", a.Path, walkErr)}
	}
	if len(paths) == 0 {
		return miniagent.ToolResult{Output: "无匹配"}
	}
	out := text.Truncate(strings.Join(paths, "\n"), maxOutputChars, "…[glob 输出已截断]")
	if truncated {
		out += fmt.Sprintf("\n…（超过 %d 条，已停止收集）", maxGlobEntries)
	}
	return miniagent.ToolResult{Output: out}
}
