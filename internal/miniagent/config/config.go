package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/justphantom/miniagent/internal/miniagent"
)

// Config is the root structure of ./miniagent.json. Defaults=strategy (model/thinking/mode/system),
// Run=runtime parameters (limits/timeouts/paths/stream); pointers distinguish unset from zero value (review v2 #12).
type Config struct {
	Session    SessionConfig    `json:"session"`
	Providers  []ProviderConfig `json:"providers"`
	Defaults   DefaultsConfig   `json:"defaults"`
	Run        RunConfig        `json:"run"`
	Compaction CompactionConfig `json:"compaction"`
}

type SessionConfig struct {
	Dir string `json:"dir"`
}

type ProviderConfig struct {
	Name      string `json:"name"`
	Kind      string `json:"kind,omitempty"` // wire format: "" / "openai" (default, Chat Completions) or "anthropic" (Messages API)
	ChatURL   string `json:"chat_url"`
	ModelsURL string `json:"models_url,omitempty"`
	Key       string `json:"key,omitempty"`
	// Models is the list of models this provider supports; each entry can carry model-level parameters overriding provider-level and global.
	// breaking: was []string, now requires object form [{"name":...}].
	Models []ModelConfig `json:"models,omitempty"`
	// Thinking is the wire mapping ({field,map}) for the thinking field, provider-level — the provider must declare it when thinking is enabled.
	Thinking *miniagent.ThinkingMapping `json:"thinking,omitempty"`
	// Provider-level model parameters (override global run): output limit, context window, HTTP timeout (transport layer, same provider shares endpoint).
	MaxTokens     *int    `json:"max_tokens,omitempty"`
	ContextWindow *int    `json:"context_window,omitempty"`
	HTTPTimeout   *string `json:"http_timeout,omitempty"`
	// ThinkingLevel is the provider-level thinking level (a level string), overriding defaults.thinking. The json key uses
	// thinking_level to avoid clashing with Thinking (the wire mapping, key "thinking").
	ThinkingLevel *string `json:"thinking_level,omitempty"`
	// Headers are per-provider custom request headers, injected with each request; they do not override Authorization / Content-Type.
	Headers map[string]string `json:"headers,omitempty"`
	// StreamAllowUnterminated (opt-in) accepts a streaming response that emitted content then ended without [DONE]/finish_reason
	// (a connection drop) as success, returning the partial Response. For non-compliant endpoints (some vLLM/Ollama configs)
	// that emit content then close with no terminator; default false = the drop is a hard error. Affects streaming only.
	StreamAllowUnterminated *bool `json:"stream_allow_unterminated,omitempty"`
	// Cache (anthropic only) toggles prompt-caching cache_control breakpoints on the system prompt and the
	// last user message. nil=auto (anthropic provider defaults to enabled); false=kill-switch. No effect on openai.
	Cache *bool `json:"cache,omitempty"`
}

// ModelConfig is the configuration for a single model under a provider: Name is required, the rest are model-level parameters
// overriding provider-level and global. max_tokens/context_window follow model>provider>global; thinking level follows
// cli>model>provider>global; http_timeout does not enter model-level (transport-layer attribute).
type ModelConfig struct {
	Name          string  `json:"name"`
	MaxTokens     *int    `json:"max_tokens,omitempty"`
	ContextWindow *int    `json:"context_window,omitempty"`
	Thinking      *string `json:"thinking,omitempty"` // model-level level (model has no wire mapping, key "thinking" is the level)
}

// ModelRef is a usable provider/model combination (the return unit of ListAllModels).
// provider and model are separated, so consumers do not need to split the "provider/model_id" text
// (model id itself may contain '/', making text splitting ambiguous).
type ModelRef struct {
	Provider, Model string
}

// DefaultsConfig's Provider/Model are both required (after provider/model split, the "provider/id" string is no longer parsed).
type DefaultsConfig struct {
	Provider         string `json:"provider"`
	Model            string `json:"model"`
	Thinking         string `json:"thinking,omitempty"`
	Mode             string `json:"mode,omitempty"`
	SystemPrompt     string `json:"system_prompt,omitempty"`
	SummaryRequest   string `json:"summary_request,omitempty"`
	SummarizerPrompt string `json:"summarizer_prompt,omitempty"`
	// SubagentGuidance is the subagent fork bootstrap template, injected at the end of the system prompt; placeholders {config_path}/{mode}
	// (config_path is wrapped via shellSingleQuote). Empty uses the built-in default.
	SubagentGuidance string `json:"subagent_guidance,omitempty"`
	// SummaryCreateInstruction/SummaryUpdateInstruction are the compaction summary instruction templates (CREATE/UPDATE modes),
	// with placeholder {max_chars}. Empty uses the built-in default. SummaryTemplate is the fixed summary structure template (no variables); empty uses the built-in default.
	SummaryCreateInstruction string `json:"summary_create_instruction,omitempty"`
	SummaryUpdateInstruction string `json:"summary_update_instruction,omitempty"`
	SummaryTemplate          string `json:"summary_template,omitempty"`
}

