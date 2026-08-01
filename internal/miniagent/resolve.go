package miniagent

import (
	"errors"
	"time"
)

// CLIOverrides 收集「显式传入」的 CLI 参数（main 用 flag.Visit 区分未设置），供 Resolve
// 按 cli>config>builtin 优先级裁决。指针为 nil 表示未传入。
type CLIOverrides struct {
	Model, Thinking, Mode, System, Workdir, Session, ChatURL, ModelsURL *string
	MaxTokens, MaxIterations, MaxTotalTokens, ContextWindow             *int
	MaxDuration, ShellTimeout                                           *time.Duration
	Stream, ResultOnly                                                  *bool
}

// ResolvedRun 是 Resolve 输出的运行参数（duration 已解析）；nil 表示未设置，main 回落 flag 默认。
type ResolvedRun struct {
	Workdir                                                 *string
	MaxTokens, MaxIterations, MaxTotalTokens, ContextWindow *int
	MaxDuration, ShellTimeout                               *time.Duration
	Stream                                                  *bool
}

type Resolved struct {
	Provider   ProviderConfig
	ModelID    string
	Thinking   string
	Mode       string
	System     string
	Session    SessionConfig
	Compaction CompactionConfig
	Run        ResolvedRun
}

// Resolve 按 cli>config>builtin 裁决产出 Resolved。cfg 可 nil（裸模式：用 -chat-url 构造
// 隐式 cli provider，model 取裸 id）。key 不在此处理（main 据 provider.Key/env 决定）。
func Resolve(cfg *Config, o CLIOverrides) (*Resolved, error) {
	r := &Resolved{}
	if cfg != nil {
		r.Session = cfg.Session
		r.Compaction = cfg.Compaction
	}

	if cfg == nil {
		// 裸模式：隐式 cli provider
		if o.ChatURL == nil || *o.ChatURL == "" {
			return nil, errors.New("裸模式需 -chat-url")
		}
		if o.Model == nil || *o.Model == "" {
			return nil, errors.New("裸模式需 -model")
		}
		p := ProviderConfig{Name: "cli", ChatURL: *o.ChatURL}
		if o.ModelsURL != nil {
			p.ModelsURL = *o.ModelsURL
		}
		r.Provider = p
		r.ModelID = *o.Model
	} else {
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
	}

	switch {
	case o.Thinking != nil:
		r.Thinking = *o.Thinking
	case cfg != nil && cfg.Defaults.Thinking != "":
		r.Thinking = cfg.Defaults.Thinking
	}
	switch {
	case o.Mode != nil && *o.Mode != "":
		r.Mode = *o.Mode
	case cfg != nil && cfg.Defaults.Mode != "":
		r.Mode = cfg.Defaults.Mode
	default:
		r.Mode = "default"
	}
	switch {
	case o.System != nil && *o.System != "":
		r.System = *o.System
	case cfg != nil && cfg.Defaults.SystemPrompt != "":
		r.System = cfg.Defaults.SystemPrompt
	}

	r.Run = resolveRun(cfg, o)
	return r, nil
}

func resolveRun(cfg *Config, o CLIOverrides) ResolvedRun {
	var r ResolvedRun
	intPtr := func(get func(RunConfig) *int) *int {
		if cfg == nil {
			return nil
		}
		return get(cfg.Run)
	}
	durPtr := func(get func(RunConfig) *string) *string {
		if cfg == nil {
			return nil
		}
		return get(cfg.Run)
	}
	if o.Workdir != nil && *o.Workdir != "" {
		r.Workdir = o.Workdir
	} else if cfg != nil {
		r.Workdir = cfg.Run.Workdir
	}
	r.MaxTokens = pickInt(o.MaxTokens, intPtr(func(rc RunConfig) *int { return rc.MaxTokens }))
	r.MaxIterations = pickInt(o.MaxIterations, intPtr(func(rc RunConfig) *int { return rc.MaxIterations }))
	r.MaxTotalTokens = pickInt(o.MaxTotalTokens, intPtr(func(rc RunConfig) *int { return rc.MaxTotalTokens }))
	r.ContextWindow = pickInt(o.ContextWindow, intPtr(func(rc RunConfig) *int { return rc.ContextWindow }))
	if o.Stream != nil {
		r.Stream = o.Stream
	} else if cfg != nil {
		r.Stream = cfg.Run.Stream
	}
	r.MaxDuration = pickDur(o.MaxDuration, durPtr(func(rc RunConfig) *string { return rc.MaxDuration }))
	r.ShellTimeout = pickDur(o.ShellTimeout, durPtr(func(rc RunConfig) *string { return rc.ShellTimeout }))
	return r
}

func pickInt(ov, cv *int) *int {
	if ov != nil {
		return ov
	}
	return cv
}

func pickDur(ov *time.Duration, cv *string) *time.Duration {
	if ov != nil {
		return ov
	}
	if cv == nil {
		return nil
	}
	d, err := time.ParseDuration(*cv)
	if err != nil {
		return nil
	}
	return &d
}
