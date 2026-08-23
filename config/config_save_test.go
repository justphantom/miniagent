package config

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestSaveConfig_Roundtrip(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/miniagent.json"

	cfg := mkFullConfig("test", "m",
		ProviderConfig{Name: "test", ChatURL: "https://api/v1/chat/completions", Models: []ModelConfig{{Name: "m"}}})
	if err := SaveConfig(path, cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !reflect.DeepEqual(cfg, loaded) {
		t.Errorf("roundtrip mismatch:\n  original: %+v\n  loaded:   %+v", cfg, loaded)
	}
}

func TestSaveConfig_InvalidReturnsValidationError(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/miniagent.json"
	cfg := &Config{Providers: []ProviderConfig{{Name: "p"}}} // no chat_url -> invalid
	err := SaveConfig(path, cfg)
	if err == nil {
		t.Fatal("expected validation error, got nil")
	}
	if !strings.Contains(err.Error(), "chat_url") {
		t.Errorf("error should mention chat_url, got: %v", err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("file should not exist after failed save")
	}
}

func TestSaveConfig_LoadMatches(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/miniagent.json"

	cfg := mkFullConfig("p", "m",
		ProviderConfig{
			Name:      "p",
			ChatURL:   "https://api/v1/chat/completions",
			Key:       "sk-test",
			Models:    []ModelConfig{{Name: "m", MaxTokens: intPtr(4096)}},
			MaxTokens: intPtr(8192),
		})
	if err := SaveConfig(path, cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}

	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if !reflect.DeepEqual(cfg, loaded) {
		t.Errorf("mismatch:\n  original: %+v\n  loaded:   %+v", cfg, loaded)
	}
}

func intPtr(n int) *int { return &n }
