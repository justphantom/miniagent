package miniagent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// ThinkingMapping 让 provider 显式声明思考字段的 wire 名（默认 reasoning_effort）
// 与级别取值映射（跨供应商兼容）。
type ThinkingMapping struct {
	Field string            `json:"field"`
	Map   map[string]string `json:"map,omitempty"`
}

// Config 是 ./miniagent.json 的根结构。Defaults=策略（model/thinking/mode/system），
// Run=运行参数（限额/超时/路径/stream）；指针区分未设置/零值（审查 v2 #12）。
type Config struct {
	Session    SessionConfig    `json:"session"`
	Providers  []ProviderConfig `json:"providers"`
	Defaults   DefaultsConfig   `json:"defaults"`
	Run        RunConfig        `json:"run"`
	Compaction CompactionConfig `json:"compaction"`
	Memory     MemoryConfig     `json:"memory"`
}

type SessionConfig struct {
	Dir string `json:"dir"`
}

type ProviderConfig struct {
	Name      string           `json:"name"`
	ChatURL   string           `json:"chat_url"`
	ModelsURL string           `json:"models_url,omitempty"`
	Key       string           `json:"key,omitempty"`
	Models    []string         `json:"models,omitempty"`
	Thinking  *ThinkingMapping `json:"thinking,omitempty"`
}

type DefaultsConfig struct {
	Model            string `json:"model,omitempty"`
	Thinking         string `json:"thinking,omitempty"`
	Mode             string `json:"mode,omitempty"`
	SystemPrompt     string `json:"system_prompt,omitempty"`
	SummaryRequest   string `json:"summary_request,omitempty"`
	SummarizerPrompt string `json:"summarizer_prompt,omitempty"`
}

// RunConfig 的 duration 字段用 *string（"30s"），Resolve 解析为 time.Duration，
// 配置文件可读且无需自定义 JSON 类型。
type RunConfig struct {
	Workdir        *string `json:"workdir,omitempty"`
	MaxTokens      *int    `json:"max_tokens,omitempty"`
	MaxIterations  *int    `json:"max_iterations,omitempty"`
	MaxTotalTokens *int    `json:"max_tokens_total,omitempty"`
	ContextWindow  *int    `json:"context_window,omitempty"`
	MaxDuration    *string `json:"max_duration,omitempty"`
	ShellTimeout   *string `json:"shell_timeout,omitempty"`
	FileOpTimeout  *string `json:"file_op_timeout,omitempty"`
	WriteTimeout   *string `json:"write_timeout,omitempty"`
	Stream         *bool   `json:"stream,omitempty"`
	// S4 策略化常量（<=0 或缺省=内置默认）。
	MaxToolResultChars   *int    `json:"max_tool_result_chars,omitempty"`
	MaxFileResultChars   *int    `json:"max_file_result_chars,omitempty"`
	MaxParallelTools     *int    `json:"max_parallel_tools,omitempty"`
	ContextKeepRecent    *int    `json:"context_keep_recent,omitempty"`
	SummaryMaxChars      *int    `json:"summary_max_chars,omitempty"`
	HTTPTimeout          *string `json:"http_timeout,omitempty"`
	MaxReadFileBytes     *int    `json:"max_read_file_bytes,omitempty"`
	MaxShellOutputChars  *int    `json:"max_shell_output_chars,omitempty"`
	MaxSessionBytes      *int    `json:"max_session_bytes,omitempty"`
	SummaryMaxTokens     *int    `json:"summary_max_tokens,omitempty"`
	GrepMaxMatches       *int    `json:"grep_max_matches,omitempty"`
	MemoryRecentN        *int    `json:"memory_recent_n,omitempty"`
	ContextTrimToolChars *int    `json:"context_trim_tool_chars,omitempty"`
}

// CompactionConfig 配置长会话摘要压缩模型。
// model 可以是 model id（与主模型同 provider），也可以是 provider/model（跨 provider）。
type CompactionConfig struct {
	Model string `json:"model,omitempty"`
}

