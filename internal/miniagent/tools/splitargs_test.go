package tools

import (
	"reflect"
	"strings"
	"testing"
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
