package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// splitArgs integration: git commit -m "multi word message" must preserve the message as one arg.
// Before the fix, strings.Fields split the quoted string into separate pathspecs and commit failed.
func TestGit_CommitMultiWordMessageViaQuotes(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	ctx := context.Background()
	_ = exec.CommandContext(ctx, "git", "init", dir).Run()
	_ = exec.CommandContext(ctx, "git", "-C", dir, "config", "user.email", "t@t").Run()
	_ = exec.CommandContext(ctx, "git", "-C", dir, "config", "user.name", "t").Run()
	os.WriteFile(filepath.Join(dir, "f.txt"), []byte("hi"), 0o644)

	git := GitTool(dir, 0, 0)
	if res := git.Call(context.Background(), `{"subcommand":"add","args":"f.txt"}`); res.IsError {
		t.Fatalf("git add failed: %s", res.Output)
	}

	res := git.Call(context.Background(), `{"subcommand":"commit","args":"-m \"feat: add new feature with details\""}`)
	if res.IsError {
		t.Fatalf("git commit with quoted multi-word message failed: %s", res.Output)
	}

	out, _ := exec.CommandContext(ctx, "git", "-C", dir, "log", "--format=%s").Output()
	got := strings.TrimSpace(string(out))
	if got != "feat: add new feature with details" {
		t.Errorf("commit subject = %q, want %q", got, "feat: add new feature with details")
	}
}
