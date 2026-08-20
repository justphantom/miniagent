package config

import (
	"fmt"
	"time"
)

func pickInt(ov, cv *int) *int {
	if ov != nil {
		return ov
	}
	return cv
}

// findModelConfig looks up model-level config by model id within provider.Models; if not declared it returns the zero value + false.
func findModelConfig(p ProviderConfig, modelID string) (ModelConfig, bool) {
	for _, m := range p.Models {
		if m.Name == modelID {
			return m, true
		}
	}
	return ModelConfig{}, false
}

// pickMPG arbitrates model>provider>global (no cli) for max_tokens/context_window.
func pickMPG(model, provider, global *int) *int {
	if model != nil {
		return model
	}
	if provider != nil {
		return provider
	}
	return global
}

// ParseDuration parses a duration string ("30s") from config; cv == nil means unset and returns (nil, nil).
// Negative values are invalid. Shared by Resolve and the cmd layer (httpTimeoutFromConfig), unifying duration validation semantics and error format.
func ParseDuration(cv *string, label string) (*time.Duration, error) {
	if cv == nil {
		return nil, nil
	}
	d, err := time.ParseDuration(*cv)
	if err != nil {
		return nil, fmt.Errorf("config %s %q: %w", label, *cv, err)
	}
	if d < 0 {
		return nil, fmt.Errorf("config %s %q: negative value is invalid", label, *cv)
	}
	return &d, nil
}
