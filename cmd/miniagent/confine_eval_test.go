package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/justphantom/miniagent/internal/miniagent"
)

// confine_eval_symlinks (opt-in): a normal existing in-workdir path passes — no regression vs the lexical check.
func TestCheckConfine_EvalSymlinks_NormalPath(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.MkdirAll(sub, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "f.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkConfine(root, filepath.Join("sub", "f.txt"), false, true); err != nil {
		t.Errorf("eval on normal in-workdir path: %v, want nil", err)
	}
}

// ENOENT fallback (the critical gotcha): a not-yet-created in-workdir path (the MkdirAll/Create case) must pass — EvalSymlinks
// errors on a non-existent path, so the eval branch falls back to the lexical result, preserving create semantics.
func TestCheckConfine_EvalSymlinks_NonExistentFallsBack(t *testing.T) {
	root := t.TempDir()
	if err := checkConfine(root, filepath.Join("new", "deep", "file.txt"), false, true); err != nil {
		t.Errorf("eval on non-existent in-workdir path: %v, want nil (ENOENT fallback for create)", err)
	}
}

// A symlink component in the path is rejected by the lexical per-component check whether or not eval is on (eval does not bypass it).
func TestCheckConfine_EvalSymlinks_SymlinkComponentCaught(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	if err := checkConfine(root, filepath.Join("link", "file"), false, true); err == nil {
		t.Error("symlink component in path: want error, got nil")
	}
}

// confineAuto=false (default) + ModeAuto: file tools are NOT wrapped (current behavior). Just confirms the buildTools flag
// threads through without panicking; the wrapping-vs-not distinction is covered by the buildTools confine condition.
func TestBuildTools_ConfineFlagsThread(t *testing.T) {
	built := buildTools("/wd", 0, 0, 0, miniagent.ModeAuto, 0, miniagent.Limits{}, false, false)
	if len(built) == 0 {
		t.Fatal("buildTools returned no tools")
	}
}
