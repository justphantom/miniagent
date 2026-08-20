package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	miniagent "github.com/justphantom/miniagent/miniagent"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/justphantom/miniagent/text"
)

const maxAstMatches = 500

// astKinds maps the kind filter values to the GoSpecs entry kinds they select.
// interface and struct are refinements of type (GenDecl with TypeSpec), listed
// separately because "find all interfaces" is a distinct query from "find all types".
var astKinds = map[string]bool{
	"func": true, "method": true, "type": true,
	"interface": true, "struct": true, "const": true, "var": true,
}

type astArgs struct {
	Pattern string `json:"pattern"`
	Kind    string `json:"kind,omitempty"`
	Path    string `json:"path,omitempty"`
	Glob    string `json:"glob,omitempty"`
}

type astMatch struct {
	file string
	line int
	text string
}

// AstTool recursively searches Go source files for symbol DECLARATIONS by name
// (functions, methods, types, interfaces, structs, consts, vars) via go/parser.
// Lexical name matching only — it finds definitions, not references; reference
// search stays with grep. timeout<=0 uses the default fileOpTimeout.
func AstTool(workspaceRoot string, timeout time.Duration, maxOutputChars int) miniagent.Tool {
	if timeout <= 0 {
		timeout = fileOpTimeout
	}
	if maxOutputChars <= 0 {
		maxOutputChars = maxShellOutputChars
	}
	return miniagent.Tool{
		Name:        "ast",
		Description: "Recursively searches Go source files for symbol declarations by name (parser-based AST, immune to comments/strings). Outputs path:lineno:kind Name, methods as 'method (R) Name'. Kinds: " + sortedNames(astKinds) + " (interface/struct refine type). Pattern is a Go regexp matched against the symbol name. Finds definitions, not references — use grep for those. Match limit " + strconv.Itoa(maxAstMatches) + ". Skips .git, vendor, testdata, non-.go files.",
		Parameters: object(map[string]any{
			"pattern": map[string]any{"type": "string", "description": "A Go regexp matched against symbol names, e.g. Tool, ^Run, (?i)client"},
			"kind":    map[string]any{"type": "string", "description": "Filter by declaration kind: " + sortedNames(astKinds)},
			"path":    map[string]any{"type": "string", "description": "Search root directory, relative to workdir or absolute, defaults to workdir"},
			"glob":    map[string]any{"type": "string", "description": "File-name include filter, a filepath.Match glob (e.g. *_test.go)"},
		}, "pattern"),
		ResultLimit: miniagent.MaxToolResultInHistory,
		Call: func(ctx context.Context, args string) miniagent.ToolResult {
			return runWithTimeout(ctx, timeout, "ast", func(rctx context.Context) miniagent.ToolResult {
				return runAst(rctx, workspaceRoot, args, maxOutputChars)
			})
		},
	}
}

func runAst(ctx context.Context, workspaceRoot, args string, maxOutputChars int) miniagent.ToolResult {
	var a astArgs
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("argument parsing failed: %v (received %q)", err, args)}
	}
	if strings.TrimSpace(a.Pattern) == "" {
		return miniagent.ToolResult{IsError: true, Output: "missing argument: pattern"}
	}
	if err := validateGrepPattern(a.Pattern); err != nil {
		return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("invalid pattern: %v", err)}
	}
	re, err := regexp.Compile(a.Pattern)
	if err != nil {
		return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("invalid regex: %v", err)}
	}
	kinds, err := parseAstKinds(a.Kind)
	if err != nil {
		return miniagent.ToolResult{IsError: true, Output: err.Error()}
	}
	root := resolveToolPath(workspaceRoot, a.Path)
	matches, skipped, truncated, err := astWalk(ctx, root, re, a.Glob, kinds)
	if err != nil {
		return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("ast %q failed: %v", a.Path, err)}
	}
	if len(matches) == 0 {
		return miniagent.ToolResult{Output: "no matches"}
	}
	var sb strings.Builder
	for _, m := range matches {
		fmt.Fprintf(&sb, "%s:%d:%s\n", m.file, m.line, m.text)
	}
	out := text.Truncate(sb.String(), maxOutputChars, "…[ast output truncated]")
	if truncated {
		out += fmt.Sprintf("\n…(over %d matches, collection stopped)", maxAstMatches)
	}
	if skipped > 0 {
		out += fmt.Sprintf("\n(%d file(s) skipped: parse errors)", skipped)
	}
	return miniagent.ToolResult{Output: out}
}

// parseAstKinds validates the kind filter: empty means all kinds; multiple kinds
// are comma-separated. interface/struct imply type, so selecting type alone does
// NOT select them and selecting them does not drag type in — each is explicit.
func parseAstKinds(kind string) (map[string]bool, error) {
	if strings.TrimSpace(kind) == "" {
		return astKinds, nil
	}
	kinds := map[string]bool{}
	for k := range strings.SplitSeq(kind, ",") {
		k = strings.TrimSpace(k)
		if !astKinds[k] {
			return nil, fmt.Errorf("invalid kind %q; permitted: %s", k, sortedNames(astKinds))
		}
		kinds[k] = true
	}
	return kinds, nil
}

