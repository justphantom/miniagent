package main

import (
	"context"
	"fmt"
	"github.com/justphantom/miniagent/internal/miniagent/tools"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A relative path falling inside the workdir subtree should pass.
func TestCheckConfine_RelativeInside(t *testing.T) {
	root := t.TempDir()
	if err := checkConfine(root, "sub/file.txt", false); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

// An absolute path falling inside the workdir subtree should pass.
func TestCheckConfine_AbsoluteInside(t *testing.T) {
	root := t.TempDir()
	if err := checkConfine(root, filepath.Join(root, "sub", "file.txt"), false); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

// A path escaping workdir must be rejected.
func TestCheckConfine_OutsideRejected(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.txt")
	if err := checkConfine(root, outside, false); err == nil {
		t.Error("expected outside path to be rejected")
	}
}

// §P1-A read-back exception: read-only tools may read the persisted tool-output dir (outside workdir).
func TestConfineWrap_ToolOutputReadAllowlist(t *testing.T) {
	root := t.TempDir()
	out := t.TempDir()
	name := filepath.Join(out, "tool_1_call_1.txt")
	if err := os.WriteFile(name, []byte("full output"), 0o600); err != nil {
		t.Fatal(err)
	}
	wrapped := confineWrap(tools.ReadFileTool(root, 0, 0), root, true, []string{out})
	r := wrapped.Call(context.Background(), fmt.Sprintf(`{"path":%q}`, name))
	if r.IsError {
		t.Fatalf("read of allowlisted tool output failed: %+v", r)
	}
	if !strings.Contains(r.Output, "full output") {
		t.Errorf("allowlisted read should return content: %+v", r)
	}
	// Sibling of the allow-root is NOT covered.
	outside := filepath.Join(filepath.Dir(out), "sibling.txt")
	if err := os.WriteFile(outside, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	r = wrapped.Call(context.Background(), fmt.Sprintf(`{"path":%q}`, outside))
	if !r.IsError {
		t.Error("read outside allow-root should be rejected")
	}
}

func TestPathInRoots(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "a.txt")
	cases := []struct {
		p    string
		want bool
	}{
		{file, true},
		{root, true},
		{root + "/../other", false},
		{"/abs/other", false},
		{"relative", false},
		{"", false},
	}
	for _, c := range cases {
		if got := pathInRoots(c.p, []string{root}); got != c.want {
			t.Errorf("pathInRoots(%q)=%v, want %v", c.p, got, c.want)
		}
	}
	if pathInRoots(file, nil) {
		t.Error("nil roots should never match")
	}
}

// path="." or workdir itself must be rejected, to prevent overwriting the entire workdir.
func TestCheckConfine_RootItselfRejected(t *testing.T) {
	root := t.TempDir()
	if err := checkConfine(root, ".", false); err == nil {
		t.Error("expected workdir itself to be rejected")
	}
	if err := checkConfine(root, root, false); err == nil {
		t.Error("expected workdir itself to be rejected")
	}
}

// .git and everything under it must be rejected for ALL tools (read included): direct file access
// bypasses the git tool's allow-list (write .git/hooks/* → hook execution; edit .git/config remote →
// push exfiltration; read .git/config leaks remote URLs). Covers nested submodule layouts too.
func TestCheckConfine_DotGitRejected(t *testing.T) {
	root := t.TempDir()
	cases := []struct {
		path     string
		readOnly bool
	}{
		{".git", false},
		{".git/config", false},
		{".git/hooks/pre-commit", false},
		{".git/config", true}, // read-only tools are also rejected (leaks remote URLs/credentials)
		{"sub/.git/HEAD", false},
		{filepath.Join(root, ".git", "config"), false}, // absolute form
	}
	for _, c := range cases {
		if err := checkConfine(root, c.path, c.readOnly); err == nil {
			t.Errorf("expected %q to be rejected (readOnly=%v)", c.path, c.readOnly)
		}
	}
	// A directory merely NAMED like git (not ".git") stays usable.
	if err := checkConfine(root, "github/config.txt", false); err != nil {
		t.Errorf("github/config.txt should pass: %v", err)
	}
}

// On case-insensitive filesystems (Windows/macOS) .GIT IS the repo gitdir — git opens .git
// case-insensitively there — so the guard must not depend on exact case to hold.
func TestCheckConfine_DotGitCaseInsensitive(t *testing.T) {
	root := t.TempDir()
	for _, path := range []string{".GIT", ".GIT/config", ".Git/hooks/pre-commit", "sub/.GIT/HEAD"} {
		if err := checkConfine(root, path, false); err == nil {
			t.Errorf("expected %q to be rejected (case-insensitive .git)", path)
		}
	}
	// A directory named e.g. "gitstuff" (not a case variant of .git) stays usable.
	if err := checkConfine(root, "gitstuff/config.txt", false); err != nil {
		t.Errorf("gitstuff/config.txt should pass: %v", err)
	}
}

// rename sends {from,to} rather than {path}: the wrap must confine BOTH endpoints or rename is a
// no-op pass-through at the cmd layer. The escape here is a symlinked destination parent (lexical
// checkConfine catches it); resolveConfinedPath inside the tools package is purely lexical and would not.
func TestConfineWrap_RenameConfinesFromAndTo(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	linkdir := filepath.Join(root, "sub")
	if err := os.Symlink(outside, linkdir); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	wrapped := confineWrap(tools.RenameTool(root, 0), root, false, nil)
	r := wrapped.Call(context.Background(), `{"from":"a.txt","to":"sub/b.txt"}`)
	if !r.IsError || !strings.Contains(r.Output, "symlink") {
		t.Errorf("rename into symlinked dir should be rejected by the wrap, got: %+v", r)
	}
	if _, err := os.Stat(filepath.Join(outside, "b.txt")); err == nil {
		t.Error("rename landed outside workdir despite confineWrap")
	}
	if _, err := os.Stat(filepath.Join(root, "a.txt")); err != nil {
		t.Errorf("source should be intact after rejected rename: %v", err)
	}
	// An out-of-workdir `from` is caught too (the tools layer resolves it confined, the cmd layer must not be a no-op).
	r = wrapped.Call(context.Background(), fmt.Sprintf(`{"from":%q,"to":"ok.txt"}`, filepath.Join(outside, "secret.txt")))
	if !r.IsError || !strings.Contains(r.Output, "escapes workdir") {
		t.Errorf("rename from outside workdir should be rejected, got: %+v", r)
	}
}

// A rename whose both endpoints are inside workdir still works after the from/to check was added.
func TestConfineWrap_RenameInsideWorkdir(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	wrapped := confineWrap(tools.RenameTool(root, 0), root, false, nil)
	r := wrapped.Call(context.Background(), `{"from":"a.txt","to":"sub/b.txt"}`)
	if r.IsError {
		t.Fatalf("in-workdir rename should pass, got: %s", r.Output)
	}
	if _, err := os.Stat(filepath.Join(root, "sub", "b.txt")); err != nil {
		t.Errorf("renamed file missing: %v", err)
	}
}

// Existing path components containing symlinks must be rejected.
func TestCheckConfine_SymlinkComponentRejected(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	link := filepath.Join(root, "linkdir")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	if err := checkConfine(root, "linkdir/file.txt", false); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Errorf("expected symlink component to be rejected, got: %v", err)
	}
}

// Non-existent subdirectories are allowed (created later by the tool).
func TestCheckConfine_NonExistentSubdirAllowed(t *testing.T) {
	root := t.TempDir()
	if err := checkConfine(root, "not-yet/exists.txt", false); err != nil {
		t.Errorf("non-existent subdir should be allowed: %v", err)
	}
}

// A write tool wrapped by confineWrap rejects paths containing symlinks and does not execute the original tool.
func TestConfineWrap_BlocksSymlinkPath(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "target.txt")
	if err := os.WriteFile(outsideFile, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	linkdir := filepath.Join(root, "linkdir")
	if err := os.Symlink(outside, linkdir); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}

	tool := tools.WriteFileTool(root, 0)
	wrapped := confineWrap(tool, root, false, nil)
	r := wrapped.Call(context.Background(), `{"path":"linkdir/pwned.txt","content":"x"}`)
	if !r.IsError || !strings.Contains(r.Output, "symlink") {
		t.Errorf("expected confineWrap to reject symlink path, got: %+v", r)
	}
	// The target file should not be created/overwritten.
	if _, err := os.Stat(filepath.Join(outside, "pwned.txt")); err == nil {
		t.Error("symlink target was written outside workdir")
	}
}

// confineWrap still allows writing on regular paths.
func TestConfineWrap_AllowsRegularPath(t *testing.T) {
	root := t.TempDir()
	tool := tools.WriteFileTool(root, 0)
	wrapped := confineWrap(tool, root, false, nil)
	r := wrapped.Call(context.Background(), `{"path":"sub/ok.txt","content":"hello"}`)
	if r.IsError {
		t.Fatalf("expected regular path to pass: %s", r.Output)
	}
	got, err := os.ReadFile(filepath.Join(root, "sub", "ok.txt"))
	if err != nil || string(got) != "hello" {
		t.Errorf("file not written correctly: %q err=%v", got, err)
	}
}

// confineWrap passes default/empty path through to orig: each tool self-validates path (write rejects an empty path),
// no unconstrained successful write is produced. Regression guard: previously rejecting all empty paths once hurt grep/glob.
func TestConfineWrap_EmptyPathFallsThroughToOrig(t *testing.T) {
	root := t.TempDir()
	wrapped := confineWrap(tools.WriteFileTool(root, 0), root, false, nil)
	// Non-JSON / missing path / empty path: all should be rejected by orig(write), no successful write allowed.
	for _, args := range []string{`not-json`, `{"content":"x"}`, `{"path":""}`} {
		r := wrapped.Call(context.Background(), args)
		if !r.IsError {
			t.Errorf("args %q: want orig to reject, got success: %+v", args, r)
		}
	}
	// No files should appear in root (security property: no unconstrained write).
	if ents, _ := os.ReadDir(root); len(ents) != 0 {
		t.Errorf("expected empty root after rejected writes, got %+v", ents)
	}
}

// Regression: grep path is optional (defaults to workdir), confineWrap must not reject due to missing path.
func TestConfineWrap_GrepWithoutPathWorks(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("hello foo bar"), 0o600); err != nil {
		t.Fatal(err)
	}
	grep := confineWrap(tools.GrepTool(root, 0, 0, 0), root, true, nil)
	r := grep.Call(context.Background(), `{"pattern":"foo"}`)
	if r.IsError {
		t.Fatalf("grep without path should work (default workdir), got error: %+v", r)
	}
	if !strings.Contains(r.Output, "foo") {
		t.Errorf("grep should match: %+v", r)
	}
}

