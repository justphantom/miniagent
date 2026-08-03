package miniagent

import (
	"errors"
	"fmt"
	"time"
)

// CLIOverrides 收集「显式传入」的 CLI 参数（main 用 flag.Visit 区分未设置），供 Resolve
// 按 cli>config>builtin 优先级裁决。指针为 nil 表示未传入。P2 后仅保留 CLI 核心参数；
// 策略参数（summary/duration/window 等）只在 config，故此处不含。
type CLIOverrides struct {
	Model, Thinking, Mode, System, Workdir, Session *string
	MaxTokens, MaxIterations                        *int
	Stream, ResultOnly                              *bool
}

// ResolvedRun 是 Resolve 输出的运行参数（duration 已解析）；nil 表示未设置，main 回落 flag 默认。
type ResolvedRun struct {
	Workdir                                                                                      *string
	MaxTokens, MaxIterations, MaxTotalTokens, ContextWindow                                      *int
	MaxDuration, ShellTimeout, FileOpTimeout, WriteTimeout, HTTPTimeout                          *time.Duration
	Stream                                                                                       *bool
	MaxToolResultChars, MaxFileResultChars, MaxParallelTools, ContextKeepRecent, SummaryMaxChars *int
	MaxReadFileBytes, MaxShellOutputChars, MaxSessionBytes                                       *int
	SummaryMaxTokens, GrepMaxMatches, MemoryRecentN, ContextTrimToolChars                        *int
}

type Resolved struct {
	Provider         ProviderConfig
	ModelID          string
	Thinking         string
	Mode             string
	System           string
	SummaryRequest   string
	SummarizerPrompt string
	Session          SessionConfig
	Compaction       CompactionConfig
	Run              ResolvedRun
}

// Resolve 按 cli>config>builtin 裁决产出 Resolved。cfg 必须非 nil（S1 删裸模式后始终有 config）。
// key 不在此处理（main 据 provider.Key/env 决定）。
func Resolve(cfg *Config, o CLIOverrides) (*Resolved, error) {
	if cfg == nil {
		return nil, errors.New("Resolve: cfg 为 nil（S1 后 config 必须存在）")
	}
	r := &Resolved{Session: cfg.Session, Compaction: cfg.Compaction}

	spec := ""
	switch {
	case o.Model != nil && *o.Model != "":
		spec = *o.Model
	case cfg.Defaults.Model != "":
		spec = cfg.Defaults.Model
	}
	if spec == "" {
		return nil, errors.New("无法确定 model（用 -model 或 config defaults.model）")
	}
	p, modelID, err := ParseModelSpec(spec, cfg)
	if err != nil {
		return nil, err
	}
	r.Provider = p
	r.ModelID = modelID

	switch {
	case o.Thinking != nil:
		r.Thinking = *o.Thinking
	case cfg.Defaults.Thinking != "":
		r.Thinking = cfg.Defaults.Thinking
	}
	customKeys := map[string]bool{}
	if r.Provider.Thinking != nil {
		for k := range r.Provider.Thinking.Map {
			customKeys[k] = true
		}
	}
	if err := validateThinking(r.Thinking, customKeys); err != nil {
		return nil, fmt.Errorf("thinking: %w", err)
	}
	switch {
	case o.Mode != nil && *o.Mode != "":
		r.Mode = *o.Mode
	case cfg.Defaults.Mode != "":
		r.Mode = cfg.Defaults.Mode
	default:
		r.Mode = "default"
	}
	if r.Mode != ModeDefault && r.Mode != ModeAuto {
		return nil, fmt.Errorf("mode %q 非法（%s|%s）", r.Mode, ModeDefault, ModeAuto)
	}
	switch {
	case o.System != nil && *o.System != "":
		r.System = *o.System
	case cfg.Defaults.SystemPrompt != "":
		r.System = cfg.Defaults.SystemPrompt
	}
	// summary_request / summarizer_prompt 仅 config 来源（P2 移出 CLI）。
	if cfg.Defaults.SummaryRequest != "" {
		r.SummaryRequest = cfg.Defaults.SummaryRequest
	}
	if cfg.Defaults.SummarizerPrompt != "" {
		r.SummarizerPrompt = cfg.Defaults.SummarizerPrompt
	}

	run, rerr := resolveRun(cfg, o)
	if rerr != nil {
		return nil, rerr
	}
	r.Run = run
	return r, nil
}

