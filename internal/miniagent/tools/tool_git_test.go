package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGit_RejectsDestructiveCommand(t *testing.T) {
	dir := t.TempDir()
	git := GitTool(dir, 0)
	// 历史改写 / 仓库管理 / 配置类子命令仍被拒
	cases := []string{"fetch", "clone", "reset", "merge", "rebase",
		"checkout", "rm", "mv", "tag -d", "branch -D", "restore", "switch",
		"stash", "config", "worktree", "clean", "grep"}
	for _, c := range cases {
		res := git.Call(context.Background(), `{"subcommand":"`+c+`"}`)
		if !res.IsError || !strings.Contains(res.Output, "not in the allow-list") {
			t.Errorf("git %q should be rejected, got: %s", c, res.Output)
		}
	}
}

func TestGit_AllowedCommandsRequireGitRepo(t *testing.T) {
	git := GitTool(t.TempDir(), 0)
	res := git.Call(context.Background(), `{"subcommand":"status"}`)
	if !res.IsError || !strings.Contains(res.Output, "not a git repository") {
		t.Errorf("expected 'not a git repository', got: %s", res.Output)
	}
}

func TestGit_ReadOnlySubcommandRuns(t *testing.T) {
	dir := t.TempDir()
	if err := exec.CommandContext(context.Background(), "git", "init", dir).Run(); err != nil {
		t.Skipf("git not available: %v", err)
	}
	git := GitTool(dir, 0)
	res := git.Call(context.Background(), `{"subcommand":"status"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if !strings.Contains(res.Output, "On branch") && !strings.Contains(res.Output, "No commits yet") {
		t.Errorf("expected branch info, got: %s", res.Output)
	}
}

func TestGit_DeniesFileWritingOptions(t *testing.T) {
	dir := t.TempDir()
	if err := exec.CommandContext(context.Background(), "git", "init", dir).Run(); err != nil {
		t.Skipf("git not available: %v", err)
	}
	git := GitTool(dir, 0)
	cases := []string{"--output=/tmp/x", "-O/tmp/x", "--ext-diff"}
	for _, opt := range cases {
		res := git.Call(context.Background(), `{"subcommand":"diff","args":"`+opt+`"}`)
		if !res.IsError || !strings.Contains(res.Output, "blocked") {
			t.Errorf("diff %q should be blocked, got: %s", opt, res.Output)
		}
	}
}

func TestGit_AddCommitWorkflow(t *testing.T) {
	dir := t.TempDir()
	if err := exec.CommandContext(context.Background(), "git", "init", dir).Run(); err != nil {
		t.Skipf("git not available: %v", err)
	}
	_ = exec.CommandContext(context.Background(), "git", "-C", dir, "config", "user.email", "t@t").Run()
	_ = exec.CommandContext(context.Background(), "git", "-C", dir, "config", "user.name", "t").Run()
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	git := GitTool(dir, 0)
	res := git.Call(context.Background(), `{"subcommand":"add","args":"f.txt"}`)
	if res.IsError {
		t.Fatalf("git add failed: %s", res.Output)
	}
	res = git.Call(context.Background(), `{"subcommand":"commit","args":"-m test"}`)
	if res.IsError {
		t.Fatalf("git commit failed: %s", res.Output)
	}
}

func TestGit_MissingSubcommand(t *testing.T) {
	git := GitTool(t.TempDir(), 0)
	res := git.Call(context.Background(), `{}`)
	if !res.IsError || !strings.Contains(res.Output, "missing argument: subcommand") {
		t.Errorf("expected missing subcommand error, got: %s", res.Output)
	}
}
