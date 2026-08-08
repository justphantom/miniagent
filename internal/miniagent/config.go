package miniagent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// ThinkingMapping 让 provider 显式声明思考字段的 wire 名（如 openai 的 reasoning_effort）
// 与级别取值映射（跨供应商兼容）。钉死：启用思考时 provider 必声明 {field,map}，wire 必经映射。
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
	Name      string `json:"name"`
	ChatURL   string `json:"chat_url"`
	ModelsURL string `json:"models_url,omitempty"`
	Key       string `json:"key,omitempty"`
	// Models 是该 provider 支持的模型列表，每项可承载模型级参数覆盖供应商级与全局。
	// breaking：原为 []string，现须对象形式 [{"name":...}]。
	Models []ModelConfig `json:"models,omitempty"`
	// Thinking 是思考字段的 wire 映射（{field,map}），供应商级——启用思考时 provider 必声明。
	Thinking *ThinkingMapping `json:"thinking,omitempty"`
	// 供应商级模型参数（覆盖全局 run）：输出上限、上下文窗口、HTTP 超时（传输层，同 provider 共享 endpoint）。
	MaxTokens     *int    `json:"max_tokens,omitempty"`
	ContextWindow *int    `json:"context_window,omitempty"`
	HTTPTimeout   *string `json:"http_timeout,omitempty"`
	// ThinkingLevel 是供应商级思考级别（level 字符串），覆盖 defaults.thinking。json key 用
	// thinking_level 避免与 Thinking（wire 映射，key "thinking"）冲突。
	ThinkingLevel *string `json:"thinking_level,omitempty"`
	// Headers 是每 provider 的自定义请求头，随请求注入；不覆盖 Authorization / Content-Type。
	Headers map[string]string `json:"headers,omitempty"`
}

// ModelConfig 是 provider 下单个模型的配置：Name 必填，其余为模型级参数，覆盖供应商级与全局。
// max_tokens/context_window 走 model>provider>global；thinking level 走 cli>model>provider>global；
// http_timeout 不进模型级（传输层属性）。
type ModelConfig struct {
	Name          string  `json:"name"`
	MaxTokens     *int    `json:"max_tokens,omitempty"`
	ContextWindow *int    `json:"context_window,omitempty"`
	Thinking      *string `json:"thinking,omitempty"` // 模型级 level（model 无 wire 映射，key "thinking" 即 level）
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
	// SubagentGuidance 是 subagent fork 引导模板，注入 system prompt 末尾；占位符 {config_path}/{mode}
	//（config_path 经 shellSingleQuote 包裹）。空用内置默认。
	SubagentGuidance string `json:"subagent_guidance,omitempty"`
	// SummaryCreateInstruction/SummaryUpdateInstruction 是 compaction 摘要指令模板（CREATE/UPDATE 模式），
	// 占位符 {max_chars}。空用内置默认。SummaryTemplate 是固定摘要结构模板（无变量），空用内置默认。
	SummaryCreateInstruction string `json:"summary_create_instruction,omitempty"`
	SummaryUpdateInstruction string `json:"summary_update_instruction,omitempty"`
	SummaryTemplate          string `json:"summary_template,omitempty"`
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
	// ShellStreamWindowBytes 是 shell/script 输出滑窗字节上限（保尾部，§P1-D）；缺省/<=0 用默认 max_shell_output_chars*8。
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

// ReadFileLimited 读取 path 并限制大小；超过 maxBytes 返回错误。经 openNoFollow 打开，拒最终分量
// 符号链接（与 session 文件一致硬化），防 config 路径（含 API key）被 symlink 劫持到敏感文件。
func ReadFileLimited(path string, maxBytes int64) ([]byte, error) {
	f, err := openNoFollow(path, os.O_RDONLY, 0)
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
// （如 field:"max_tokens" 覆盖 max_tokens 限额、field:"tools" 覆盖工具表）。
// 注：钉死后 reasoning_effort 不再是「默认冗余 field」，而是 provider 显式声明的合法 field（openai 标准映射），
// 故已从黑名单移除——provider.thinking.field:"reasoning_effort" 现合法。
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

// validateThinking 校验 thinking 取值（钉死）：空串/off 合法；非 off 必须在 customKeys（= 主 provider.thinking.map
// keys）中声明——wire 必经 provider 映射，不再原样接受标准级别。迫使 provider 显式声明枚举映射（level→value）。
func validateThinking(thinking string, customKeys map[string]bool) error {
	if thinking == "" || thinking == ThinkingOff {
		return nil
	}
	if customKeys[thinking] {
		return nil
	}
	return fmt.Errorf("thinking %q 未在 provider.thinking.map 中声明（钉死：level 必经 provider 映射，请补 defaults 所指 provider 的 thinking.map）", thinking)
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
		// （如 max_tokens/tools）。钉死后 thinking.field 必显式声明（reasoning_effort 等合法，
		// 已从黑名单移除），但仍禁指向其他标准字段。
		if p.Thinking != nil && p.Thinking.Field != "" && thinkingFieldBlacklist[p.Thinking.Field] {
			return fmt.Errorf("provider %q thinking.field %q 是保留 payload key，会覆盖标准字段", p.Name, p.Thinking.Field)
		}
		// 模型级/供应商级 thinking level 须经本 provider.thinking.map 校验（钉死：level 必经映射）。
		provCustomKeys := map[string]bool{}
		if p.Thinking != nil {
			for k := range p.Thinking.Map {
				provCustomKeys[k] = true
			}
		}
		// Models：Name 非空、provider 内唯一。
		modelNames := map[string]bool{}
		for j, mc := range p.Models {
			if mc.Name == "" {
				return fmt.Errorf("providers[%d].models[%d].name 为空", i, j)
			}
			if modelNames[mc.Name] {
				return fmt.Errorf("provider %q models 名 %q 重复", p.Name, mc.Name)
			}
			modelNames[mc.Name] = true
			if mc.Thinking != nil {
				if err := validateThinking(*mc.Thinking, provCustomKeys); err != nil {
					return fmt.Errorf("provider %q model %q thinking: %w", p.Name, mc.Name, err)
				}
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
	if cfg.Defaults.Mode != "" && cfg.Defaults.Mode != ModeDefault && cfg.Defaults.Mode != ModeAuto {
		return fmt.Errorf("defaults.mode %q 非法（%s|%s）", cfg.Defaults.Mode, ModeDefault, ModeAuto)
	}
	// 钉死：defaults.thinking≠off → 主 provider 必声明 thinking{field≠"", map 非空}，wire 必经 provider 映射。
	if cfg.Defaults.Thinking != "" && cfg.Defaults.Thinking != ThinkingOff {
		if defProv.Thinking == nil {
			return fmt.Errorf("defaults.thinking %q 已启用，但 provider %q 未声明 thinking（钉死：启用思考必须声明 {field,map}）", cfg.Defaults.Thinking, defProv.Name)
		}
		if defProv.Thinking.Field == "" {
			return fmt.Errorf("provider %q thinking.field 为空（钉死：field 必填）", defProv.Name)
		}
		if len(defProv.Thinking.Map) == 0 {
			return fmt.Errorf("provider %q thinking.map 为空（钉死：map 必填，枚举必经映射）", defProv.Name)
		}
	}
	// 用 defaults 所指 provider（defProv）的 thinking.map keys 校验（per-provider，与 Resolve 一致）。
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

// FindProvider / resolveProviderModel / resolveOptionalPair 见 resolve.go。

// Resolve / resolveRun / pickInt / pickDur 见 resolve.go。
// ListModels / ListAllModels 见 models.go。