func resolveRun(cfg *Config, o CLIOverrides) (ResolvedRun, error) {
	var r ResolvedRun
	// cfg 在 Resolve 入口已断言非 nil（S1 删裸模式），此处直接读 cfg.Run。
	intPtr := func(get func(RunConfig) *int) *int { return get(cfg.Run) }
	strPtr := func(get func(RunConfig) *string) *string { return get(cfg.Run) }
	if o.Workdir != nil && *o.Workdir != "" {
		r.Workdir = o.Workdir
	} else {
		r.Workdir = cfg.Run.Workdir
	}
	r.MaxTokens = pickInt(o.MaxTokens, intPtr(func(rc RunConfig) *int { return rc.MaxTokens }))
	r.MaxIterations = pickInt(o.MaxIterations, intPtr(func(rc RunConfig) *int { return rc.MaxIterations }))
	// 以下均为仅 config 来源（P2 移出 CLI）：直接读 config。
	r.MaxTotalTokens = intPtr(func(rc RunConfig) *int { return rc.MaxTotalTokens })
	r.ContextWindow = intPtr(func(rc RunConfig) *int { return rc.ContextWindow })
	// S4 策略化常量：仅 config 来源。
	r.MaxToolResultChars = intPtr(func(rc RunConfig) *int { return rc.MaxToolResultChars })
	r.MaxFileResultChars = intPtr(func(rc RunConfig) *int { return rc.MaxFileResultChars })
	r.MaxParallelTools = intPtr(func(rc RunConfig) *int { return rc.MaxParallelTools })
	r.ContextKeepRecent = intPtr(func(rc RunConfig) *int { return rc.ContextKeepRecent })
	r.SummaryMaxChars = intPtr(func(rc RunConfig) *int { return rc.SummaryMaxChars })
	if o.Stream != nil {
		r.Stream = o.Stream
	} else {
		r.Stream = cfg.Run.Stream
	}
	var err error
	r.MaxDuration, err = parseDur(strPtr(func(rc RunConfig) *string { return rc.MaxDuration }), "run.max_duration")
	if err != nil {
		return r, err
	}
	r.ShellTimeout, err = parseDur(strPtr(func(rc RunConfig) *string { return rc.ShellTimeout }), "run.shell_timeout")
	if err != nil {
		return r, err
	}
	r.FileOpTimeout, err = parseDur(strPtr(func(rc RunConfig) *string { return rc.FileOpTimeout }), "run.file_op_timeout")
	if err != nil {
		return r, err
	}
	r.WriteTimeout, err = parseDur(strPtr(func(rc RunConfig) *string { return rc.WriteTimeout }), "run.write_timeout")
	if err != nil {
		return r, err
	}
	r.HTTPTimeout, err = parseDur(strPtr(func(rc RunConfig) *string { return rc.HTTPTimeout }), "run.http_timeout")
	if err != nil {
		return r, err
	}
	r.MaxReadFileBytes = intPtr(func(rc RunConfig) *int { return rc.MaxReadFileBytes })
	r.MaxShellOutputChars = intPtr(func(rc RunConfig) *int { return rc.MaxShellOutputChars })
	r.MaxSessionBytes = intPtr(func(rc RunConfig) *int { return rc.MaxSessionBytes })
	// S4 策略化常量（接续上文，仅 config 来源）：与 MaxToolResultChars 等同批，须一并装配，
	// 否则 config 值不入 ResolvedRun、main 的 Set* 收到 0 当作未设置而回落内置默认。
	r.SummaryMaxTokens = intPtr(func(rc RunConfig) *int { return rc.SummaryMaxTokens })
	r.GrepMaxMatches = intPtr(func(rc RunConfig) *int { return rc.GrepMaxMatches })
	r.MemoryRecentN = intPtr(func(rc RunConfig) *int { return rc.MemoryRecentN })
	r.ContextTrimToolChars = intPtr(func(rc RunConfig) *int { return rc.ContextTrimToolChars })
	return r, nil
}

func pickInt(ov, cv *int) *int {
	if ov != nil {
		return ov
	}
	return cv
}

// parseDur 解析 config 中的 duration 字符串（"30s"）；nil 表示未配置。
func parseDur(cv *string, label string) (*time.Duration, error) {
	if cv == nil {
		return nil, nil
	}
	d, err := time.ParseDuration(*cv)
	if err != nil {
		return nil, fmt.Errorf("config %s %q: %w", label, *cv, err)
	}
	if d < 0 {
		return nil, fmt.Errorf("config %s %q: 负值不合法", label, *cv)
	}
	return &d, nil
}