// MemoryConfig 配置会话结束后的自动记忆抽取。
// model 缺省回落链：memory.model → defaults.model → 主会话模型（同 compaction）。
// 不带 '/' 表示与主会话同 provider，只换 model id；带 '/' 走 provider/model（可跨 provider）。
// auto_update 缺省（nil）= true；max_per_session 缺省/<=0 = 3。
type MemoryConfig struct {
	Model         string `json:"model,omitempty"`
	AutoUpdate    *bool  `json:"auto_update,omitempty"`
	MaxPerSession *int   `json:"max_per_session,omitempty"`
	ExtractPrompt string `json:"extract_prompt,omitempty"`
}

// CLIOverrides / ResolvedRun / Resolved / Resolve / resolveRun 见 resolve.go。

// maxConfigFileBytes 是配置/规则文件的字节上限，防止多 GB 文件导致 OOM。
const maxConfigFileBytes = 4 << 20 // 4 MiB

// ReadFileLimited 读取 path 并限制大小；超过 maxBytes 返回错误。
func ReadFileLimited(path string, maxBytes int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("文件 %q 超过 %d 字节上限", path, maxBytes)
	}
	return data, nil
}

// LoadConfig 读 path、反序列化、校验。显式 -config 不存在由调用方区分硬错误。
func LoadConfig(path string) (*Config, error) {
	data, err := ReadFileLimited(path, maxConfigFileBytes)
	if err != nil {
		return nil, fmt.Errorf("读 config %q: %w", path, err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("config %q 不是合法 JSON: %w", path, err)
	}
	if err := validateConfig(&cfg); err != nil {
		return nil, fmt.Errorf("config %q: %w", path, err)
	}
	return &cfg, nil
}

// thinkingFieldBlacklist 列出 buildChatBody 写入的标准 payload key（wire.go）。
// ThinkingMapping.Field 若指向这些 key，payload[field]=val 会 clobber 标准字段
// （如 field:"max_tokens" 覆盖 max_tokens 限额、field:"tools" 覆盖工具表）；
// reasoning_effort 是默认 field（wire.go），无需显式 mapping，显式写视同误配
// （审查 v3 P3）。
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
	"reasoning_effort":  true,
	"stop":              true,
	"n":                 true,
	"seed":              true,
}

// standardThinkingLevels 是 CLI/config 可直接使用的思考级别（空串/off 均表示关闭）。
var standardThinkingLevels = map[string]bool{
	ThinkingOff: true,
	"minimal":   true,
	"low":       true,
	"medium":    true,
	"high":      true,
	"xhigh":     true,
	"max":       true,
}

// thinkingCustomKeys 聚合所有 provider 的 thinking.map 自定义 key，供 validateConfig 校验。
func thinkingCustomKeys(cfg *Config) map[string]bool {
	custom := map[string]bool{}
	for _, p := range cfg.Providers {
		if p.Thinking == nil {
			continue
		}
		for k := range p.Thinking.Map {
			custom[k] = true
		}
	}
	return custom
}

// validateThinking 校验 thinking 取值：标准级别、customKeys 中的自定义 key，或空串/off 均合法。
func validateThinking(thinking string, customKeys map[string]bool) error {
	if thinking == "" || thinking == ThinkingOff {
		return nil
	}
	if standardThinkingLevels[thinking] {
		return nil
	}
	if customKeys[thinking] {
		return nil
	}
	return fmt.Errorf("thinking %q 非法（支持：%s 或 provider.thinking.map 自定义键）", thinking, strings.Join([]string{ThinkingOff, "minimal", "low", "medium", "high", "xhigh", "max"}, ", "))
}

