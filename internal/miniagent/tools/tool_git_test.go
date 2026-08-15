package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGit_RejectsDestructiveCommand(t *testing.T) {
	dir := t.TempDir()
	git := GitTool(dir, 0, false)
	cases := []string{"push", "pull", "fetch", "clone", "reset", "commit", "merge", "rebase", "checkout", "rm", "add", "mv", "tag -d", "branch -D", "reset HEAD", "restore", "switch", "checkout -b"}
	for _, c := range cases {
		res := git.Call(context.Background(), `{"subcommand":"`+c+`"}`)
		if !res.IsError || !strings.Contains(res.Output, "not in the allow-list") {
			t.Errorf("git %q should be rejected, got: %s", c, res.Output)
		}
	}
}

func TestGit_AllowedCommandsRequireGitRepo(t *testing.T) {
	dir := t.TempDir()
	git := GitTool(dir, 0, false)
	res := git.Call(context.Background(), `{"subcommand":"status"}`)
	if !res.IsError || !strings.Contains(res.Output, "not a git repository") {
		t.Errorf("expected 'not a git repository', got: %s", res.Output)
	}
}

func TestGit_GitRepoOperations(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, ".git"), 0755)
	git := GitTool(dir, 0, false)
	res := git.Call(context.Background(), `{"subcommand":"status"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if !strings.Contains(res.Output, "On branch") {
		t.Logf("git status output: %s", res.Output)
	}
}

func TestGit_KnownCmdNotInAllowList(t *testing.T) {
	dir := t.TempDir()
	git := GitTool(dir, 0, false)
	res := git.Call(context.Background(), `{"subcommand":"foobar"}`)
	if !res.IsError || !strings.Contains(res.Output, "not in the allow-list") {
		t.Errorf("unknown command should be rejected: %s", res.Output)
	}
}

func TestGit_CleanWithoutDryRunBlocked(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, ".git"), 0755)
	git := GitTool(dir, 0, false)
	res := git.Call(context.Background(), `{"subcommand":"clean","args":"-f"}`)
	if !res.IsError || !strings.Contains(res.Output, "--dry-run") {
		t.Errorf("clean without --dry-run should be rejected: %s", res.Output)
	}
	res = git.Call(context.Background(), `{"subcommand":"clean","args":"--dry-run"}`)
	if res.IsError {
		t.Fatalf("unexpected error with --dry-run: %s", res.Output)
	}
}

func TestGit_MissingSubcommand(t *testing.T) {
	git := GitTool(t.TempDir(), 0, false)
	res := git.Call(context.Background(), `{}`)
	if !res.IsError || !strings.Contains(res.Output, "missing argument: subcommand") {
		t.Errorf("expected missing subcommand error: %s", res.Output)
	}
