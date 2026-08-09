package config

import (
	"strings"
	"testing"

	"github.com/justphantom/miniagent/internal/miniagent"
)

// validateConfig rejects miniagent.ThinkingMapping.Field pointing to a reserved payload key: testBuildChatBody writes
// the thinking level via payload[field]=val (wire.go), and hitting a reserved key (e.g. max_tokens) would clobber
// a standard field (review v3 P3).
func TestValidateConfig_ThinkingFieldBlacklisted(t *testing.T) {
	for _, bad := range []string{"max_tokens", "tools", "messages", "temperature", "model"} {
		cfg := &Config{
			Providers: []ProviderConfig{{
				Name:     "main",
				ChatURL:  "https://api/v1/chat/completions",
				Thinking: &miniagent.ThinkingMapping{Field: bad},
			}},
		}
		err := validateConfig(cfg)
		if err == nil {
			t.Errorf("field %q: expected error, got nil", bad)
			continue
		}
		if !strings.Contains(err.Error(), bad) || !strings.Contains(err.Error(), "reserved") {
			t.Errorf("field %q: error should mention reserved key, got: %v", bad, err)
		}
	}
}

// Non-reserved keys (reasoning/thinking/extended_thinking/reasoning_effort) pass (not in thinkingFieldBlacklist).
func TestValidateConfig_ThinkingFieldValid(t *testing.T) {
	for _, ok := range []string{"reasoning", "thinking", "extended_thinking", "reasoning_effort"} {
		cfg := mkFullConfig("main", "m", ProviderConfig{
			Name:     "main",
			ChatURL:  "https://api/v1/chat/completions",
			Thinking: &miniagent.ThinkingMapping{Field: ok},
		})
		if err := validateConfig(cfg); err != nil {
			t.Errorf("field %q should pass, got: %v", ok, err)
		}
	}
}
