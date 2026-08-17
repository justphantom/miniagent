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

// ** 与含 / 模式按相对路径段匹配（实测会话：**/app.css 连试 6 种 pattern 全 no matches 后
// 模型放弃）。无 / 模式保持 basename 语义（*.go 仍列深层文件，历史行为）。
func TestGlobTool_DoubleStarAndSlashPatterns(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{
		"app.css":                       "",
		"internal/admin/assets.go":      "",
		"internal/admin/assets/app.css": "",
		"internal/x_test.go":            "",
		"other/app.css":                 "",
	})
	for _, tc := range []struct{ pattern, want string }{
		{"**/app.css", "internal/admin/assets/app.css"}, // 深层命中 + 根层命中
		{"internal/**", "internal/admin/assets.go"},     // 目录前缀 + ** 收尾
		{"internal/**/*_test.go", "internal/x_test.go"}, // ** 后跟段
		{"other/app.css", "other/app.css"},              // 字面相对路径
	} {
		res := GlobTool(dir, 0, 0).Call(context.Background(), fmt.Sprintf(`{"pattern":%q}`, tc.pattern))
		if res.IsError {
			t.Errorf("pattern %q: unexpected error: %s", tc.pattern, res.Output)
			continue
		}
		if !strings.Contains(res.Output, tc.want) {
			t.Errorf("pattern %q: want %q in output, got: %q", tc.pattern, tc.want, res.Output)
		}
	}
	// **/app.css 命中根层与所有深层 app.css（共 3 处）。
	res := GlobTool(dir, 0, 0).Call(context.Background(), `{"pattern":"**/app.css"}`)
	if n := strings.Count(res.Output, "app.css"); n != 3 {
		t.Errorf("**/app.css should match 3 files (root + 2 nested), got %d: %q", n, res.Output)
	}
	// 含 / 模式不再匹配纯文件名错位的路径。
	res = GlobTool(dir, 0, 0).Call(context.Background(), `{"pattern":"internal/*.css"}`)
	if strings.Contains(res.Output, "assets/app.css") {
		t.Errorf("internal/*.css must not cross / into assets/: %q", res.Output)
	}
	// 无 / 模式保持 basename 语义。
	res = GlobTool(dir, 0, 0).Call(context.Background(), `{"pattern":"app.css"}`)
	if !strings.Contains(res.Output, "other/app.css") {
		t.Errorf("slash-less pattern keeps basename semantics: %q", res.Output)
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
