package tools

import (
	"context"
	"fmt"
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

// A malformed-bracket tail after a literal prefix passes EVERY fabricated probe (measured: all-x
// probes of length 1..255): Match reports syntax errors lazily and the literal chunk fails the probe
// before the "[" is ever parsed, silently yielding "no matches". Only the structural scan
// (globPatternMalformed) reaches these shapes; probes remain for the forms Match itself reports.
func TestGlobTool_InvalidBracketTailAfterWildcard(t *testing.T) {
	dir := t.TempDir()
	for _, pattern := range []string{"xx*a[", "xxx*a[", "xx?[", "*a[", "q*a[", "data*[", "ab*c[d", "q****a["} {
		res := GlobTool(dir, 0, 0).Call(context.Background(), fmt.Sprintf(`{"pattern":%q}`, pattern))
		if !res.IsError {
			t.Errorf("pattern %q: expected invalid-pattern error, got: %q", pattern, res.Output)
			continue
		}
		if !strings.Contains(res.Output, "invalid glob pattern") {
			t.Errorf("pattern %q: error should name the invalid pattern, got: %q", pattern, res.Output)
		}
		if strings.Contains(res.Output, "no matches") {
			t.Errorf("pattern %q: malformed pattern must not be reported as no matches: %q", pattern, res.Output)
		}
	}
}

// Well-formed patterns that simply fail both probes must keep returning "no matches", not an error.
func TestGlobTool_ValidNoMatchPatternStillOk(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"a.go": ""})
	for _, p := range []string{"q*a", "*.rs", "zzz*"} {
		res := GlobTool(dir, 0, 0).Call(context.Background(), fmt.Sprintf(`{"pattern":%q}`, p))
		if res.IsError {
			t.Errorf("pattern %q: unexpected error: %s", p, res.Output)
		}
		if !strings.Contains(res.Output, "no matches") {
			t.Errorf("pattern %q: want no matches, got: %q", p, res.Output)
		}
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
