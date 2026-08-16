package tools

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// git init helper: skips when git is unavailable; sets a deterministic identity.
func initGitRepo(t *testing.T, dir string) {
	t.Helper()
	if err := exec.CommandContext(context.Background(), "git", "init", dir).Run(); err != nil {
		t.Skipf("git not available: %v", err)
	}
	_ = exec.CommandContext(context.Background(), "git", "-C", dir, "config", "user.email", "t@t").Run()
	_ = exec.CommandContext(context.Background(), "git", "-C", dir, "config", "user.name", "t").Run()
}

// Subdirectory .gitattributes used to bypass the driver guard entirely: only the repo-root
// file was read, but git applies attributes from every directory level (audit probe:
// workdir=sub with sub/.gitattributes filter= + defined config driver executed on add).
func TestGit_AttributesInSubdirectoryRejected(t *testing.T) {
	base := t.TempDir()
	initGitRepo(t, base)
	if err := exec.CommandContext(context.Background(), "git", "-C", base, "config", "--local", "filter.xclean.clean", "sed s/x/y/").Run(); err != nil {
		t.Skipf("cannot write git config: %v", err)
	}
	sub := filepath.Join(base, "sub")
	if err := os.MkdirAll(sub, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, ".gitattributes"), []byte("*.txt filter=xclean\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "a.txt"), []byte("xx"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Layout 1: workdir IS the subdirectory declaring the filter.
	if res := GitTool(sub, 0, 0).Call(context.Background(), `{"subcommand":"add","args":"a.txt"}`); !res.IsError ||
		!strings.Contains(res.Output, ".gitattributes") {
		t.Errorf("subdir-declared driver with workdir=sub should be rejected, got: %s", res.Output)
	}
	// Layout 2: workdir is the repo root, attributes in sub/ apply to sub/a.txt.
	if res := GitTool(base, 0, 0).Call(context.Background(), `{"subcommand":"add","args":"sub/a.txt"}`); !res.IsError ||
		!strings.Contains(res.Output, ".gitattributes") {
		t.Errorf("subdir-declared driver with workdir=root should be rejected, got: %s", res.Output)
	}
}

// .git/info/attributes is read by git even when NO workdir .gitattributes exists; the
// filter executes on `git add` (verified against real git before writing this test).
func TestGit_InfoAttributesRejected(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	if err := exec.CommandContext(context.Background(), "git", "-C", dir, "config", "--local", "filter.xclean.clean", "sed s/x/y/").Run(); err != nil {
		t.Skipf("cannot write git config: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".git", "info"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git", "info", "attributes"), []byte("*.txt filter=xclean\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("xx"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := GitTool(dir, 0, 0).Call(context.Background(), `{"subcommand":"add","args":"a.txt"}`)
	if !res.IsError || !strings.Contains(res.Output, ".gitattributes") {
		t.Errorf("info/attributes-declared driver should be rejected, got: %s", res.Output)
	}
}

// Attribute sources WITHOUT any defined config driver must keep passing (undefined tokens like
// `diff=java` are common and harmless) — the widened source collection must not over-block.
func TestGit_InfoAttributesUndefinedDriverPasses(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	if err := os.MkdirAll(filepath.Join(dir, ".git", "info"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git", "info", "attributes"), []byte("*.txt diff=java\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if res := GitTool(dir, 0, 0).Call(context.Background(), `{"subcommand":"status"}`); res.IsError {
		t.Errorf("undefined driver in info/attributes should pass, got: %s", res.Output)
	}
}
