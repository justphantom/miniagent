package miniagent

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
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
	Stream         *bool   `json:"stream,omitempty"`
	// S4 策略化常量（<=0 或缺省=内置默认，见 loop/context/tools 使用点）。
	MaxToolResultChars *int `json:"max_tool_result_chars,omitempty"`
	MaxFileResultChars *int `json:"max_file_result_chars,omitempty"`
	MaxParallelTools   *int `json:"max_parallel_tools,omitempty"`
	ContextKeepRecent  *int `json:"context_keep_recent,omitempty"`
	SummaryMaxChars    *int `json:"summary_max_chars,omitempty"`
}

// CompactionModel 仅 model id（同 provider，不得含 /）。
type CompactionConfig struct {
	Model string `json:"model,omitempty"`
}

// CLIOverrides / ResolvedRun / Resolved / Resolve / resolveRun 见 resolve.go。

var envVarRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// expandVars 把 raw 中所有 ${VAR} 替换为 os.Getenv(VAR)。未设置/空报错；值含 JSON
// 特殊字符（" \）或控制字符（<0x20，含换行/制表/回车，RFC 8259 要求转义）拒绝内联，
// 防止破坏 JSON 字符串结构（裸内联会让 json.Unmarshal 报「非法 JSON」，掩盖根因）。
func expandVars(raw string) (string, error) {
	var bad error
	out := envVarRe.ReplaceAllStringFunc(raw, func(m string) string {
		if bad != nil {
			return m
		}
		name := m[2 : len(m)-1]
		v, ok := os.LookupEnv(name)
		if !ok || v == "" {
			bad = fmt.Errorf("环境变量 %q 未设置或为空", name)
			return m
		}
		if strings.ContainsAny(v, `"\`) || hasControlChar(v) {
			bad = fmt.Errorf("环境变量 %q 含 JSON 特殊字符或控制字符，拒绝内联", name)
			return m
		}
		return v
	})
	return out, bad
}

// hasControlChar 报告 s 是否含 JSON 控制字符（U+0000–U+001F，须转义）或 DEL(0x7f)。
func hasControlChar(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

// LoadConfig 读 path、展开 ${VAR}、反序列化、校验。显式 -config 不存在由调用方区分硬错误。
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读 config %q: %w", path, err)
	}
	expanded, err := expandVars(string(data))
	if err != nil {
		return nil, fmt.Errorf("config %q: %w", path, err)
	}
	var cfg Config
	if err := json.Unmarshal([]byte(expanded), &cfg); err != nil {
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

func validateConfig(cfg *Config) error {
	if len(cfg.Providers) == 0 {
		return errors.New("providers 为空")
	}
	seen := map[string]bool{}
	for i, p := range cfg.Providers {
		if p.Name == "" {
			return fmt.Errorf("providers[%d].name 为空", i)
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
		// key 明文入 config 与「机密不入文件」的承诺相悖；不强制拒绝（兼容现有
		// 用法），仅 stderr 告警引导用 ${VAR} 注入（审查 P3-11）。
		if p.Key != "" && !envVarRe.MatchString(p.Key) {
			fmt.Fprintf(os.Stderr, "miniagent: warning: provider %q 的 key 明文写入 config，建议用 ${VAR} 注入避免机密入文件\n", p.Name)
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
	if cfg.Compaction.Model != "" && strings.Contains(cfg.Compaction.Model, "/") {
		return errors.New("compaction.model 不得含 provider 前缀 '/'（同 provider）")
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
// ListAvailableModels 见 models.go。