func validateConfig(cfg *Config) error {
	if len(cfg.Providers) == 0 {
		return errors.New("providers 为空")
	}
	seen := map[string]bool{}
	for i, p := range cfg.Providers {
		if p.Name == "" {
			return fmt.Errorf("providers[%d].name 为空", i)
		}
		if strings.Contains(p.Name, "/") {
			return fmt.Errorf("providers[%d].name %q 含 '/'，会导致 provider/model 解析歧义", i, p.Name)
		}
		if seen[p.Name] {
			return fmt.Errorf("provider 名 %q 重复", p.Name)
		}
		seen[p.Name] = true
		if _, err := validateURL(p.ChatURL); err != nil {
			return fmt.Errorf("provider %q chat_url: %w", p.Name, err)
		}
		if p.ModelsURL != "" {
			if _, err := validateURL(p.ModelsURL); err != nil {
				return fmt.Errorf("provider %q models_url: %w", p.Name, err)
			}
		}
		// thinking.field 不得指向 buildChatBody 的保留 payload key：buildChatBody 用
		// payload[field]=val 写思考级别（wire.go），命中保留 key 会 clobber 标准字段
		// （如 max_tokens/tools）。reasoning_effort 是默认 field，无需显式 mapping
		// （审查 v3 P3）。
		if p.Thinking != nil && p.Thinking.Field != "" && thinkingFieldBlacklist[p.Thinking.Field] {
			return fmt.Errorf("provider %q thinking.field %q 是保留 payload key，会覆盖标准字段", p.Name, p.Thinking.Field)
		}
	}
	if cfg.Defaults.Model != "" {
		if _, _, err := ParseModelSpec(cfg.Defaults.Model, cfg); err != nil {
			return fmt.Errorf("defaults.model: %w", err)
		}
	}
	if cfg.Defaults.Mode != "" && cfg.Defaults.Mode != "default" && cfg.Defaults.Mode != "auto" {
		return fmt.Errorf("defaults.mode %q 非法（default|auto）", cfg.Defaults.Mode)
	}
	if err := validateThinking(cfg.Defaults.Thinking, thinkingCustomKeys(cfg)); err != nil {
		return fmt.Errorf("defaults.thinking: %w", err)
	}
	if cfg.Compaction.Model != "" && strings.Contains(cfg.Compaction.Model, "/") {
		if _, _, err := ParseModelSpec(cfg.Compaction.Model, cfg); err != nil {
			return fmt.Errorf("compaction.model: %w", err)
		}
	}
	if cfg.Memory.Model != "" && strings.Contains(cfg.Memory.Model, "/") {
		if _, _, err := ParseModelSpec(cfg.Memory.Model, cfg); err != nil {
			return fmt.Errorf("memory.model: %w", err)
		}
	}
	return nil
}

// ParseModelSpec 拆分 "provider/model"。无 / 时仅当 provider 恰好 1 个才回落到它；
// 否则要求显式前缀。cfg 必须非 nil（S1 删裸模式后始终有 config）。
func ParseModelSpec(spec string, cfg *Config) (ProviderConfig, string, error) {
	if spec == "" {
		return ProviderConfig{}, "", errors.New("model spec 为空")
	}
	if cfg == nil {
		return ProviderConfig{}, "", errors.New("ParseModelSpec: cfg 为 nil（S1 后 config 必须存在）")
	}
	name, modelID, hasSlash := strings.Cut(spec, "/")
	if !hasSlash {
		if len(cfg.Providers) != 1 {
			return ProviderConfig{}, "", fmt.Errorf("model %q 缺 provider 前缀，且 provider 数非 1（用 provider/model）", spec)
		}
		return cfg.Providers[0], spec, nil
	}
	for _, p := range cfg.Providers {
		if p.Name == name {
			return p, modelID, nil
		}
	}
	return ProviderConfig{}, "", fmt.Errorf("未知 provider %q", name)
}

// Resolve / resolveRun / pickInt / pickDur 见 resolve.go。
// ListModels / ListAllModels 见 models.go。
