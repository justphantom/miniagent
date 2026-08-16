package tools

import (
	"reflect"
	"strings"
	"testing"

	miniagent "github.com/justphantom/miniagent/internal/miniagent"
)

// splitArgs 是 git/go/npm/lint args 的唯一引号层（argv 直传 exec），锁住分词语义。
func TestSplitArgs(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		// plain whitespace
		{``, nil},
		{`   `, nil},
		{`-m feat`, []string{"-m", "feat"}},
		// double quotes keep spaces
		{`-m "feat: add thing"`, []string{"-m", "feat: add thing"}},
		// single quotes keep spaces
		{`-m 'feat: add thing'`, []string{"-m", "feat: add thing"}},
		// empty quoted word survives as empty arg
		{`-m ""`, []string{"-m", ""}},
		// backslash escape outside quotes glues words
		{`one\ two three`, []string{"one two", "three"}},
		// backslash inside double quotes escapes next char, stays two chars otherwise
		{`"a\\b"`, []string{"a\\b"}},
		{`"a\nb"`, []string{"a\\nb"}}, // literal chars, not newline — no interpolation
		// adjacent segments concatenate (POSIX sh semantics)
		{`a"b c"d`, []string{"ab cd"}},
		{`-m "feat: ""x"" thing"`, []string{"-m", "feat: x thing"}},
		// tabs/newlines are separators too
		{"-m\ta\nb", []string{"-m", "a", "b"}},
		// unterminated quote: remainder becomes one word, no error
		{`-m "open ended`, []string{"-m", "open ended"}},
		{`-m 'open ended`, []string{"-m", "open ended"}},
		// trailing backslash at end of input: dropped, not a panic
		{`abc\`, []string{"abc"}},
		// unicode survives
		{`-m "修复：解析 bug"`, []string{"-m", "修复：解析 bug"}},
	}
	for _, c := range cases {
		if got := splitArgs(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("splitArgs(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// splitArgsStrict：未闭合引号报错（曾静默吞并后续 token：--grep=won't --oneline 成单参数），
// 引号内的空白/另一类引号保留字面量。
func TestSplitArgsStrict(t *testing.T) {
	cases := []struct {
		in        string
		want      []string
		wantErrOn string
	}{
		{`-m "feat: x"`, []string{"-m", "feat: x"}, ""},
		{`--grep='won''t' -F`, []string{"--grep=wont", "-F"}, ""},
		{`-m "it's here"`, []string{"-m", "it's here"}, ""},
		{`-m 'say "hi"'`, []string{"-m", `say "hi"`}, ""},
		{`--grep=won't --oneline`, nil, "unterminated '"},
		{`-m "open ended`, nil, `unterminated "`},
	}
	for _, c := range cases {
		got, err := splitArgsStrict(c.in)
		if c.wantErrOn != "" {
			if err == "" || !strings.Contains(err, c.wantErrOn) {
				t.Errorf("splitArgsStrict(%q) err = %q, want containing %q", c.in, err, c.wantErrOn)
			}
			continue
		}
		if err != "" {
			t.Errorf("splitArgsStrict(%q) unexpected err: %q", c.in, err)
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("splitArgsStrict(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// POSIX line continuation: backslash+newline (LF or CRLF) is removed entirely — no token character,
// no word break. The LLM writes `... \<LF>--flag` when formatting long args; the old code emitted a
// token starting with the raw newline, which the deny matcher (dash-prefixed tokens only) never inspected.
func TestSplitArgs_LineContinuation(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"--author=Phantom \\\n--oneline", []string{"--author=Phantom", "--oneline"}},
		{"install \\\n--no-audit", []string{"install", "--no-audit"}},
		// CRLF: both \r and \n consumed (the old code left a stray "\r" positional)
		{"x \\\r\n--y", []string{"x", "--y"}},
		// continuation glues the surrounding text into ONE word
		{"one\\\ntwo", []string{"onetwo"}},
		// inside double quotes POSIX also removes \<newline>
		{`-m "a\` + "\n" + `b"`, []string{"-m", "ab"}},
		// escaped ordinary chars keep the old semantics
		{`one\ two`, []string{"one two"}},
		// escaped newline inside single quotes stays literal (no escapes there)
		{`-m 'a\` + "\n" + `b'`, []string{"-m", "a\\\nb"}},
	}
	for _, c := range cases {
		if got := splitArgs(c.in); !reflect.DeepEqual(got, c.want) {
			t.Errorf("splitArgs(%q) = %q, want %q", c.in, got, c.want)
		}
		gotStrict, err := splitArgsStrict(c.in)
		if err != "" {
			t.Errorf("splitArgsStrict(%q) unexpected err: %q", c.in, err)
			continue
		}
		if !reflect.DeepEqual(gotStrict, c.want) {
			t.Errorf("splitArgsStrict(%q) = %q, want %q", c.in, gotStrict, c.want)
		}
	}
	// no token may carry an embedded newline anymore
	for _, in := range []string{"a \\\n b", "a \\\r\n b", `-m "x\` + "\n" + `y"`} {
		for _, tok := range splitArgs(in) {
			if strings.ContainsAny(tok, "\n\r") {
				t.Errorf("splitArgs(%q) produced token %q with an embedded newline", in, tok)
			}
		}
	}
}

// decodeStrict must reject trailing data after the JSON object: providers emit duplicated/concatenated
// tool-call payloads under retry/fragmentation, and silently keeping only the first object would make
// the LLM believe both invocations executed.
func TestDecodeStrict_TrailingData(t *testing.T) {
	var a struct {
		Sub string `json:"subcommand"`
	}
	if err := decodeStrict(`{"subcommand":"status"} trailing garbage`, &a); err == nil {
		t.Error("trailing garbage should be rejected")
	}
	if err := decodeStrict(`{"subcommand":"a"}{"subcommand":"b"}`, &a); err == nil {
		t.Error("a second concatenated object should be rejected")
	}
	if err := decodeStrict(`{"subcommand":"status"}`, &a); err != nil {
		t.Errorf("exact single object rejected: %v", err)
	}
	if a.Sub != "status" {
		t.Errorf("decoded subcommand = %q, want status", a.Sub)
	}
}

// denyResult carries ExitCodeNotSet: no command ran, and the zero value 0 would be misread by the
// event layer as success (P3-4).
func TestDenyResult_ExitCodeNotSet(t *testing.T) {
	r := denyResult("go %q is not allowed", "run")
	if !r.IsError {
		t.Fatal("denyResult must set IsError")
	}
	if r.ExitCode != miniagent.ExitCodeNotSet {
		t.Errorf("ExitCode = %d, want %d", r.ExitCode, miniagent.ExitCodeNotSet)
	}
	if r.Output == "" {
		t.Error("denyResult must format the output message")
	}
}
