package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/justphantom/miniagent/internal/miniagent"
	"github.com/justphantom/miniagent/internal/text"
)

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
		// Kind enum + anthropic-specific constraint: the Messages API mandates max_tokens. (models_url is
		// allowed for kind=anthropic: the upstream /v1/models endpoint works with the same auth headers.)
		kind := p.Kind
		if kind == "" {
			kind = "openai"
		}
		if kind != "openai" && kind != "anthropic" {
			return fmt.Errorf("provider %q kind %q is invalid (openai|anthropic)", p.Name, kind)
		}
		if kind == "anthropic" && (p.MaxTokens == nil || *p.MaxTokens <= 0) {
			return fmt.Errorf("provider %q kind=anthropic requires max_tokens > 0 (Anthropic Messages API mandates it)", p.Name)
		}
		if _, err := text.ValidateURL(p.ChatURL); err != nil {
			return fmt.Errorf("provider %q chat_url: %w", p.Name, err)
		}
		if p.ModelsURL != "" {
			if _, err := text.ValidateURL(p.ModelsURL); err != nil {
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
