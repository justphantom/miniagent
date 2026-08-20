package config

import (
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
	Kind      string `json:"kind,omitempty"` // wire format: "" / "openai" (Chat Completions, default)
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
	// Cache is a v5.0.0 removed key (provider.cache): kept as a json field so DisallowUnknownFields
	// does not reject old configs. The value is silently ignored.
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

// ModelLimits carries the capability limits a models endpoint reports for one model:
// ContextWindow/MaxOutputTokens are non-standard extensions to the OpenAI /v1/models schema
// (fields like context_window/max_output_tokens offered by some gateways), absent on official
// endpoints — hence pointers: nil = endpoint did not report it.
type ModelLimits struct {
	ContextWindow   *int
	MaxOutputTokens *int
}

// DefaultsConfig's Provider/Model are both required (after provider/model split, the "provider/id" string is no longer parsed).
type DefaultsConfig struct {
	Provider     string `json:"provider"`
	Model        string `json:"model"`
	Thinking     string `json:"thinking,omitempty"`
	SystemPrompt string `json:"system_prompt,omitempty"`
	// RulesFile is a bare filename under workdir (e.g. "AGENTS.md"); when the file exists its text is appended to the
	// system prompt after the base (built-in default or system_prompt), before subagent guidance. Empty = no file source
	// (config-only, the default). Must be a bare filename (no path separators) — resolved strictly under workdir.
	RulesFile        string `json:"rules_file,omitempty"`
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
	// Mode is a v5.0.0 removed key (defaults.mode): kept as a json field so DisallowUnknownFields
	// does not reject old configs. The value is silently ignored — miniagent has no mode since 5.0.0.
	Mode string `json:"mode,omitempty"`
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
	// ConfineEvalSymlinks / ConfineAuto are v5.0.0 removed keys (run.confine_eval_symlinks / run.confine_auto):
	// kept as json fields so DisallowUnknownFields does not reject old configs. Values are silently ignored.
	ConfineEvalSymlinks *bool `json:"confine_eval_symlinks,omitempty"`
	ConfineAuto         *bool `json:"confine_auto,omitempty"`
	// ToolOutputDir is the root directory for persisting tool output (§P1-A); empty=disabled. When unset, main.go derives it from the
	// session directory (<sessionDir>/<id>.tool-output/). Tools exceeding the limit write full text to disk, and the history Content is
	// replaced with a preview + path hint.
	ToolOutputDir *string `json:"tool_output_dir,omitempty"`
	// ToolOutputRetention is the retention duration for persisted files ("168h"); <=0/unset=7d.
	ToolOutputRetention *string `json:"tool_output_retention,omitempty"`
	// WebTimeout is the per-fetch overall timeout ("30s"); unset uses the built-in default (30s).
	WebTimeout *string `json:"web_timeout,omitempty"`
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
