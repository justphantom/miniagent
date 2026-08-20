package config

import (
	"errors"
	"fmt"
	"strings"

	"github.com/justphantom/miniagent/internal/miniagent"
	"github.com/justphantom/miniagent/internal/text"
)

// thinkingFieldBlacklist lists the standard payload keys written by buildChatBody (wire.go).
// If miniagent.ThinkingMapping.Field points to any of these keys, payload[field]=val would clobber the standard field.
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

// validateThinking validates the thinking value: empty string/off is valid; non-off must be declared in customKeys.
func validateThinking(thinking string, customKeys map[string]bool) error {
	if thinking == "" || thinking == miniagent.ThinkingOff {
		return nil
	}
	if customKeys[thinking] {
		return nil
	}
	return fmt.Errorf("thinking %q is not declared in provider.thinking.map (pinned: level must go through the provider mapping)", thinking)
}

// validateConfig validates the config structure after JSON deserialization.
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
			return fmt.Errorf("providers[%d].name %q contains '/'", i, p.Name)
		}
		if seen[p.Name] {
			return fmt.Errorf("provider name %q is duplicated", p.Name)
		}
		seen[p.Name] = true
		kind := p.Kind
		if kind == "" {
			kind = "openai"
		}
		if kind != "openai" {
			return fmt.Errorf("provider %q kind %q is invalid (only openai supported)", p.Name, kind)
		}
		if _, err := text.ValidateURL(p.ChatURL); err != nil {
			return fmt.Errorf("provider %q chat_url: %w", p.Name, err)
		}
		if p.ModelsURL != "" {
			if _, err := text.ValidateURL(p.ModelsURL); err != nil {
				return fmt.Errorf("provider %q models_url: %w", p.Name, err)
			}
		}
		if p.Thinking != nil && p.Thinking.Field != "" && thinkingFieldBlacklist[p.Thinking.Field] {
			return fmt.Errorf("provider %q thinking.field %q is a reserved payload key", p.Name, p.Thinking.Field)
		}
		provCustomKeys := map[string]bool{}
		if p.Thinking != nil {
			for k := range p.Thinking.Map {
				provCustomKeys[k] = true
			}
		}
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
	if cfg.Defaults.Thinking != "" && cfg.Defaults.Thinking != miniagent.ThinkingOff {
		if defProv.Thinking == nil {
			return fmt.Errorf("defaults.thinking %q is enabled, but provider %q does not declare thinking", cfg.Defaults.Thinking, defProv.Name)
		}
		if defProv.Thinking.Field == "" {
			return fmt.Errorf("provider %q thinking.field is empty (pinned: field is required)", defProv.Name)
		}
		if len(defProv.Thinking.Map) == 0 {
			return fmt.Errorf("provider %q thinking.map is empty", defProv.Name)
		}
	}
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
