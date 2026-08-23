package config

import (
	"os"
	"strings"
	"testing"
)

// v5.0.0: strategy constants replaced the old mode/run fields.
func TestLoadConfig_StrategyConstants(t *testing.T) {
	body := `{"providers":[{"name":"p","chat_url":"https://a/v1/chat/completions"}],"defaults":{"provider":"p","model":"m"},"compaction":{"provider":"p","model":"m"},"run":{"max_tool_result_chars":1234,"max_file_result_chars":9999,"max_parallel_tools":3,"context_keep_recent":8,"summary_max_chars":1500,"context_keep_reasoning":2}}`
	cfg, err := LoadConfig(writeTmpConfig(t, body))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		got  *int
		want int
	}{
		{"max_tool_result_chars", cfg.Run.MaxToolResultChars, 1234},
		{"max_file_result_chars", cfg.Run.MaxFileResultChars, 9999},
		{"max_parallel_tools", cfg.Run.MaxParallelTools, 3},
		{"context_keep_recent", cfg.Run.ContextKeepRecent, 8},
		{"summary_max_chars", cfg.Run.SummaryMaxChars, 1500},
		{"context_keep_reasoning", cfg.Run.ContextKeepReasoning, 2},
	} {
		if c.got == nil || *c.got != c.want {
			t.Errorf("%s = %v, want %d", c.name, c.got, c.want)
		}
	}
}

// config.example.json is the release flagship sample: after stripping // comments it must be loadable by LoadConfig
// (after pinning, the openai provider explicitly declares thinking{field:reasoning_effort,map:identity}; defaults.thinking=off is not enforced).
func TestLoadConfig_ExampleFile(t *testing.T) {
	data, err := os.ReadFile("../config.example.json")
	if err != nil {
		t.Fatalf("read example: %v", err)
	}
	if _, err := LoadConfig(writeTmpConfig(t, stripJSONComments(string(data)))); err != nil {
		t.Fatalf("config.example.json should be loadable after stripping comments: %v", err)
	}
}

// stripJSONComments removes whole-line // comments (all comments in config.example.json are on standalone lines, so per-line checks suffice).
func stripJSONComments(in string) string {
	var b strings.Builder
	for line := range strings.SplitSeq(in, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}
