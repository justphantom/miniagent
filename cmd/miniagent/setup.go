package main

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"log/slog"

	"github.com/justphantom/miniagent/internal/miniagent"
)

func mustParseLogLevel(s string) slog.Level {
	var level slog.Level
	if err := level.UnmarshalText([]byte(s)); err != nil {
		fmt.Fprintf(os.Stderr, "miniagent: invalid -log-level %q (want debug|info|warn|error)\n", s)
		os.Exit(1)
	}
	return level
}

func requireConfig(configPath string) (*miniagent.Config, error) {
	if configPath != "" {
		return miniagent.LoadConfig(configPath)
	}
	p, err := findDefaultConfigPath()
	if err != nil {
		return nil, err
	}
	if p != "" {
		return miniagent.LoadConfig(p)
	}
	return nil, errors.New("miniagent config 不存在（-config <path> 或 ~/.miniagent/miniagent.json）")
}

// findDefaultConfigPath 查找默认 config 路径：仅 ~/.miniagent/miniagent.json。
// 返回找到的路径的绝对路径；若都未找到返回空字符串；若 stat 出错返回该错误。
func findDefaultConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("定位 home 目录失败：%w", err)
	}
	p := filepath.Join(home, ".miniagent", "miniagent.json")
	if _, err := os.Stat(p); err == nil {
		abs, _ := filepath.Abs(p)
		return abs, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("stat config %q: %w", p, err)
	}
	return "", nil
}

// collectOverrides 用 flag.Visit 收集「显式传入」的 flag（未传入置 nil），供 Resolve 裁决。
// P2 后仅保留 CLI 核心参数；策略参数（summary/duration/window 等）只在 config，不经 CLI。
func collectOverrides(f *cliFlags) miniagent.CLIOverrides {
	set := map[string]bool{}
	flag.Visit(func(fl *flag.Flag) { set[fl.Name] = true })
	o := miniagent.CLIOverrides{}
	if set["model"] {
		o.Model = f.model
	}
	if set["thinking"] {
		o.Thinking = f.thinking
	}
	if set["mode"] {
		o.Mode = f.mode
	}
	if set["system"] {
		o.System = f.system
	}
	if set["workdir"] {
		o.Workdir = f.workdir
	}
	if set["session"] {
		o.Session = f.session
	}
	if set["max-tokens"] {
		o.MaxTokens = f.maxTokens
	}
	if set["max-iterations"] {
		o.MaxIterations = f.maxIterations
	}
	if set["stream"] {
		o.Stream = f.stream
	}
	if set["result-only"] {
		o.ResultOnly = f.resultOnly
	}
	return o
}

// resolveFinalKey：config(provider.Key) > env。机密建议用环境变量注入，避免明文入 config。
func resolveFinalKey(providerKey string) string {
	if providerKey != "" {
		return providerKey
	}
	return os.Getenv("MINIAGENT_API_KEY")
}

func validateConversation(resolved *miniagent.Resolved, f *cliFlags) {
	if *f.stream && *f.resultOnly {
		fmt.Fprintln(os.Stderr, "miniagent: -stream 与 -result-only 互斥")
		os.Exit(1)
	}
	if resolved.Mode == miniagent.ModeDefault && effectiveWorkdir(resolved, f) == "" {
		fmt.Fprintln(os.Stderr, "miniagent: default 模式需 -workdir（或 config run.workdir，或用 -mode auto）")
		os.Exit(1)
	}
}

func effectiveWorkdir(resolved *miniagent.Resolved, f *cliFlags) string {
	if resolved.Run.Workdir != nil && *resolved.Run.Workdir != "" {
		return *resolved.Run.Workdir
	}
	return *f.workdir
}

func maxDurationOf(resolved *miniagent.Resolved) time.Duration {
	if resolved.Run.MaxDuration != nil {
		return *resolved.Run.MaxDuration
	}
	return 0
}

func shellTimeoutOf(resolved *miniagent.Resolved) time.Duration {
	if resolved.Run.ShellTimeout != nil {
		return *resolved.Run.ShellTimeout
	}
	return 0
}

func fileOpTimeoutOf(resolved *miniagent.Resolved) time.Duration {
	if resolved.Run.FileOpTimeout != nil {
		return *resolved.Run.FileOpTimeout
	}
	return 0
}

func writeTimeoutOf(resolved *miniagent.Resolved) time.Duration {
	if resolved.Run.WriteTimeout != nil {
		return *resolved.Run.WriteTimeout
	}
	return 0
}

func httpTimeoutOf(resolved *miniagent.Resolved) time.Duration {
	if resolved.Run.HTTPTimeout != nil {
		return *resolved.Run.HTTPTimeout
	}
	return 0
}

func maxReadFileBytesOf(resolved *miniagent.Resolved) int {
	if resolved.Run.MaxReadFileBytes != nil {
		return *resolved.Run.MaxReadFileBytes
	}
	return 0
}

func maxShellOutputCharsOf(resolved *miniagent.Resolved) int {
	if resolved.Run.MaxShellOutputChars != nil {
		return *resolved.Run.MaxShellOutputChars
	}
	return 0
}

func maxSessionBytesOf(resolved *miniagent.Resolved) int {
	if resolved.Run.MaxSessionBytes != nil {
		return *resolved.Run.MaxSessionBytes
	}
	return 0
}

func buildHooks(resultOnly bool) miniagent.LoopHooks {
	if resultOnly {
		// subagent fork：stdout 纯文本即结果，不发 NDJSON 事件。
		return miniagent.LoopHooks{}
	}
	emit := miniagent.ToolUseWriter(os.Stdout)
	return miniagent.LoopHooks{
		OnToolUse: func(name, input string) error { return emit(name, input) },
		OnToolResult: func(name, callID string, r miniagent.ToolResult) error {
			return miniagent.EmitToolResult(os.Stdout, name, callID, r)
		},
		OnDelta: func(step int, kind miniagent.DeltaKind, text string) error {
			return miniagent.EmitDelta(os.Stdout, step, kind, text)
		},
	}
}

func emitRunError(err error, resultOnly bool, logger *slog.Logger) {
	if resultOnly {
		fmt.Printf("error: %s\n", err.Error())
		return
	}
	if eerr := miniagent.EmitError(os.Stdout, err.Error()); eerr != nil {
		logger.Warn("emit error failed", "error", eerr)
		fmt.Fprintf(os.Stderr, "miniagent: %v\n", err)
	}
}

func emitRunResult(result miniagent.Result, model string, resultOnly bool, logger *slog.Logger) {
	if resultOnly {
		fmt.Println(result.Text)
		return
	}
	if err := miniagent.EmitResult(os.Stdout, result, model); err != nil {
		logger.Warn("emit result failed", "error", err)
		fmt.Fprintf(os.Stderr, "miniagent: emit result failed: %v (text: %.200q)\n", err, result.Text)
		os.Exit(1)
	}
}

// absConfigPath 返回实际加载的 config 绝对路径（显式 -config 或默认 ~/.miniagent/miniagent.json），
// 供 subagent fork 引导注入。cfg 始终非 nil（S1 删裸模式）。
// 逻辑与 requireConfig 保持一致：显式 -config > ~/.miniagent/miniagent.json。
func absConfigPath(configPath string) string {
	if configPath != "" {
		abs, _ := filepath.Abs(configPath)
		return abs
	}
	p, _ := findDefaultConfigPath()
	return p
}
