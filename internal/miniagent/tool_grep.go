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
	"regexp/syntax"
	"strconv"
	"strings"
	"time"

	"github.com/justphantom/miniagent/internal/text"
)

const (
	maxGrepMatches   = 500
	maxGrepFileBytes = 50 << 20 // 单文件大小上限：超大文件（日志/生成物）逐行扫到 fileOpTimeout 才超时，浪费 IO，入口 Stat 直接跳过
	// maxGrepRegexNodes 限制正则复杂度：AST 节点数超限直接拒绝，防止构造性慢正则消耗 CPU。
	maxGrepRegexNodes = 100
)

type grepArgs struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path,omitempty"`
	Glob    string `json:"glob,omitempty"`
}

// GrepTool 递归正则搜索文本文件，输出 file:lineno:line（与 grep -n 一致）。
// workspaceRoot 为空且 path 缺省时，搜调用方进程 cwd。
// timeout<=0 用默认 fileOpTimeout。
func GrepTool(workspaceRoot string, timeout time.Duration, maxMatches, maxOutputChars int) Tool {
	if timeout <= 0 {
		timeout = fileOpTimeout
	}
	if maxMatches <= 0 {
		maxMatches = maxGrepMatches
	}
	if maxOutputChars <= 0 {
		maxOutputChars = maxShellOutputChars
	}
	return Tool{
		Name:        "grep",
		Description: "递归正则搜索文本文件内容。输出 path:lineno:line（与 grep -n 一致，便于定位）。默认搜 workdir；可用 glob 按文件名过滤（仅匹配 base name，* 不跨 /，如 sub/*.go 不命中）。命中行上限 " + strconv.Itoa(maxMatches) + "，输出超 " + strconv.Itoa(maxOutputChars) + " 字符截断。跳过 .git、二进制与超过 50MB 的文件。",
		Parameters: object(map[string]any{
			"pattern": map[string]any{"type": "string", "description": "正则表达式（Go regexp 语法，如 foo、Foo.*、(?i)error）"},
			"path":    map[string]any{"type": "string", "description": "搜索根目录，相对 workdir 或绝对，默认 workdir"},
			"glob":    map[string]any{"type": "string", "description": "文件名 include 过滤，filepath.Match 通配（如 *.go）"},
		}, "pattern"),
		ResultLimit:   maxToolResultInHistory,
		SplitTruncate: true, // 命中上限/无匹配等汇总在尾部，前截断会丢失
		Call: func(ctx context.Context, args string) ToolResult {
			return runWithTimeout(ctx, timeout, "搜索", func() ToolResult { return runGrep(workspaceRoot, args, maxMatches, maxOutputChars) })
		},
	}
}

func runGrep(workspaceRoot, args string, maxMatches, maxOutputChars int) ToolResult {
	a, err := parseGrepArgs(args)
	if err != nil {
		return ToolResult{IsError: true, Output: err.Error()}
	}
	if err := validateGrepPattern(a.Pattern); err != nil {
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
	matches, truncated, err := grepWalk(root, re, globFn, maxMatches)
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
	out := text.Truncate(sb.String(), maxOutputChars, "…[grep 输出已截断]")
	if truncated {
		out += fmt.Sprintf("\n…（命中超过 %d 行，已停止收集）", maxMatches)
	}
	return ToolResult{Output: out}
}

// validateGrepPattern 用 regexp/syntax 解析并限制 AST 节点数，防止构造性慢正则。
func validateGrepPattern(pattern string) error {
	re, err := syntax.Parse(pattern, syntax.Perl)
	if err != nil {
		return fmt.Errorf("正则解析失败：%w", err)
	}
	if n := countRegexNodes(re); n > maxGrepRegexNodes {
		return fmt.Errorf("正则过于复杂：%d 个节点（上限 %d）", n, maxGrepRegexNodes)
	}
	return nil
}

func countRegexNodes(re *syntax.Regexp) int {
	if re == nil {
		return 0
	}
	n := 1
	for _, sub := range re.Sub {
		n += countRegexNodes(sub)
	}
	return n
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
func grepWalk(root string, re *regexp.Regexp, globFn func(string) bool, maxMatches int) ([]grepMatch, bool, error) {
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
		if len(matches) >= maxMatches {
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
			if len(matches) >= maxMatches {
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
// 含 NUL 视为二进制跳过（与 read 工具一致，防乱码污染上下文）。入口 Stat 限制单文
// 件大小：无匹配的超大文件（日志/生成物）会逐行扫到 fileOpTimeout，纯耗 IO。
func grepFile(path, display string, re *regexp.Regexp) ([]grepMatch, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if fi.Size() > maxGrepFileBytes {
		return nil, errors.New("file too large, skipped")
	}
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
