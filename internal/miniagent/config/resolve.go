package config

import (
	"errors"
	"fmt"
	"time"

	"github.com/justphantom/miniagent/internal/miniagent"
)

// CLIOverrides collects the "explicitly passed" CLI parameters (main uses flag.Visit to distinguish unset),
// for Resolve to arbitrate by cli>config>builtin precedence. A nil pointer means not passed. After P2 only
// core CLI parameters remain; strategy parameters (summary/duration/window etc.) live only in config, so they are absent here.
type CLIOverrides struct {
	Provider, Model, Thinking, Mode        *string
	MaxIterations                          *int
	Stream, ResultOnly, ConfirmDestructive *bool
}

// ResolvedRun holds the run parameters produced by Resolve — only fields that need arbitration or parsing
// (cli>config three-state arbitration + duration parsing). Pure pass-through fields (config-only source, no CLI
// override, no parsing) bypass this indirection; consumers read Resolved.RunConfig (= cfg.Run) directly.
type ResolvedRun struct {
	MaxIterations                                          *int
	Stream                                                 *bool
	ConfirmDestructive                                     *bool
	MaxDuration, ShellTimeout, FileOpTimeout, WriteTimeout *time.Duration
	ToolOutputRetention                                    *time.Duration
	WebTimeout                                             *time.Duration
}

type Resolved struct {
	Provider                 ProviderConfig
	ModelID                  string
	CompactionProvider       ProviderConfig
	CompactionModelID        string
	CompactionAuto           bool
	CompactionReserved       int
	Thinking                 string
	Mode                     string
	System                   string
	RulesFile                string
	SummaryRequest           string
	SummarizerPrompt         string
	SubagentGuidance         string
	SummaryCreateInstruction string
	SummaryUpdateInstruction string
	SummaryTemplate          string
	Session                  SessionConfig
	Run                      ResolvedRun
	RunConfig                RunConfig // original cfg.Run: source of pure pass-through fields; consumers read directly, bypassing the ResolvedRun indirection
	// Layered model parameters (consumers read these instead of Run/RunConfig):
	// MaxTokens/ContextWindow: model>provider>global (no cli); HTTPTimeout: provider>global (no model).
	MaxTokens     *int
	ContextWindow *int
	HTTPTimeout   *time.Duration
}

