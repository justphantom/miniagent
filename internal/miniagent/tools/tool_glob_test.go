package tools

import (
	"context"
	"strings"
	"testing"
)

func TestGlobTool_Basic(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"a.go":     "",
		"b.txt":    "",
		"sub/c.go": "",
	})
	res := GlobTool(dir, 0, 0).Call(context.Background(), `{"pattern":"*.go"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if !strings.Contains(res.Output, "a.go") || !strings.Contains(res.Output, "sub/c.go") {
		t.Errorf("missing .go entries: %s", res.Output)
	}
	if strings.Contains(res.Output, "b.txt") {
		t.Errorf("b.txt should not match *.go: %s", res.Output)
	}
}

func TestGlobTool_NoMatch(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"a.go": ""})
	res := GlobTool(dir, 0, 0).Call(context.Background(), `{"pattern":"*.rs"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if !strings.Contains(res.Output, "no matches") {
		t.Errorf("Output = %q", res.Output)
	}
}

func TestGlobTool_InvalidPattern(t *testing.T) {
	dir := t.TempDir()
	res := GlobTool(dir, 0, 0).Call(context.Background(), `{"pattern":"["}`)
	if !res.IsError {
		t.Fatal("expected error")
	}
}

func TestGlobTool_SkipDotGit(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"a.go":      "",
		".git/x.go": "",
	})
	res := GlobTool(dir, 0, 0).Call(context.Background(), `{"pattern":"*.go"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	// Only a.go is listed; .git/x.go is excluded.
	lines := strings.Split(strings.TrimSpace(res.Output), "\n")
	if len(lines) != 1 || lines[0] != "a.go" {
		t.Errorf(".git not skipped: %s", res.Output)
	}
}

func TestGlobTool_MissingRootErrors(t *testing.T) {
	res := GlobTool(t.TempDir(), 0, 0).Call(context.Background(), `{"pattern":"*.go","path":"/nonexistent/dir"}`)
	if !res.IsError {
		t.Fatal("expected error for missing root")
	}
	if !strings.Contains(res.Output, "glob") {
		t.Errorf("error message should mention glob failure: %q", res.Output)
	}
}
