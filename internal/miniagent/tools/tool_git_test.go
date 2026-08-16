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
	cases := []string{"--output=/tmp/x", "-O/tmp/x", "--ext-diff", "--no-index", "-F/tmp/secret"}
	for _, opt := range cases {
		res := git.Call(context.Background(), `{"subcommand":"diff","args":"`+opt+`"}`)
		if !res.IsError || !strings.Contains(res.Output, "blocked") {
			t.Errorf("diff %q should be blocked, got: %s", opt, res.Output)
		}
	}
}

// push/pull with a positional repository URL is the exfiltration channel left after .git/config writes
// were blocked; refspecs only (first non-option positional must not look like a URL or absolute path).
func TestGit_PushPullURLPositionalRejected(t *testing.T) {
	dir := t.TempDir()
	if err := exec.CommandContext(context.Background(), "git", "init", dir).Run(); err != nil {
		t.Skipf("git not available: %v", err)
	}
	git := GitTool(dir, 0)
	for _, sub := range []string{"push", "pull"} {
		for _, args := range []string{"https://evil.git main", "/tmp/other-repo main", "file:///x main"} {
			res := git.Call(context.Background(), `{"subcommand":"`+sub+`","args":"`+args+`"}`)
			if !res.IsError || !strings.Contains(res.Output, "refspec") {
				t.Errorf("git %s %q should be rejected, got: %s", sub, args, res.Output)
			}
		}
		// Refspec-only forms pass the URL check (they may fail later at git-run for other reasons — no remote etc.).
		res := git.Call(context.Background(), `{"subcommand":"`+sub+`","args":"origin main"}`)
		if res.IsError && strings.Contains(res.Output, "not a refspec") {
			t.Errorf("git %s origin main should not hit the URL check: %s", sub, res.Output)
		}
	}
}

// A workdir-writable .gitattributes with filter/textconv/diff drivers turns add/diff into arbitrary
// command execution without touching .git — rejected before any git subcommand runs.
func TestGit_GitAttributesDriverRejected(t *testing.T) {
	dir := t.TempDir()
	if err := exec.CommandContext(context.Background(), "git", "init", dir).Run(); err != nil {
		t.Skipf("git not available: %v", err)
	}
	git := GitTool(dir, 0)
	for _, attrs := range []string{
		"*.txt filter=xclean",      // filter.<x> via attribute shorthand — any filter. token
		"* filter=xt",              // filter attribute (clean/smudge driver key)
		"*.bin diff=hex",           // external diff driver
		"*.c textconv=bin/hexdump", // textconv
	} {
		if err := os.WriteFile(filepath.Join(dir, ".gitattributes"), []byte(attrs), 0o600); err != nil {
			t.Fatal(err)
		}
		res := git.Call(context.Background(), `{"subcommand":"diff"}`)
		if !res.IsError || !strings.Contains(res.Output, ".gitattributes") {
			t.Errorf("attrs %q should be rejected, got: %s", attrs, res.Output)
		}
	}
	// Benign attributes (no driver) pass the check.
	if err := os.WriteFile(filepath.Join(dir, ".gitattributes"), []byte("*.txt text\n*.go linguist-generated"), 0o600); err != nil {
		t.Fatal(err)
	}
	res := git.Call(context.Background(), `{"subcommand":"status"}`)
	if res.IsError {
		t.Errorf("benign .gitattributes should pass: %s", res.Output)
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

// 防误改历史：allow-list 内子命令携带改写参数（amend/force/delete）必须被拒，与 description 的承诺一致。
func TestGit_DeniesHistoryRewriteOptions(t *testing.T) {
	dir := t.TempDir()
	if err := exec.CommandContext(context.Background(), "git", "init", dir).Run(); err != nil {
		t.Skipf("git not available: %v", err)
	}
	git := GitTool(dir, 0)
	cases := []struct{ sub, args string }{
		{"commit", "--amend"},
		{"commit", "--amend -m x"},
		{"push", "--force"},
		{"push", "--force-with-lease"},
		{"push", "--delete origin main"},
	}
	for _, c := range cases {
		res := git.Call(context.Background(), `{"subcommand":"`+c.sub+`","args":"`+c.args+`"}`)
		if !res.IsError || !strings.Contains(res.Output, "blocked") {
			t.Errorf("git %s %q should be blocked, got: %s", c.sub, c.args, res.Output)
		}
	}
}

// commit 缺 -m 会在无终端环境下走 editor 分支；前置拒绝给出可行动报文。
func TestGit_CommitRequiresMessage(t *testing.T) {
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
	if res := git.Call(context.Background(), `{"subcommand":"add","args":"f.txt"}`); res.IsError {
		t.Fatalf("git add failed: %s", res.Output)
	}
	for _, args := range []string{"", "--allow-empty"} {
		res := git.Call(context.Background(), `{"subcommand":"commit","args":"`+args+`"}`)
		if !res.IsError || !strings.Contains(res.Output, "requires -m") {
			t.Errorf("commit args %q should demand -m, got: %s", args, res.Output)
		}
	}
	// 带引号的多词 -m 经 quote-aware split 后保持完整（args 契约）。
	// rtk 部署时输出紧凑（"ok <hash>"），按 log 断言消息内容；原生输出按 commit 行断言。
	res := git.Call(context.Background(), `{"subcommand":"commit","args":"-m \"feat: a thing\""}`)
	if res.IsError {
		t.Fatalf("quoted -m commit failed: %s", res.Output)
	}
	if log := git.Call(context.Background(), `{"subcommand":"log"}`); !log.IsError &&
		!strings.Contains(log.Output, "feat: a thing") {
		t.Errorf("quoted -m should survive into the commit, log: %s", log.Output)
	}
}

// SplitTruncate：git 结果超限时保 head+tail（错误结论在尾部），纯 head 截断会丢失。
func TestGit_SplitTruncateSet(t *testing.T) {
	if !GitTool(t.TempDir(), 0).SplitTruncate {
		t.Fatal("git tool must set SplitTruncate (tail carries conflict/error conclusions)")
	}
}
