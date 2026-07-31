package miniagent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

const (
	maxGrepMatches = 200
	maxGrepOutput  = maxShellOutputChars // 复用 20000 字符输出上限
)

type grepArgs struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path,omitempty"`
	Glob    string `json:"glob,omitempty"`
}

// GrepTool 递归正则搜索文本文件，输出 file:lineno:line（与 grep -n 一致）。
// workspaceRoot 为空且 path 缺省时，搜调用方进程 cwd。
func GrepTool(workspaceRoot string) Tool {
	return Tool{
		Name:        "grep",
		Description: "递归正则搜索文本文件内容。输出 path:lineno:line（与 grep -n 一致，便于定位）。默认搜 workdir；可用 glob 按文件名过滤。命中行上限 " + strconv.Itoa(maxGrepMatches) + "，输出超 " + strconv.Itoa(maxGrepOutput) + " 字符截断。跳过 .git 与二进制文件。",
		Parameters: object(map[string]any{
			"pattern": map[string]any{"type": "string", "description": "正则表达式（Go regexp 语法，如 foo、Foo.*、(?i)error）"},
			"path":    map[string]any{"type": "string", "description": "搜索根目录，相对 workdir 或绝对，默认 workdir"},
			"glob":    map[string]any{"type": "string", "description": "文件名 include 过滤，filepath.Match 通配（如 *.go）"},
		}, "pattern"),
		Call: func(ctx context.Context, args string) ToolResult {
			if err := ctx.Err(); err != nil {
				return ToolResult{IsError: true, Output: "已取消：" + err.Error()}
			}
			runCtx, cancel := context.WithTimeout(ctx, fileOpTimeout)
			defer cancel()
			done := make(chan ToolResult, 1)
			go func() { done <- runGrep(workspaceRoot, args) }()
			select {
			case r := <-done:
				return r
			case <-runCtx.Done():
				return ToolResult{IsError: true, Output: "搜索超时或已取消：" + runCtx.Err().Error()}
			}
		},
	}
}

func runGrep(workspaceRoot, args string) ToolResult {
	a, err := parseGrepArgs(args)
	if err != nil {
		return ToolResult{IsError: true, Output: err.Error()}
	}
	re, err := regexp.Compile(a.Pattern)
	if err != nil {
		return ToolResult{IsError: true, Output: fmt.Sprintf("正则非法：%v", err)}
	}
	root := resolveToolPath(workspaceRoot, a.Path)
	var globFn func(string) bool
	if strings.TrimSpace(a.Glob) != "" {
		globFn = func(name string) bool {
			ok, _ := filepath.Match(a.Glob, name)
			return ok
		}
	}
	matches, truncated, err := grepWalk(root, re, globFn)
	if err != nil {
		return ToolResult{IsError: true, Output: fmt.Sprintf("搜索 %q 失败：%v", a.Path, err)}
	}
	if len(matches) == 0 {
		return ToolResult{Output: "无命中"}
	}
	var sb strings.Builder
	for _, m := range matches {
		fmt.Fprintf(&sb, "%s:%d:%s\n", m.file, m.line, m.text)
	}
	out := truncate(sb.String(), maxGrepOutput, "…[grep 输出已截断]")
	if truncated {
		out += fmt.Sprintf("\n…（命中超过 %d 行，已停止收集）", maxGrepMatches)
	}
	return ToolResult{Output: out}
}

func parseGrepArgs(args string) (grepArgs, error) {
	var a grepArgs
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return grepArgs{}, fmt.Errorf("参数解析失败：%w（收到 %q）", err, args)
	}
	if strings.TrimSpace(a.Pattern) == "" {
		return grepArgs{}, errors.New("参数缺失：pattern")
	}
	return a, nil
}

type grepMatch struct {
	file string
	line int
	text string
}

// grepWalk 遍历 root，对每个文本文件逐行匹配。不可访问的子树/文件跳过而非整体
// 失败（部分可读仍有益）。跳过 .git、符号链接（防递归误入）。
func grepWalk(root string, re *regexp.Regexp, globFn func(string) bool) ([]grepMatch, bool, error) {
	var matches []grepMatch
	truncated := false
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
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
		if globFn != nil && !globFn(d.Name()) {
			return nil
		}
		if len(matches) >= maxGrepMatches {
			truncated = true
			return filepath.SkipAll
		}
		rel := path
		if r, err := filepath.Rel(root, path); err == nil {
			rel = r
		}
		ms, err := grepFile(path, rel, re)
		if err != nil {
			return nil //nolint:nilerr // 二进制/读取失败：跳过该文件，不整体失败
		}
		for _, m := range ms {
			if len(matches) >= maxGrepMatches {
				truncated = true
				break
			}
			matches = append(matches, m)
		}
		return nil
	})
	return matches, truncated, err
}

// grepFile 读 path 逐行匹配，display 作为输出里的文件名（通常是相对 root 的路径）。
// 含 NUL 视为二进制跳过（与 read 工具一致，防乱码污染上下文）。
func grepFile(path, display string, re *regexp.Regexp) ([]grepMatch, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	br := bufio.NewReader(f)
	head, _ := br.Peek(8192)
	if bytes.IndexByte(head, 0) >= 0 {
		return nil, errors.New("binary")
	}
	scanner := bufio.NewScanner(br)
	// 默认 64KB 行长上限太小（压缩/生成文件常见超长行），放宽到 1MB。
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	var matches []grepMatch
	lineno := 0
	for scanner.Scan() {
		lineno++
		if re.MatchString(scanner.Text()) {
			matches = append(matches, grepMatch{file: display, line: lineno, text: scanner.Text()})
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return matches, nil
}
