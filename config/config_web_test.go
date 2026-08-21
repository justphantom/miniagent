package config

import "testing"

// The web section: valid when present with a parseable listen, rejected on unknown fields
// inside web, and structurally verified (the remote-without-key rule lives at -serve startup
// where $MINIAGENT_WEB_KEY is visible — config load stays env-free, so it is not tested here).

func TestLoadConfig_WebSection(t *testing.T) {
	body := `{
  "providers":[{"name":"main","chat_url":"https://api/v1/chat/completions","models":[{"name":"glm"}]}],
  "defaults":{"provider":"main","model":"glm"},
  "web":{"listen":"0.0.0.0:8787","key":"secret"}
}`
	cfg, err := LoadConfig(writeTmpConfig(t, body))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.Web.Listen != "0.0.0.0:8787" || cfg.Web.Key != "secret" {
		t.Errorf("web = %+v", cfg.Web)
	}
}

func TestLoadConfig_WebListenInvalid(t *testing.T) {
	body := `{
  "providers":[{"name":"main","chat_url":"https://api/v1/chat/completions","models":[{"name":"glm"}]}],
  "defaults":{"provider":"main","model":"glm"},
  "web":{"listen":"no-port-here"}
}`
	if _, err := LoadConfig(writeTmpConfig(t, body)); err == nil {
		t.Fatal("expected invalid listen error, got nil")
	}
}

func TestLoadConfig_WebUnknownFieldRejected(t *testing.T) {
	body := `{
  "providers":[{"name":"main","chat_url":"https://api/v1/chat/completions","models":[{"name":"glm"}]}],
  "defaults":{"provider":"main","model":"glm"},
  "web":{"listen":"127.0.0.1:8787","tls":true}
}`
	_, err := LoadConfig(writeTmpConfig(t, body))
	if err == nil {
		t.Fatal("expected unknown-field error, got nil")
	}
}
