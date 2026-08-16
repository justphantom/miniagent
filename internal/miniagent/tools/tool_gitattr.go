// tool_gitattr.go: .gitattributes 外部驱动防线。git 应用属性时读取多个来源（祖先目录各级、
// <gitdir>/info/attributes、workdir 树内每个 .gitattributes），只查仓库根会漏掉其余全部布局
// （审计实测子目录声明 filter= 即绕过）。从 tool_git.go 移入以守 300 行预算。

package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// checkGitAttributes rejects git operations whose clean/smudge filters or diff drivers would execute an
// external program: a workdir-writable .gitattributes declaring `filter=<name>` / `diff=<driver>` /
// `textconv=<cmd>` attributes turns `git add`/`git diff` into arbitrary-command execution — no .git access
// needed, so the .git lock does not cover it. Attributes are collected from every source git reads:
// each ancestor directory from the workdir up to the repository root, <gitdir>/info/attributes, and every
// .gitattributes under the workdir tree (subdirectory files apply to their subtrees — the historical
// root-only read left every other layout unguarded). Only drivers actually DEFINED
// (filter.<name>.clean / diff.<name>.command / textconv under [diff "<name>"] in git config) can execute;
// bare attribute tokens like `diff=java` (hunk-header only) are common and harmless, so an undefined
// driver passes. Guardrail, not a boundary — incoming attributes via pull are supply-chain exposure by
// definition and are not pre-checkable.
func checkGitAttributes(ctx context.Context, dir string) error {
	var declared []string
	for _, src := range gitDriverSources(ctx, dir) {
		declared = append(declared, parseDeclaredDrivers(src)...)
	}
	if len(declared) == 0 {
		return nil
	}
	defined, err := definedGitDrivers(ctx, dir)
	if err != nil {
		// git config 不可读时保守拒绝：驱动属性在场而无法证伪。
		return fmt.Errorf(".gitattributes declares external driver(s) %v but git config could not be read to verify them: %w (default mode)", declared, err)
	}
	for _, d := range declared {
		if defined[d] {
			return fmt.Errorf(".gitattributes declares external driver %q which is defined in git config (filter/diff/textconv execute commands; default mode) — remove the line or use -mode auto", d)
		}
	}
	return nil
}

// gitDriverSources 收集 dir（workdir）起效的全部 .gitattributes 内容：祖先链、
// <gitdir>/info/attributes、workdir 树内各 .gitattributes。rev-parse 失败（旧 git / 环境异常）
// 时跳过 info 源；每源缺席即跳过——声明缺席不是错误。
func gitDriverSources(ctx context.Context, dir string) []string {
	root, err := resolveGitRoot(dir)
	if err != nil {
		root = dir
	}
	sources := make([]string, 0, 8)
	for d := dir; ; d = filepath.Dir(d) {
		sources = appendIfRead(sources, filepath.Join(d, ".gitattributes"))
		if d == root || d == filepath.Dir(d) {
			break // 到仓库根或文件系统顶为止（无仓库时只查 workdir 自身一级）
		}
	}
	sources = appendIfRead(sources, infoAttributesPath(ctx, dir))
	_ = filepath.WalkDir(dir, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // 单个走查错误不打断收集：其余来源仍可判定
		}
		if e.IsDir() {
			if e.Name() == ".git" && p != dir {
				return fs.SkipDir // .git 内容不是属性来源（info/attributes 已单独收集）
			}
			return nil
		}
		if e.Name() == ".gitattributes" && p != filepath.Join(dir, ".gitattributes") {
			sources = appendIfRead(sources, p) // 根文件已由祖先链读取，树内其余全部收集
		}
		return nil
	})
	return sources
}

// appendIfRead 追加 path 的内容；ENOENT/不可读表示该来源无声明，静默跳过。
func appendIfRead(sources []string, path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return sources
	}
	return append(sources, string(data))
}

// infoAttributesPath 定位 <gitdir>/info/attributes：worktree/子模块布局下 gitdir 不在 .git/，
// rev-parse --git-path 是唯一可靠定位。命令失败返回空串（该来源按缺席处理）。
func infoAttributesPath(ctx context.Context, dir string) string {
	cfgCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cfgCtx, "git", "-C", dir, "rev-parse", "--git-path", ".")
	cmd.Env = scrubEnv(os.Environ())
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = nil
	if err := cmd.Run(); err != nil {
		return ""
	}
	gitDir := strings.TrimSpace(out.String())
	if gitDir == "" {
		return ""
	}
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(dir, gitDir)
	}
	return filepath.Join(gitDir, "info", "attributes")
}

// parseDeclaredDrivers 解析 .gitattributes 内容，返回 filter=/diff=/textconv= 声明的驱动名。
func parseDeclaredDrivers(data string) []string {
	var declared []string
	for line := range strings.SplitSeq(data, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		for tok := range strings.FieldsSeq(line) {
			name, value, ok := strings.Cut(tok, "=")
			if !ok {
				continue
			}
			if v := strings.TrimSpace(value); v != "" && (name == "filter" || name == "diff" || name == "textconv") {
				declared = append(declared, v)
			}
		}
	}
	return declared
}

// definedGitDrivers 解析 `git config -l --null` 输出，返回已定义的 filter.<name>.* 与 diff.<name>.*
// 驱动名集合。-l 含 system/global/local 三级，覆盖驱动定义的所有来源。
func definedGitDrivers(ctx context.Context, dir string) (map[string]bool, error) {
	cfgCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cfgCtx, "git", "-C", dir, "config", "-l", "--null", "--includes")
	cmd.Env = scrubEnv(os.Environ())
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	defined := map[string]bool{}
	for entry := range strings.SplitSeq(out.String(), "\x00") {
		key, _, ok := strings.Cut(entry, "\n")
		if !ok {
			key = entry
		}
		key = strings.ToLower(key)
		// filter.<name>.clean/smudge → name；diff.<name>.command/textconv → name（diff.textconv 顶层键无驱动语义，跳过）
		for _, prefix := range []string{"filter.", "diff."} {
			if strings.HasPrefix(key, prefix) {
				rest := key[len(prefix):]
				if dot := strings.IndexByte(rest, '.'); dot > 0 {
					defined[rest[:dot]] = true
				}
			}
		}
	}
	return defined, nil
}

// resolveGitRoot 向上查找含 .git 的最近祖先（workdir 可在仓库子目录）。
func resolveGitRoot(startDir string) (string, error) {
	dir := startDir
	if dir == "" {
		var err error
		dir, err = os.Getwd()
		if err != nil {
			return "", err
		}
	}
	for {
		if fi, err := os.Stat(filepath.Join(dir, ".git")); err == nil && (fi.IsDir() || fi.Mode().IsRegular()) {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", errors.New("not a git repository")
}
