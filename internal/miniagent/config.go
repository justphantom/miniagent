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
	// Headers 是每 provider 的自定义请求头，随请求注入；不覆盖 Authorization / Content-Type。
	Headers map[string]string `json:"headers,omitempty"`
}

// ModelRef 是一个可用的 provider/model 组合（ListAllModels 的返回单元）。
// provider 与 model 分离，消费方无需从 "provider/model_id" 文本拆分
// （model id 本身可含 '/'，文本拆分有歧义）。
type ModelRef struct {
	Provider, Model string
}

// DefaultsConfig 的 Provider/Model 均必填（provider/model 拆分后不再解析 "provider/id" 串）。
type DefaultsConfig struct {
	Provider         string `json:"provider"`
	Model            string `json:"model"`
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
	MaxToolResultChars  *int    `json:"max_tool_result_chars,omitempty"`
	MaxFileResultChars  *int    `json:"max_file_result_chars,omitempty"`
	MaxParallelTools    *int    `json:"max_parallel_tools,omitempty"`
	ContextKeepRecent   *int    `json:"context_keep_recent,omitempty"`
	SummaryMaxChars     *int    `json:"summary_max_chars,omitempty"`
	HTTPTimeout         *string `json:"http_timeout,omitempty"`
	MaxReadFileBytes    *int    `json:"max_read_file_bytes,omitempty"`
	MaxShellOutputChars *int    `json:"max_shell_output_chars,omitempty"`
	// ShellStreamWindowBytes 是 shell/script 输出滑窗字节上限（保尾部，§P1-D）；缺省/<=0 用默认 2*max_shell_output_chars*4。
	ShellStreamWindowBytes *int `json:"shell_stream_window_bytes,omitempty"`
	MaxSessionBytes        *int `json:"max_session_bytes,omitempty"`
	SummaryMaxTokens       *int `json:"summary_max_tokens,omitempty"`
	GrepMaxMatches         *int `json:"grep_max_matches,omitempty"`
	ContextTrimToolChars   *int `json:"context_trim_tool_chars,omitempty"`
	// ContextKeepReasoning 是主动 reasoning 清理保留的最近 assistant 条数（P1）；<=0/缺省=内置默认 1。
	ContextKeepReasoning *int `json:"context_keep_reasoning,omitempty"`
	// ContextKeepToolArgs 是主动 tool_call args 压缩保留的最近 assistant 条数（P4）；<=0/缺省=内置默认 2。
	ContextKeepToolArgs *int `json:"context_keep_tool_args,omitempty"`
	// ContextKeepReasoningChars 是保留窗口内单条 Reasoning 字符上限（P7）；缺省=内置默认 4000，超过则头尾
	// 分段；负数=关闭，正数=自定义阈值。
	ContextKeepReasoningChars *int `json:"context_keep_reasoning_chars,omitempty"`
	// PreserveRecentTokens 是 retainedTail token 预算上界（§P1-E）；缺省/<=0=自动（floor(window/4) clamp [2000,8000]）。
	PreserveRecentTokens *int `json:"preserve_recent_tokens,omitempty"`
	// ContextUseRealUsage 控制压缩阈值是否优先采纳 provider 真实 usage（§P0-B）。nil/缺省=启用（默认）；
	// false=kill-switch 回落纯本地 estimateTokens。无可用真实 usage 时自动回落，故零回归。
	ContextUseRealUsage *bool `json:"context_use_real_usage,omitempty"`
	// ToolOutputDir 是工具输出落盘根目录（§P1-A）；空=禁用。未设时 main.go 按 session 目录自动派生
	// （<sessionDir>/<id>.tool-output/）。超 limit 的工具全文写盘、历史 Content 改为预览+路径提示。
	ToolOutputDir *string `json:"tool_output_dir,omitempty"`
	// ToolOutputRetention 是落盘文件保留时长（"168h"）；<=0/缺省=7d。
	ToolOutputRetention *string `json:"tool_output_retention,omitempty"`
}

// CompactionConfig 配置长会话摘要压缩模型。
// Provider/Model 须成对设置：同设可跨 provider；同空则整体回落 defaults 对；只设其一报错。
type CompactionConfig struct {
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
	// Auto 控制静默用量溢出检测（对标 opencode compaction.auto，§P1-B）。nil=启用（默认）；false=关闭，
	// 仅保留 estimateTokens 主动压缩 + ErrContextLength 反应重试。
	Auto *bool `json:"auto,omitempty"`
	// Reserved 是从 ContextWindow 预留的输出/增长缓冲（对标 opencode compaction.reserved）。
	// <=0/缺省回落 min(compactionBuffer=20000, run.max_tokens)。
	Reserved *int `json:"reserved,omitempty"`
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
		if _, err := ValidateURL(p.ChatURL); err != nil {
			return fmt.Errorf("provider %q chat_url: %w", p.Name, err)
		}
		if p.ModelsURL != "" {
			if _, err := ValidateURL(p.ModelsURL); err != nil {
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
	defProv, defModel, err := resolveProviderModel(cfg, "defaults", cfg.Defaults.Provider, cfg.Defaults.Model)
	if err != nil {
		return err
	}
	if cfg.Defaults.Mode != "" && cfg.Defaults.Mode != "default" && cfg.Defaults.Mode != "auto" {
		return fmt.Errorf("defaults.mode %q 非法（default|auto）", cfg.Defaults.Mode)
	}
	if err := validateThinking(cfg.Defaults.Thinking, thinkingCustomKeys(cfg)); err != nil {
		return fmt.Errorf("defaults.thinking: %w", err)
	}
	if _, _, err := resolveOptionalPair(cfg, "compaction", cfg.Compaction.Provider, cfg.Compaction.Model, defProv, defModel); err != nil {
		return err
	}
	return nil
}

// FindProvider / resolveProviderModel / resolveOptionalPair 见 resolve.go。

// Resolve / resolveRun / pickInt / pickDur 见 resolve.go。
// ListModels / ListAllModels 见 models.go。
