package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// callAst runs the ast tool against root with raw JSON args, returning output and error flag.
func callAst(t *testing.T, root, args string) (string, bool) {
	t.Helper()
	r := AstTool(root, time.Second, 0).Call(context.Background(), args)
	return r.Output, r.IsError
}

// writeGo creates a .go file under root; dir parts are created.
func writeGo(t *testing.T, root, rel, content string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

const sampleGo = `package p

// comment mentioning RunHelper — a comment must not match
const Alpha = 1
const (
	Beta  = 2
	Gamma = 3
)

var Delta = "x"
var (
	Epsilon bool
)

type Zeta struct{}
type Eta interface{ Foo() }
type Theta int

func Run() {}
func runHelper() {}

type Owner struct{}
type Box[T any] struct{ v T }

func (o *Owner) Drive() {}
func (o Owner) Park() {}
func (b *Box[T]) Get() T { return b.v }
`

// Full sweep: kinds, method receivers (incl. generic receiver type), comment immunity, refined kinds in output.
func TestAst_KindsAndPattern(t *testing.T) {
	root := t.TempDir()
	writeGo(t, root, "a.go", sampleGo)
	out, isErr := callAst(t, root, `{"pattern":"."}`)
	if isErr {
		t.Fatalf("unexpected error: %s", out)
	}
	for _, sub := range []string{
		"a.go:4:const Alpha", "a.go:6:const Beta", "a.go:7:const Gamma",
		"a.go:10:var Delta", "a.go:12:var Epsilon",
		"a.go:15:struct Zeta", "a.go:16:interface Eta", "a.go:17:type Theta",
		"a.go:19:func Run", "a.go:20:func runHelper",
		"a.go:22:struct Owner", "a.go:23:struct Box",
		"a.go:25:method (Owner) Drive", "a.go:26:method (Owner) Park", "a.go:27:method (Box) Get",
	} {
		if !strings.Contains(out, sub) {
			t.Errorf("output missing %q; got:\n%s", sub, out)
		}
	}
	if strings.Contains(out, "RunHelper") {
		t.Errorf("comment mention must not match (AST, not text):\n%s", out)
	}
}

// kind filter: interface/struct refine type; type alone must not drag them in; commas union.
func TestAst_KindFilter(t *testing.T) {
	root := t.TempDir()
	writeGo(t, root, "a.go", sampleGo)

	out, _ := callAst(t, root, `{"pattern":".","kind":"interface"}`)
	if !strings.Contains(out, "interface Eta") {
		t.Errorf("kind=interface must match Eta:\n%s", out)
	}
	if strings.Contains(out, "Zeta") || strings.Contains(out, "Theta") || strings.Contains(out, "Owner") {
		t.Errorf("kind=interface must match only interfaces:\n%s", out)
	}

	out, _ = callAst(t, root, `{"pattern":".","kind":"struct"}`)
	if !strings.Contains(out, "struct Zeta") || !strings.Contains(out, "struct Owner") {
		t.Errorf("kind=struct must match Zeta and Owner:\n%s", out)
	}
	if strings.Contains(out, "interface Eta") || strings.Contains(out, "type Theta") {
		t.Errorf("kind=struct must not match interface or plain type:\n%s", out)
	}

	out, _ = callAst(t, root, `{"pattern":".","kind":"type"}`)
	if !strings.Contains(out, "type Theta") {
		t.Errorf("kind=type must match plain type:\n%s", out)
	}
	if strings.Contains(out, "struct Zeta") || strings.Contains(out, "interface Eta") {
		t.Errorf("kind=type must not drag interface/struct in (explicit refinement):\n%s", out)
	}

	out, _ = callAst(t, root, `{"pattern":"^.(eta|psilon)$","kind":"const,var"}`)
	if !strings.Contains(out, "const Beta") || !strings.Contains(out, "var Epsilon") {
		t.Errorf("comma-separated kinds must union:\n%s", out)
	}
	if strings.Contains(out, "func") {
		t.Errorf("kind=const,var must exclude funcs:\n%s", out)
	}

	if out, isErr := callAst(t, root, `{"pattern":".","kind":"funcx"}`); !isErr {
		t.Errorf("invalid kind must be an error, got: %s", out)
	}
}

// func/method are disjoint kinds: kind=func excludes methods and vice versa.
func TestAst_FuncVsMethod(t *testing.T) {
	root := t.TempDir()
	writeGo(t, root, "a.go", sampleGo)
	out, _ := callAst(t, root, `{"pattern":".","kind":"func"}`)
	if strings.Contains(out, "method") {
		t.Errorf("kind=func must exclude methods:\n%s", out)
	}
	if !strings.Contains(out, "func Run") || !strings.Contains(out, "func runHelper") {
		t.Errorf("kind=func must match free funcs:\n%s", out)
	}
	out, _ = callAst(t, root, `{"pattern":".","kind":"method"}`)
	if strings.Contains(out, "func Run") {
		t.Errorf("kind=method must exclude free funcs:\n%s", out)
	}
}

// Regex on the name: ^-anchor and (?i) work as documented.
func TestAst_RegexOnName(t *testing.T) {
	root := t.TempDir()
	writeGo(t, root, "a.go", sampleGo)
	out, _ := callAst(t, root, `{"pattern":"^run"}`)
	if !strings.Contains(out, "func runHelper") || strings.Contains(out, "func Run") {
		t.Errorf("anchored pattern must match runHelper only:\n%s", out)
	}
	out, _ = callAst(t, root, `{"pattern":"(?i)^run"}`)
	if !strings.Contains(out, "func Run") || !strings.Contains(out, "func runHelper") {
		t.Errorf("case-insensitive anchored pattern must match both:\n%s", out)
	}
}

// vendor/testdata pruned; non-.go files ignored; glob filter applies to base names.
func TestAst_PruneAndGlob(t *testing.T) {
	root := t.TempDir()
	writeGo(t, root, "a.go", sampleGo)
	writeGo(t, root, "vendor/v.go", "package v\nfunc Vendored() {}\n")
	writeGo(t, root, "testdata/t.go", "package t\nfunc Data() {}\n")
	writeGo(t, root, "notgo.txt", "func TextFile() {}\n")
	writeGo(t, root, "b_test.go", "package p\nfunc Tested() {}\n")

	out, _ := callAst(t, root, `{"pattern":"."}`)
	for _, banned := range []string{"Vendored", "Data", "TextFile"} {
		if strings.Contains(out, banned) {
			t.Errorf("%s must not appear:\n%s", banned, out)
		}
	}
	if !strings.Contains(out, "func Tested") {
		t.Errorf("b_test.go is project code and must be searched:\n%s", out)
	}

	out, _ = callAst(t, root, `{"pattern":".","glob":"*_test.go"}`)
	if !strings.Contains(out, "func Tested") || strings.Contains(out, "func Run") {
		t.Errorf("glob must restrict to _test.go files:\n%s", out)
	}
}

// Parse errors skip the file with a tail note, but partial AST matches survive (parser recovery).
func TestAst_ParseErrorSkipsFile(t *testing.T) {
	root := t.TempDir()
	writeGo(t, root, "good.go", sampleGo)
	writeGo(t, root, "bad.go", "package p\nfunc Broken( {\n")

	out, isErr := callAst(t, root, `{"pattern":"."}`)
	if isErr {
		t.Fatalf("a parse error in one file must not fail the search: %s", out)
	}
	if !strings.Contains(out, "func Run") {
		t.Errorf("good file must still match:\n%s", out)
	}
	if !strings.Contains(out, "1 file(s) skipped: parse errors") {
		t.Errorf("skip note missing:\n%s", out)
	}
}

// Match cap: over maxAstMatches matches, collection stops and notes truncation.
func TestAst_MatchCap(t *testing.T) {
	root := t.TempDir()
	for i := range 60 {
		name := filepath.Join("d", string(rune('a'+i%26))+string(rune('a'+i/26))+".go")
		writeGo(t, root, name, "package p\n"+strings.Repeat("func F() {}\n", 10))
	}
	out, _ := callAst(t, root, `{"pattern":"."}`)
	if !strings.Contains(out, "over 500 matches, collection stopped") {
		t.Errorf("truncation note missing (output len %d):\n%.200s", len(out), out)
	}
	if n := strings.Count(out, "\nfunc F"); n > 500 {
		t.Errorf("collected %d match lines, cap is 500", n)
	}
}

// Argument validation: missing pattern, bad regex, bad JSON, complexity cap.
func TestAst_ArgumentValidation(t *testing.T) {
	root := t.TempDir()
	writeGo(t, root, "a.go", sampleGo)
	if _, isErr := callAst(t, root, `{}`); !isErr {
		t.Error("missing pattern must error")
	}
	if _, isErr := callAst(t, root, `{"pattern":"["}`); !isErr {
		t.Error("invalid regex must error")
	}
	if _, isErr := callAst(t, root, `not json`); !isErr {
		t.Error("invalid JSON must error")
	}
	// Grep's complexity cap is shared: a deeply nested regex (each paren level costs nodes) is rejected.
	if _, isErr := callAst(t, root, `{"pattern":"`+strings.Repeat("(", 100)+"a"+strings.Repeat(")", 100)+`"}`); !isErr {
		t.Error("complexity-capped regex must error")
	}
}

// Multi-file: results sorted by file path then line.
func TestAst_MultiFileOrdering(t *testing.T) {
	root := t.TempDir()
	writeGo(t, root, "b/second.go", "package b\nfunc Later() {}\n")
	writeGo(t, root, "a/first.go", "package a\nfunc Earlier() {}\n")
	writeGo(t, root, "a/mid.go", "package a\nfunc Middle() {}\n")
	out, _ := callAst(t, root, `{"pattern":"."}`)
	iA, iM, iB := strings.Index(out, "a/first.go"), strings.Index(out, "a/mid.go"), strings.Index(out, "b/second.go")
	if iA < 0 || iM < 0 || iB < 0 {
		t.Fatalf("missing matches:\n%s", out)
	}
	if iA >= iM || iM >= iB {
		t.Errorf("output must be sorted by file then line:\n%s", out)
	}
}

// no matches → "no matches"; missing root → error (a typo'd path must not look like "no matches").
func TestAst_NoMatchAndBadRoot(t *testing.T) {
	root := t.TempDir()
	writeGo(t, root, "a.go", sampleGo)
	if out, _ := callAst(t, root, `{"pattern":"zzzz"}`); out != "no matches" {
		t.Errorf("want 'no matches', got %q", out)
	}
	if _, isErr := callAst(t, root, `{"pattern":".","path":"no/such/dir"}`); !isErr {
		t.Error("missing root must error, not 'no matches'")
	}
}
