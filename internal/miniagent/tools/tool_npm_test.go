package tools

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

func TestNpm_RejectsDisallowedSubcommand(t *testing.T) {
	got := NpmTool(t.TempDir(), 0)
	for _, sub := range []string{"publish", "adduser", "logout", "create", "init", "foobar"} {
		res := got.Call(context.Background(), `{"subcommand":"`+sub+`"}`)
		if !res.IsError || !strings.Contains(res.Output, "not allowed") {
			t.Errorf("npm %q should be rejected, got: %s", sub, res.Output)
		}
	}
}

func TestNpm_MissingSubcommand(t *testing.T) {
	res := NpmTool(t.TempDir(), 0).Call(context.Background(), `{}`)
	if !res.IsError || !strings.Contains(res.Output, "missing argument: subcommand") {
		t.Errorf("expected missing subcommand error, got: %s", res.Output)
	}
}

func TestNpm_AllowedSubcommandRuns(t *testing.T) {
	if _, err := exec.LookPath("npm"); err != nil {
		t.Skip("npm not available")
	}
	dir := t.TempDir()
	res := NpmTool(dir, 0).Call(context.Background(), `{"subcommand":"version"}`)
	if res.IsError {
		t.Fatalf("npm version should succeed, got: %s", res.Output)
	}
}
