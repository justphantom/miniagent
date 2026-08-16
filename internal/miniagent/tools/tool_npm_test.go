package tools

import (
	"context"
	"os/exec"
	"strings"
	"testing"
)

func TestNpm_RejectsDisallowedSubcommand(t *testing.T) {
	got := NpmTool(t.TempDir(), 0, 0)
	for _, sub := range []string{"publish", "adduser", "logout", "create", "init", "foobar"} {
		res := got.Call(context.Background(), `{"subcommand":"`+sub+`"}`)
		if !res.IsError || !strings.Contains(res.Output, "not allowed") {
			t.Errorf("npm %q should be rejected, got: %s", sub, res.Output)
		}
	}
}

func TestNpm_MissingSubcommand(t *testing.T) {
	res := NpmTool(t.TempDir(), 0, 0).Call(context.Background(), `{}`)
	if !res.IsError || !strings.Contains(res.Output, "missing argument: subcommand") {
		t.Errorf("expected missing subcommand error, got: %s", res.Output)
	}
}

// --prefix/-C redirect npm's working root outside the module tree; --registry redirects the dependency
// stream to an attacker-controlled server. npm accepts single-dash long spellings (-registry=URL is
// equivalent), so the optSpec dash-normalizing matcher must catch both forms.
func TestNpm_DeniesRedirectOptions(t *testing.T) {
	got := NpmTool(t.TempDir(), 0, 0)
	for _, opt := range []string{
		"--prefix /tmp/other", "--prefix=/tmp/other", "-C /tmp/other",
		"--C /tmp/other", "--C=/tmp/other", "-C=/tmp/other",
		"--registry https://evil.reg", "--registry=https://evil.reg",
		"-prefix=/tmp/other", "-prefix /tmp/other", "-registry=https://evil.reg",
	} {
		res := got.Call(context.Background(), `{"subcommand":"install","args":"`+opt+`"}`)
		if !res.IsError || !strings.Contains(res.Output, "blocked") {
			t.Errorf("npm install %q should be blocked, got: %s", opt, res.Output)
		}
	}
}

func TestNpm_AllowedSubcommandRuns(t *testing.T) {
	if _, err := exec.LookPath("npm"); err != nil {
		t.Skip("npm not available")
	}
	dir := t.TempDir()
	res := NpmTool(dir, 0, 0).Call(context.Background(), `{"subcommand":"version"}`)
	if res.IsError {
		t.Fatalf("npm version should succeed, got: %s", res.Output)
	}
}

// SplitTruncate：install/test 的错误摘要（ELIFECYCLE/exit 1）在尾部。
func TestNpm_SplitTruncateSet(t *testing.T) {
	if !NpmTool(t.TempDir(), 0, 0).SplitTruncate {
		t.Fatal("npm tool must set SplitTruncate (error summaries live in the tail)")
	}
}