// RunConfig's duration fields use *string ("30s"); Resolve parses them into time.Duration,
// keeping the config file readable without a custom JSON type.
type RunConfig struct {
	MaxTokens      *int    `json:"max_tokens,omitempty"`
	MaxIterations  *int    `json:"max_iterations,omitempty"`
	MaxTotalTokens *int    `json:"max_tokens_total,omitempty"`
	ContextWindow  *int    `json:"context_window,omitempty"`
	MaxDuration    *string `json:"max_duration,omitempty"`
	ShellTimeout   *string `json:"shell_timeout,omitempty"`
	FileOpTimeout  *string `json:"file_op_timeout,omitempty"`
	WriteTimeout   *string `json:"write_timeout,omitempty"`
	Stream         *bool   `json:"stream,omitempty"`
	// ConfirmDestructive (opt-in, S-2) gates write/edit + dangerous shell behind an interactive prompt; in non-interactive
	// /subagent mode destructive tools are denied unless MINIAGENT_AUTO_APPROVE=1. nil/unset=false (current behavior preserved).
	ConfirmDestructive *bool `json:"confirm_destructive,omitempty"`
	// S4 strategized constants (<=0 or unset = built-in default).
	MaxToolResultChars  *int    `json:"max_tool_result_chars,omitempty"`
	MaxFileResultChars  *int    `json:"max_file_result_chars,omitempty"`
	MaxParallelTools    *int    `json:"max_parallel_tools,omitempty"`
	ContextKeepRecent   *int    `json:"context_keep_recent,omitempty"`
	SummaryMaxChars     *int    `json:"summary_max_chars,omitempty"`
	HTTPTimeout         *string `json:"http_timeout,omitempty"`
	MaxReadFileBytes    *int    `json:"max_read_file_bytes,omitempty"`
	MaxShellOutputChars *int    `json:"max_shell_output_chars,omitempty"`
	// ShellStreamWindowBytes is the sliding-window byte cap for shell output (keeps the tail, §P1-D); unset/<=0 uses the default max_shell_output_chars*8.
	ShellStreamWindowBytes *int `json:"shell_stream_window_bytes,omitempty"`
	MaxSessionBytes        *int `json:"max_session_bytes,omitempty"`
	SummaryMaxTokens       *int `json:"summary_max_tokens,omitempty"`
	GrepMaxMatches         *int `json:"grep_max_matches,omitempty"`
	ContextTrimToolChars   *int `json:"context_trim_tool_chars,omitempty"`
	// ContextKeepReasoning is the number of most-recent assistant entries retained during proactive reasoning cleanup (P1); <=0/unset = built-in default 1.
	ContextKeepReasoning *int `json:"context_keep_reasoning,omitempty"`
	// ContextKeepToolArgs is the number of most-recent assistant entries retained during proactive tool_call args compaction (P4); <=0/unset = built-in default 2.
	ContextKeepToolArgs *int `json:"context_keep_tool_args,omitempty"`
	// ContextKeepReasoningChars is the per-message Reasoning character cap within the retain window (P7); unset = built-in default 4000,
	// exceeding which triggers head-tail splitting; negative=disabled, positive=custom threshold.
	ContextKeepReasoningChars *int `json:"context_keep_reasoning_chars,omitempty"`
	// PreserveRecentTokens is the retainedTail token budget upper bound (§P1-E); unset/<=0=auto (floor(window/4) clamp [2000,8000]).
	PreserveRecentTokens *int `json:"preserve_recent_tokens,omitempty"`
	// ContextUseRealUsage controls whether the compaction threshold prefers the provider's real usage (§P0-B). nil/unset=enabled (default);
	// false=kill-switch, falling back to pure local estimateTokens. When no real usage is available it falls back automatically, so zero regression.
	ContextUseRealUsage *bool `json:"context_use_real_usage,omitempty"`
	// ToolOutputDir is the root directory for persisting tool output (§P1-A); empty=disabled. When unset, main.go derives it from the
	// session directory (<sessionDir>/<id>.tool-output/). Tools exceeding the limit write full text to disk, and the history Content is
	// replaced with a preview + path hint.
	ToolOutputDir *string `json:"tool_output_dir,omitempty"`
	// ToolOutputRetention is the retention duration for persisted files ("168h"); <=0/unset=7d.
	ToolOutputRetention *string `json:"tool_output_retention,omitempty"`
	// ConfineEvalSymlinks (opt-in) tightens the default-mode file-tool confine check with a final filepath.EvalSymlinks +
	// re-HasPrefix (resolving both target and root, so a workdir reached via a symlink does not false-positive), narrowing the
	// parallel-symlink-swap TOCTOU window. Default false preserves the lexical semantics the maintainer chose. This is guardrail
	// hardening, NOT security — shell stays an unrestricted write primitive (S-1 root cause untouched).
	ConfineEvalSymlinks *bool `json:"confine_eval_symlinks,omitempty"`
	// ConfineAuto (opt-in) wraps the file tools (read/write/edit/grep/glob) with confineWrap in ModeAuto too (shell stays
	// free). Defense-in-depth for long sessions where the deterministic file primitives are worth constraining even when shell is free.
	ConfineAuto *bool `json:"confine_auto,omitempty"`
	// ShellAllowlist (opt-in, default-mode only) restricts shell to commands whose leading token is in this list; every command in a
	// pipeline/list (a | b && c; d) is checked. Exact match only — path-qualified forms (/usr/bin/git) must be listed verbatim, so they
	// cannot be confused with an allowed name. Empty/unset = no check (current behavior). Best-effort lexical guardrail, bypassable via
	// eval/$()/backticks/alias — same framing as the sudo/su denylist, NOT a security boundary (see README "default mode").
	ShellAllowlist []string `json:"shell_allowlist,omitempty"`
	// ShellConfineCd (opt-in, default-mode only) blocks cd/pushd whose target escapes workdir lexically (absolute path, .. that climbs out,
	// ~, $VAR, bare cd→HOME, cd -→previous). Best-effort lexical guardrail, NOT a security boundary — bypassable via subshell/eval; true
	// isolation relies on the caller's OS layer.
	ShellConfineCd *bool `json:"shell_confine_cd,omitempty"`
}