// Regression: confinement still holds — an explicit path escaping workdir (absolute path or ..) must be rejected.
func TestConfineWrap_ConfinesEscapePath(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	// write with .. out of bounds
	wrapped := confineWrap(tools.WriteFileTool(root, 0), root, false, nil)
	r := wrapped.Call(context.Background(), `{"path":"../escape.txt","content":"x"}`)
	if !r.IsError || !strings.Contains(r.Output, "escapes workdir") {
		t.Errorf("write escaping workdir should be rejected: %+v", r)
	}
	// grep with an absolute path escaping bounds (grep uses workspaceRoot but still needs confine to prevent absolute path escape)
	grep := confineWrap(tools.GrepTool(root, 0, 0, 0), root, true, nil)
	r = grep.Call(context.Background(), fmt.Sprintf(`{"pattern":"x","path":%q}`, outside))
	if !r.IsError || !strings.Contains(r.Output, "escapes workdir") {
		t.Errorf("grep escaping workdir should be rejected: %+v", r)
	}
}

// checkConfine: .. out-of-bounds rejected; an existing path component containing a symlink is also rejected (narrowing the TOCTOU window).
func TestCheckConfine(t *testing.T) {
	dir := t.TempDir()
	inner := filepath.Join(dir, "inner.txt")
	if err := os.WriteFile(inner, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := checkConfine(dir, "inner.txt", false); err != nil {
		t.Errorf("inner relative rejected: %v", err)
	}
	if err := checkConfine(dir, filepath.Join(dir, "..", "outside"), false); err == nil {
		t.Error("escape via .. should be rejected")
	}
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("s"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	// When an existing component is a symlink, reject it, to prevent IO landing outside workdir (default mode is still a thin
	// soft constraint; true isolation still relies on the OS; but obvious symlink escapes are no longer actively allowed).
	if err := checkConfine(dir, "link", false); err == nil {
		t.Error("existing symlink component should be rejected")
	}
}

// checkConfine rejects path="." or the workdir absolute path itself for WRITE tools: rename over a directory would
// EISDIR (ambiguous error), and if MkdirAll/Rename actually took effect it would destroy the entire workdir (review P3-8).
func TestCheckConfine_RejectsWorkdirRoot(t *testing.T) {
	dir := t.TempDir()
	if err := checkConfine(dir, ".", false); err == nil {
		t.Error(`path="." should be rejected as workdir root`)
	}
	if err := checkConfine(dir, dir, false); err == nil {
		t.Error("absolute path equal to workdir root should be rejected")
	}
}

// Read-only tools (read/grep/glob) may target the workdir root — reading/listing the whole workdir is legitimate.
// The "root overwrite" guard above is for write tools only (MkdirAll/Rename clobbering the workdir).
func TestCheckConfine_ReadOnlyAllowsWorkdirRoot(t *testing.T) {
	root := t.TempDir()
	if err := checkConfine(root, ".", true); err != nil {
		t.Errorf(`read-only path="." should be allowed: %v`, err)
	}
	if err := checkConfine(root, root, true); err != nil {
		t.Errorf("read-only workdir root should be allowed: %v", err)
	}
	// Read-only still rejects escapes.
	outside := filepath.Join(t.TempDir(), "x")
	if err := checkConfine(root, outside, true); err == nil {
		t.Error("read-only should still reject out-of-workdir path")
	}
}

// confineWrap readOnly=true lets grep target the workdir root (absolute path or "."); write (readOnly=false) still rejects it.
func TestConfineWrap_ReadOnlyGrepAllowsRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("hello foo"), 0o600); err != nil {
		t.Fatal(err)
	}
	grep := confineWrap(tools.GrepTool(root, 0, 0, 0), root, true, nil)
	for _, p := range []string{root, "."} {
		r := grep.Call(context.Background(), fmt.Sprintf(`{"pattern":"foo","path":%q}`, p))
		if r.IsError {
			t.Errorf("read-only grep path=%q should be allowed, got: %+v", p, r)
		} else if !strings.Contains(r.Output, "foo") {
			t.Errorf("grep path=%q should match: %+v", p, r)
		}
	}
	// Write tool (readOnly=false) still rejects the workdir root.
	write := confineWrap(tools.WriteFileTool(root, 0), root, false, nil)
	r := write.Call(context.Background(), fmt.Sprintf(`{"path":%q,"content":"x"}`, root))
	if !r.IsError || !strings.Contains(r.Output, "workdir root") {
		t.Errorf("write targeting workdir root should be rejected: %+v", r)
	}
}
