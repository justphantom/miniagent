package config

import (
	"strings"
	"testing"

	"github.com/justphantom/miniagent/internal/miniagent"
)

// validateConfig 拒绝 miniagent.ThinkingMapping.Field 指向保留 payload key：testBuildChatBody 用
// payload[field]=val 写思考级别（wire.go），命中保留 key（如 max_tokens）会 clobber
// 标准字段（审查 v3 P3）。
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
		if !strings.Contains(err.Error(), bad) || !strings.Contains(err.Error(), "保留") {
			t.Errorf("field %q: error should mention reserved key, got: %v", bad, err)
		}
	}
}

// 非保留 key（reasoning/thinking/extended_thinking/reasoning_effort）通过（不在 thinkingFieldBlacklist 中）。
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