// CompactionConfig configures the summary compaction model for long sessions.
// Provider/Model must be set as a pair: both set allows cross-provider; both empty falls back to the defaults pair; setting only one is an error.
type CompactionConfig struct {
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
	// Auto controls silent usage overflow detection (mirrors opencode compaction.auto, §P1-B). nil=enabled (default); false=disabled,
	// keeping only estimateTokens proactive compaction + ErrContextLength reactive retry.
	Auto *bool `json:"auto,omitempty"`
	// Reserved is the output/growth buffer reserved from the ContextWindow (mirrors opencode compaction.reserved).
	// <=0/unset falls back to min(compactionBuffer=20000, run.max_tokens).
	Reserved *int `json:"reserved,omitempty"`
}

// CLIOverrides / ResolvedRun / Resolved / Resolve / resolveRun are in resolve.go.

// maxConfigFileBytes is the byte cap for the config file, preventing OOM from multi-GB files.
const maxConfigFileBytes = 4 << 20 // 4 MiB

// ReadFileLimited reads path with a size limit; returns an error if it exceeds maxBytes. It opens via miniagent.OpenNoFollow,
// rejecting final-component symlinks (hardened consistently with session files), to prevent the config path (which contains the API key)
// from being symlink-hijacked to a sensitive file.
func ReadFileLimited(path string, maxBytes int64) ([]byte, error) {
	f, err := miniagent.OpenNoFollow(path, os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("file %q exceeds the %d-byte limit", path, maxBytes)
	}
	return data, nil
}

