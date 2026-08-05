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
	Provider, Model, Thinking, Mode, System, Workdir *string
	MaxTokens, MaxIterations                         *int
	Stream, ResultOnly                               *bool
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
	ContextKeepReasoning                                                                         *int
	ContextKeepToolArgs                                                                          *int
	ContextKeepReasoningChars                                                                    *int
}

type Resolved struct {
	Provider           ProviderConfig
	ModelID            string
	CompactionProvider ProviderConfig
	CompactionModelID  string
	MemoryProvider     ProviderConfig
	MemoryModelID      string
	Thinking           string
	Mode               string
	System             string
	SummaryRequest     string
	SummarizerPrompt   string
	Session            SessionConfig
	Run                ResolvedRun
	// 会话结束自动抽取记忆的生效配置（已应用默认值：AutoUpdate 缺省 true，MaxPerSession 缺省 3）。
	MemoryAutoUpdate    bool
	MemoryMaxPerSession int
	MemoryExtractPrompt string
}

// Resolve 按 cli>config>builtin 裁决产出 Resolved。cfg 必须非 nil（S1 删裸模式后始终有 config）。
// key 不在此处理（main 据 provider.Key/env 决定）。
func Resolve(cfg *Config, o CLIOverrides) (*Resolved, error) {
	if cfg == nil {
		return nil, errors.New("Resolve: cfg 为 nil（S1 后 config 必须存在）")
	}
	r := &Resolved{Session: cfg.Session}

	// 成对规则：CLI -provider/-model 须同传（只传其一报错）；不传则以 config defaults 对为准。
	cliProv := o.Provider != nil && *o.Provider != ""
	cliModel := o.Model != nil && *o.Model != ""
	if cliProv != cliModel {
		return nil, errors.New("-provider 与 -model 须成对传入（同传覆盖，同缺以 config 为准）")
	}
	defProv, defModel, err := resolveProviderModel(cfg, "defaults", cfg.Defaults.Provider, cfg.Defaults.Model)
	if err != nil {
		return nil, err
	}
	r.Provider, r.ModelID = defProv, defModel
	if cliProv {
		p, err := FindProvider(cfg, *o.Provider)
		if err != nil {
			return nil, fmt.Errorf("-provider: %w", err)
		}
		r.Provider, r.ModelID = p, *o.Model
	}

	// compaction / memory：成对可选——同设取自身（可跨 provider），同空整体回落 defaults 对。
	cp, cm, err := resolveOptionalPair(cfg, "compaction", cfg.Compaction.Provider, cfg.Compaction.Model, defProv, defModel)
	if err != nil {
		return nil, err
	}
	r.CompactionProvider, r.CompactionModelID = cp, cm
	mp, mm, err := resolveOptionalPair(cfg, "memory", cfg.Memory.Provider, cfg.Memory.Model, defProv, defModel)
	if err != nil {
		return nil, err
	}
	r.MemoryProvider, r.MemoryModelID = mp, mm

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

	// 记忆抽取默认值：auto_update 缺省 true，max_per_session 缺省/<=0 = 3。
	r.MemoryAutoUpdate = true
	if cfg.Memory.AutoUpdate != nil {
		r.MemoryAutoUpdate = *cfg.Memory.AutoUpdate
	}
	r.MemoryMaxPerSession = 3
	if cfg.Memory.MaxPerSession != nil && *cfg.Memory.MaxPerSession > 0 {
		r.MemoryMaxPerSession = *cfg.Memory.MaxPerSession
	}
	r.MemoryExtractPrompt = cfg.Memory.ExtractPrompt
	return r, nil
}

// resolveProviderModel 校验必填模型对（defaults）：两者均非空且 provider 已声明，
// 通过则返回该 provider 与原样 model id（model id 允许含 '/'，不再承担 provider 前缀语义）。
// label 是 JSON 段名（defaults），用于错误定位。validateConfig 与 Resolve 共用。
func resolveProviderModel(cfg *Config, label, provider, model string) (ProviderConfig, string, error) {
	if provider == "" {
		return ProviderConfig{}, "", fmt.Errorf("%s.provider 必填", label)
	}
	if model == "" {
		return ProviderConfig{}, "", fmt.Errorf("%s.model 必填", label)
	}
	p, err := FindProvider(cfg, provider)
	if err != nil {
		return ProviderConfig{}, "", fmt.Errorf("%s.provider: %w", label, err)
	}
	return p, model, nil
}

// resolveOptionalPair 校验可选模型对（compaction/memory）：provider/model 同空时整体回落
// defaults 对（def/defModel）；同设时 provider 须已声明；只设其一报错（成对规则不允许交叉回落）。
func resolveOptionalPair(cfg *Config, label, provider, model string, def ProviderConfig, defModel string) (ProviderConfig, string, error) {
	if provider == "" && model == "" {
		return def, defModel, nil
	}
	if provider == "" || model == "" {
		return ProviderConfig{}, "", fmt.Errorf("%s.provider 与 %s.model 须成对设置（同空则回落 defaults）", label, label)
	}
	p, err := FindProvider(cfg, provider)
	if err != nil {
		return ProviderConfig{}, "", fmt.Errorf("%s.provider: %w", label, err)
	}
	return p, model, nil
}

// FindProvider 按名查找 provider；未找到报错。provider/model 拆分后取代 ParseModelSpec。
func FindProvider(cfg *Config, name string) (ProviderConfig, error) {
	if cfg == nil {
		return ProviderConfig{}, errors.New("FindProvider: cfg 为 nil（S1 后 config 必须存在）")
	}
	for _, p := range cfg.Providers {
		if p.Name == name {
			return p, nil
		}
	}
	return ProviderConfig{}, fmt.Errorf("未知 provider %q", name)
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
	r.ContextKeepReasoning = intPtr(func(rc RunConfig) *int { return rc.ContextKeepReasoning })
	r.ContextKeepToolArgs = intPtr(func(rc RunConfig) *int { return rc.ContextKeepToolArgs })
	r.ContextKeepReasoningChars = intPtr(func(rc RunConfig) *int { return rc.ContextKeepReasoningChars })
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
