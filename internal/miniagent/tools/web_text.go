package tools

import (
	"strconv"
	"strings"
)

// webToText converts a fetched body to context-friendly text. For HTML: drops script/style
// blocks, strips all tags, decodes a small set of entities, collapses blank-line runs.
// Minimal by design (~40 lines): parser-grade correctness would need x/net/html (the project's
// first third-party dependency); malformed HTML degrades to noisy text, acceptable for a
// best-effort reading tool.
func webToText(body string, isHTML bool) string {
	if !isHTML {
		return strings.TrimRight(body, "\n\r\t ")
	}
	s := body
	// Drop script/style contents entirely (case-insensitive, tolerating attributes).
	for _, tag := range []string{"script", "style"} {
		for {
			start := findTag(s, "<"+tag)
			if start < 0 {
				break
			}
			end := findTag(s[start:], "</"+tag+">")
			if end < 0 {
				s = s[:start]
				break
			}
			s = s[:start] + s[start+end+len(tag)+3:]
		}
	}
	// Strip tags; preserve block boundaries as newlines so words don't glue across elements.
	var b strings.Builder
	b.Grow(len(s))
	inTag := false
	for _, r := range s {
		switch {
		case r == '<':
			inTag = true
		case r == '>':
			if inTag {
				inTag = false
				b.WriteByte('\n')
			}
		case !inTag:
			b.WriteRune(r)
		}
	}
	out := decodeEntities(b.String())
	// Collapse runs of blank lines; trim each line; drop leading/trailing blanks.
	lines := strings.Split(out, "\n")
	kept := lines[:0]
	for _, ln := range lines {
		if ln = strings.TrimSpace(ln); ln != "" {
			kept = append(kept, ln)
		}
	}
	return strings.Join(kept, "\n")
}

// findTag finds a case-insensitive tag open/close marker in s, returning the index of '<'.
func findTag(s, tag string) int {
	return strings.Index(strings.ToLower(s), strings.ToLower(tag))
}

// decodeEntities decodes the common named entities + numeric forms. Not exhaustive by design.
func decodeEntities(s string) string {
	repl := strings.NewReplacer(
		"&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", "\"", "&#39;", "'", "&apos;", "'",
		"&nbsp;", " ", "&mdash;", "—", "&ndash;", "–", "&hellip;", "…",
	)
	s = repl.Replace(s)
	// Numeric: &#NNN; and &#xHH; (leave unknown forms literal).
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] == '&' && i+1 < len(s) && s[i+1] == '#' {
			if end := strings.IndexByte(s[i:], ';'); end > 2 && end <= 10 {
				numStr := s[i+2 : i+end]
				base := 10
				if len(numStr) > 0 && (numStr[0] == 'x' || numStr[0] == 'X') {
					base = 16
					numStr = numStr[1:]
				}
				if n, err := strconv.ParseInt(numStr, base, 32); err == nil && n > 0 && n <= 0x10FFFF {
					b.WriteRune(rune(n))
					i += end + 1
					continue
				}
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}