// astWalk walks root and parses each .go file, collecting declaration matches.
// Walk rules mirror grep: root errors propagate, deeper errors skip; .git and
// symlinked directories are pruned. vendor and testdata are pruned too — search
// targets the project's own code, and parsing the dependency tree multiplies
// work and matches for declarations the model rarely asks about.
func astWalk(ctx context.Context, root string, re *regexp.Regexp, glob string, kinds map[string]bool) ([]astMatch, int, bool, error) {
	var matches []astMatch
	skipped := 0
	truncated := false
	var globFn func(string) bool
	if strings.TrimSpace(glob) != "" {
		globFn = func(name string) bool {
			ok, _ := filepath.Match(glob, name)
			return ok
		}
	}
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			if path == root {
				return walkErr // typo'd root must not masquerade as "no matches" (aligned with grep/glob)
			}
			return nil //nolint:nilerr // skip inaccessible subtrees, keep results from accessible parts
		}
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == "vendor" || d.Name() == "testdata" || d.Type()&fs.ModeSymlink != 0 {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 || !d.Type().IsRegular() || filepath.Ext(path) != ".go" {
			return nil
		}
		if globFn != nil && !globFn(d.Name()) {
			return nil
		}
		rel := path
		if r, err := filepath.Rel(root, path); err == nil {
			rel = r
		}
		// Per-file match cap mirrors grepFile: a huge generated .go file must not
		// transiently allocate a full-file match slice before global truncation.
		ms, parseErr := astFile(fset, path, rel, re, kinds, maxAstMatches)
		for _, m := range ms {
			if len(matches) >= maxAstMatches {
				truncated = true
				return filepath.SkipAll
			}
			matches = append(matches, m)
		}
		if parseErr != nil {
			skipped++
		}
		return nil
	})
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].file != matches[j].file {
			return matches[i].file < matches[j].file
		}
		return matches[i].line < matches[j].line
	})
	return matches, skipped, truncated, err
}

// astFile parses one file and matches its top-level declarations. Parse errors
// do not fail the search — a half-edited repo still yields the declarations that
// did parse — but the file's partial matches (parser recovers and returns a
// partial AST) are kept and the file is counted as skipped for the tail note.
// maxGrepFileBytes reuses grep's per-file size cap: same "huge file wastes IO" rationale.
func astFile(fset *token.FileSet, path, display string, re *regexp.Regexp, kinds map[string]bool, maxMatches int) ([]astMatch, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, errors.New("not regular, skipped")
	}
	if fi.Size() > maxGrepFileBytes {
		return nil, errors.New("file too large, skipped")
	}
	src, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	f, parseErr := parser.ParseFile(fset, display, src, parser.SkipObjectResolution)
	var matches []astMatch
	add := func(line int, format string, args ...any) {
		if len(matches) < maxMatches {
			matches = append(matches, astMatch{file: display, line: line, text: fmt.Sprintf(format, args...)})
		}
	}
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if d.Recv != nil && len(d.Recv.List) > 0 {
				recv := typeString(d.Recv.List[0].Type)
				if kinds["method"] && re.MatchString(d.Name.Name) {
					add(fset.Position(d.Pos()).Line, "method (%s) %s", recv, d.Name.Name)
				}
				continue
			}
			if kinds["func"] && re.MatchString(d.Name.Name) {
				add(fset.Position(d.Pos()).Line, "func %s", d.Name.Name)
			}
		case *ast.GenDecl:
			specKind := "const"
			switch d.Tok {
			case token.VAR:
				specKind = "var"
			case token.TYPE:
				specKind = "type"
			}
			for _, spec := range d.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if ok {
					for _, name := range vs.Names {
						if kinds[specKind] && re.MatchString(name.Name) {
							add(fset.Position(name.Pos()).Line, "%s %s", specKind, name.Name)
						}
					}
					continue
				}
				ts, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				if !kinds["type"] && !kinds["interface"] && !kinds["struct"] {
					continue
				}
				// interface/struct refine type: naming only type means plain types
				// (struct/interface are excluded unless named or implied by empty kind).
				matched := false
				switch ts.Type.(type) {
				case *ast.InterfaceType:
					matched = kinds["interface"]
				case *ast.StructType:
					matched = kinds["struct"]
				default:
					matched = kinds["type"]
				}
				if matched && re.MatchString(ts.Name.Name) {
					add(fset.Position(ts.Pos()).Line, "%s %s", kindOfType(ts.Type), ts.Name.Name)
				}
			}
		}
	}
	return matches, parseErr
}