// LoadConfig reads path, deserializes, and validates. Whether an explicitly-passed -config not existing is a hard error is decided by the caller.
func LoadConfig(path string) (*Config, error) {
	data, err := ReadFileLimited(path, maxConfigFileBytes)
	if err != nil {
		return nil, fmt.Errorf("reading config %q: %w", path, err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("config %q is not valid JSON: %w", path, err)
	}
	if err := validateConfig(&cfg); err != nil {
		return nil, fmt.Errorf("config %q: %w", path, err)
	}
	return &cfg, nil
}

// thinkingFieldBlacklist lists the standard payload keys written by buildChatBody (wire.go).
// If miniagent.ThinkingMapping.Field points to any of these keys, payload[field]=val would clobber the standard field
// (e.g. field:"max_tokens" overrides the max_tokens limit, field:"tools" overrides the tool table).
// Note: after pinning, reasoning_effort is no longer a "default redundant field" but a legitimate field explicitly declared by the
// provider (openai standard mapping), so it has been removed from the blacklist — provider.thinking.field:"reasoning_effort" is now valid.
var thinkingFieldBlacklist = map[string]bool{
	"messages":          true,
	"tools":             true,
	"stream":            true,
	"max_tokens":        true,
	"temperature":       true,
	"model":             true,
	"top_p":             true,
	"frequency_penalty": true,
	"presence_penalty":  true,
	"stop":              true,
	"n":                 true,
	"seed":              true,
}

// validateThinking validates the thinking value (pinned): empty string/off is valid; non-off must be declared in customKeys
// (= the main provider.thinking.map keys) — the wire must go through the provider mapping, and standard levels are no longer accepted
// as-is. This forces the provider to explicitly declare the enum mapping (level→value).
func validateThinking(thinking string, customKeys map[string]bool) error {
	if thinking == "" || thinking == miniagent.ThinkingOff {
		return nil
	}
	if customKeys[thinking] {
		return nil
	}
	return fmt.Errorf("thinking %q is not declared in provider.thinking.map (pinned: level must go through the provider mapping; please add the thinking.map for the provider referenced by defaults)", thinking)
}

func validateConfig(cfg *Config) error {
	if len(cfg.Providers) == 0 {
		return errors.New("providers is empty")
	}
	seen := map[string]bool{}
	for i, p := range cfg.Providers {
		if p.Name == "" {
			return fmt.Errorf("providers[%d].name is empty", i)
		}
		if strings.Contains(p.Name, "/") {
			return fmt.Errorf("providers[%d].name %q contains '/', which would cause provider/model parsing ambiguity", i, p.Name)
		}
		if seen[p.Name] {
			return fmt.Errorf("provider name %q is duplicated", p.Name)
		}
		seen[p.Name] = true
		// Kind enum + anthropic-specific constraints: the Messages API mandates max_tokens and has no
		// OpenAI-style /v1/models listing (a static models list is used instead).
		kind := p.Kind
		if kind == "" {
			kind = "openai"
		}
		if kind != "openai" && kind != "anthropic" {
			return fmt.Errorf("provider %q kind %q is invalid (openai|anthropic)", p.Name, kind)
		}
		if kind == "anthropic" {
			if p.MaxTokens == nil || *p.MaxTokens <= 0 {
				return fmt.Errorf("provider %q kind=anthropic requires max_tokens > 0 (Anthropic Messages API mandates it)", p.Name)
			}
			if p.ModelsURL != "" {
				return fmt.Errorf("provider %q kind=anthropic does not support models_url (leave empty; use a static models list)", p.Name)
			}
		}
		if _, err := ValidateURL(p.ChatURL); err != nil {
			return fmt.Errorf("provider %q chat_url: %w", p.Name, err)
		}
		if p.ModelsURL != "" {
			if _, err := ValidateURL(p.ModelsURL); err != nil {
				return fmt.Errorf("provider %q models_url: %w", p.Name, err)
			}
		}
		// thinking.field must not point to buildChatBody's reserved payload keys: buildChatBody writes the thinking level via
		// payload[field]=val (wire.go), and hitting a reserved key would clobber a standard field (e.g. max_tokens/tools). After pinning,
		// thinking.field must be explicitly declared (reasoning_effort etc. are valid and have been removed from the blacklist), but it
		// is still forbidden to point to other standard fields.
		if p.Thinking != nil && p.Thinking.Field != "" && thinkingFieldBlacklist[p.Thinking.Field] {
			return fmt.Errorf("provider %q thinking.field %q is a reserved payload key and would override a standard field", p.Name, p.Thinking.Field)
		}
		// Model-level/provider-level thinking level must be validated against this provider.thinking.map (pinned: level must go through the mapping).
		provCustomKeys := map[string]bool{}
		if p.Thinking != nil {
			for k := range p.Thinking.Map {
				provCustomKeys[k] = true
			}
		}
		// kind=anthropic renders thinking.map VALUES as JSON objects on the wire (resolveThinking unmarshals each value
		// into the top-level thinking field, splitting effort into output_config). A malformed value would otherwise be
		// silently dropped at runtime (resolveThinking returns on Unmarshal failure) — validate at startup so a typo
		// fails loud instead of quietly disabling thinking. The "off" level is never rendered, so its value is exempt.
		if kind == "anthropic" && p.Thinking != nil {
			for level, val := range p.Thinking.Map {
				if level == miniagent.ThinkingOff {
					continue
				}
				var obj map[string]any
				if json.Unmarshal([]byte(val), &obj) != nil {
					return fmt.Errorf("provider %q thinking.map[%q] is not a valid JSON object (kind=anthropic expects e.g. {\"type\":\"adaptive\",\"effort\":\"high\"})", p.Name, level)
				}
				if _, ok := obj["type"]; !ok {
					return fmt.Errorf("provider %q thinking.map[%q] object is missing the \"type\" key (kind=anthropic)", p.Name, level)
				}
			}
		}
		// Models: Name must be non-empty and unique within the provider.
		modelNames := map[string]bool{}
		for j, mc := range p.Models {
			if mc.Name == "" {
				return fmt.Errorf("providers[%d].models[%d].name is empty", i, j)
			}
			if modelNames[mc.Name] {
				return fmt.Errorf("provider %q models name %q is duplicated", p.Name, mc.Name)
			}
			modelNames[mc.Name] = true
			if mc.Thinking != nil {
				if err := validateThinking(*mc.Thinking, provCustomKeys); err != nil {
					return fmt.Errorf("provider %q model %q thinking: %w", p.Name, mc.Name, err)
				}
			}
			// kind=anthropic mandates a positive max_tokens (the Messages API rejects max_tokens<=0 with 400). The
			// provider-level check above only covers p.MaxTokens, but pickMPG prefers a non-nil model-level value, so a
			// model-level max_tokens<=0 would override the valid provider value and 400 every call. nil is allowed (= inherit provider).
			if kind == "anthropic" && mc.MaxTokens != nil && *mc.MaxTokens <= 0 {
				return fmt.Errorf("provider %q model %q max_tokens must be > 0 (kind=anthropic mandates a positive max_tokens; nil inherits the provider value)", p.Name, mc.Name)
			}
		}
		if p.ThinkingLevel != nil {
			if err := validateThinking(*p.ThinkingLevel, provCustomKeys); err != nil {
				return fmt.Errorf("provider %q thinking_level: %w", p.Name, err)
			}
		}
	}
	defProv, defModel, err := resolveProviderModel(cfg, "defaults", cfg.Defaults.Provider, cfg.Defaults.Model)
	if err != nil {
		return err
	}
	if cfg.Defaults.Mode != "" && cfg.Defaults.Mode != miniagent.ModeDefault && cfg.Defaults.Mode != miniagent.ModeAuto {
		return fmt.Errorf("defaults.mode %q is invalid (%s|%s)", cfg.Defaults.Mode, miniagent.ModeDefault, miniagent.ModeAuto)
	}
	// Pinned: defaults.thinking≠off → the main provider must declare thinking{field≠"", map non-empty}; the wire must go through the provider mapping.
	if cfg.Defaults.Thinking != "" && cfg.Defaults.Thinking != miniagent.ThinkingOff {
		if defProv.Thinking == nil {
			return fmt.Errorf("defaults.thinking %q is enabled, but provider %q does not declare thinking (pinned: enabling thinking requires declaring {field,map})", cfg.Defaults.Thinking, defProv.Name)
		}
		if defProv.Thinking.Field == "" {
			return fmt.Errorf("provider %q thinking.field is empty (pinned: field is required)", defProv.Name)
		}
		if len(defProv.Thinking.Map) == 0 {
			return fmt.Errorf("provider %q thinking.map is empty (pinned: map is required, enums must go through the mapping)", defProv.Name)
		}
	}
	// Validate using the thinking.map keys of the provider referenced by defaults (defProv) (per-provider, consistent with Resolve).
	defCustomKeys := map[string]bool{}
	if defProv.Thinking != nil {
		for k := range defProv.Thinking.Map {
			defCustomKeys[k] = true
		}
	}
	if err := validateThinking(cfg.Defaults.Thinking, defCustomKeys); err != nil {
		return fmt.Errorf("defaults.thinking: %w", err)
	}
	if _, _, err := resolveOptionalPair(cfg, "compaction", cfg.Compaction.Provider, cfg.Compaction.Model, defProv, defModel); err != nil {
		return err
	}
	return nil
}

// FindProvider / resolveProviderModel / resolveOptionalPair are in resolve.go.

// Resolve / resolveRun / pickInt / pickDur are in resolve.go.
// ListModels / ListAllModels are in models.go.