// Resolve arbitrates by cli>config>builtin to produce Resolved. cfg must be non-nil (after S1 removed bare mode, config always exists).
// The key is not handled here (main decides based on provider.Key/env).
func Resolve(cfg *Config, o CLIOverrides) (*Resolved, error) {
	if cfg == nil {
		return nil, errors.New("Resolve: cfg is nil (config must exist after S1)")
	}
	r := &Resolved{Session: cfg.Session, RunConfig: cfg.Run}

	// Pair rule: CLI -provider/-model must be passed together (passing only one errors); passing neither uses the config defaults pair.
	cliProv := o.Provider != nil && *o.Provider != ""
	cliModel := o.Model != nil && *o.Model != ""
	if cliProv != cliModel {
		return nil, errors.New("-provider and -model must be passed as a pair (both override; neither falls back to config)")
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

	// Model parameter layering (model>provider>global; max_tokens/context_window have no cli).
	// After the provider is selected, model-level config is looked up in its Models and arbitrated layer by layer; http_timeout is provider>global only (no model).
	mc, _ := findModelConfig(r.Provider, r.ModelID)
	r.MaxTokens = pickMPG(mc.MaxTokens, r.Provider.MaxTokens, cfg.Run.MaxTokens)
	r.ContextWindow = pickMPG(mc.ContextWindow, r.Provider.ContextWindow, cfg.Run.ContextWindow)
	httpTimeoutStr := cfg.Run.HTTPTimeout
	if r.Provider.HTTPTimeout != nil {
		httpTimeoutStr = r.Provider.HTTPTimeout
	}
	if r.HTTPTimeout, err = ParseDuration(httpTimeoutStr, "http_timeout"); err != nil {
		return nil, err
	}

	// compaction: optional pair — both set takes itself (may cross providers); both empty falls back to the defaults pair.
	cp, cm, err := resolveOptionalPair(cfg, "compaction", cfg.Compaction.Provider, cfg.Compaction.Model, defProv, defModel)
	if err != nil {
		return nil, err
	}
	r.CompactionProvider, r.CompactionModelID = cp, cm
	// §P1-B: silent usage overflow detection switch + reserve. Auto default (nil) = enabled; Reserved default (nil) is 0,
	// falling back to min(compactionBuffer=20000, max_tokens) via the compaction engine's compactionReserve — Resolve does not fall back.
	r.CompactionAuto = cfg.Compaction.Auto == nil || *cfg.Compaction.Auto
	if cfg.Compaction.Reserved != nil {
		r.CompactionReserved = *cfg.Compaction.Reserved
	}

	// thinking level four states: cli > model > provider(ThinkingLevel) > defaults.
	switch {
	case o.Thinking != nil:
		r.Thinking = *o.Thinking
	default:
		switch {
		case mc.Thinking != nil:
			r.Thinking = *mc.Thinking
		case r.Provider.ThinkingLevel != nil:
			r.Thinking = *r.Provider.ThinkingLevel
		default:
			r.Thinking = cfg.Defaults.Thinking
		}
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
	if r.Mode != miniagent.ModeDefault && r.Mode != miniagent.ModeAuto {
		return nil, fmt.Errorf("mode %q is invalid (%s|%s)", r.Mode, miniagent.ModeDefault, miniagent.ModeAuto)
	}
	r.System = cfg.Defaults.SystemPrompt
	r.RulesFile = cfg.Defaults.RulesFile
	// summary_request / summarizer_prompt are config-only sources (moved out of CLI in P2).
	if cfg.Defaults.SummaryRequest != "" {
		r.SummaryRequest = cfg.Defaults.SummaryRequest
	}
	if cfg.Defaults.SummarizerPrompt != "" {
		r.SummarizerPrompt = cfg.Defaults.SummarizerPrompt
	}
	r.SubagentGuidance = cfg.Defaults.SubagentGuidance
	r.SummaryCreateInstruction = cfg.Defaults.SummaryCreateInstruction
	r.SummaryUpdateInstruction = cfg.Defaults.SummaryUpdateInstruction
	r.SummaryTemplate = cfg.Defaults.SummaryTemplate

	run, rerr := resolveRun(cfg, o)
	if rerr != nil {
		return nil, rerr
	}
	r.Run = run

	return r, nil
}

// resolveProviderModel validates the required model pair (defaults): both non-empty and the provider is declared;
// on success it returns that provider and the model id as-is (the model id may contain '/', no longer carrying provider-prefix semantics).
// label is the JSON section name (defaults), used for error location. Shared by validateConfig and Resolve.
func resolveProviderModel(cfg *Config, label, provider, model string) (ProviderConfig, string, error) {
	if provider == "" {
		return ProviderConfig{}, "", fmt.Errorf("%s.provider is required", label)
	}
	if model == "" {
		return ProviderConfig{}, "", fmt.Errorf("%s.model is required", label)
	}
	p, err := FindProvider(cfg, provider)
	if err != nil {
		return ProviderConfig{}, "", fmt.Errorf("%s.provider: %w", label, err)
	}
	return p, model, nil
}

// resolveOptionalPair validates the optional model pair (compaction): when provider/model are both empty it falls back
// to the defaults pair (def/defModel); when both are set the provider must be declared; setting only one errors (the pair rule disallows cross fallback).
func resolveOptionalPair(cfg *Config, label, provider, model string, def ProviderConfig, defModel string) (ProviderConfig, string, error) {
	if provider == "" && model == "" {
		return def, defModel, nil
	}
	if provider == "" || model == "" {
		return ProviderConfig{}, "", fmt.Errorf("%s.provider and %s.model must be set as a pair (both empty falls back to defaults)", label, label)
	}
	p, err := FindProvider(cfg, provider)
	if err != nil {
		return ProviderConfig{}, "", fmt.Errorf("%s.provider: %w", label, err)
	}
	return p, model, nil
}

// FindProvider looks up a provider by name; it errors when not found. It replaces ParseModelSpec after the provider/model split.
func FindProvider(cfg *Config, name string) (ProviderConfig, error) {
	if cfg == nil {
		return ProviderConfig{}, errors.New("FindProvider: cfg is nil (config must exist after S1)")
	}
	for _, p := range cfg.Providers {
		if p.Name == name {
			return p, nil
		}
	}
	return ProviderConfig{}, fmt.Errorf("unknown provider %q", name)
}

func resolveRun(cfg *Config, o CLIOverrides) (ResolvedRun, error) {
	var r ResolvedRun
	// Three-state arbitration (cli>config): MaxIterations/Stream can be overridden by CLI; nil=unset.
	// MaxTokens is not here (no cli, layering is in Resolve); HTTPTimeout moved to Resolved (provider>global).
	// Workdir is deliberately NOT arbitrated here: it comes ONLY from the -workdir flag (read directly via
	// effectiveWorkdir), never from config — keeps the agent's working directory pinned to the explicit flag.
	r.MaxIterations = pickInt(o.MaxIterations, cfg.Run.MaxIterations)
	if o.Stream != nil {
		r.Stream = o.Stream
	} else {
		r.Stream = cfg.Run.Stream
	}
	if o.ConfirmDestructive != nil {
		r.ConfirmDestructive = o.ConfirmDestructive
	} else {
		r.ConfirmDestructive = cfg.Run.ConfirmDestructive
	}
	// durations: *string → *time.Duration. Pure pass-through fields bypass this (consumers read Resolved.RunConfig).
	var err error
	r.MaxDuration, err = ParseDuration(cfg.Run.MaxDuration, "run.max_duration")
	if err != nil {
		return r, err
	}
	r.ShellTimeout, err = ParseDuration(cfg.Run.ShellTimeout, "run.shell_timeout")
	if err != nil {
		return r, err
	}
	r.FileOpTimeout, err = ParseDuration(cfg.Run.FileOpTimeout, "run.file_op_timeout")
	if err != nil {
		return r, err
	}
	r.WriteTimeout, err = ParseDuration(cfg.Run.WriteTimeout, "run.write_timeout")
	if err != nil {
		return r, err
	}
	r.ToolOutputRetention, err = ParseDuration(cfg.Run.ToolOutputRetention, "run.tool_output_retention")
	if err != nil {
		return r, err
	}
	r.WebTimeout, err = ParseDuration(cfg.Run.WebTimeout, "run.web_timeout")
	if err != nil {
		return r, err
	}
	return r, nil
}
