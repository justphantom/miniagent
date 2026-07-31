package miniagent

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
	res := GlobTool(dir).Call(context.Background(), `{"pattern":"*.go"}`)
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
	res := GlobTool(dir).Call(context.Background(), `{"pattern":"*.rs"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if !strings.Contains(res.Output, "无匹配") {
		t.Errorf("Output = %q", res.Output)
	}
}

func TestGlobTool_InvalidPattern(t *testing.T) {
	dir := t.TempDir()
	res := GlobTool(dir).Call(context.Background(), `{"pattern":"["}`)
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
	res := GlobTool(dir).Call(context.Background(), `{"pattern":"*.go"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	// 仅列 a.go，不含 .git/x.go。
	lines := strings.Split(strings.TrimSpace(res.Output), "\n")
	if len(lines) != 1 || lines[0] != "a.go" {
		t.Errorf(".git not skipped: %s", res.Output)
	}
}
