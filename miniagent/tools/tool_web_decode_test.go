package tools

import (
	"strings"
	"testing"
)

func TestDecodeWebBody_UTF16LEBOM(t *testing.T) {
	body := "\xFF\xFEh\000i\000"
	out, warn := decodeWebBody("text/html", body)
	if warn != "" {
		t.Errorf("want no warning, got %q", warn)
	}
	if out != "hi" {
		t.Errorf("want %q, got %q", "hi", out)
	}
}

func TestDecodeWebBody_Latin1(t *testing.T) {
	body := "caf\xe9" // \e9 = é in latin-1
	out, warn := decodeWebBody("text/plain; charset=iso-8859-1", body)
	if warn != "" {
		t.Errorf("want no warning for latin-1, got %q", warn)
	}
	if out != "café" {
		t.Errorf("want %q, got %q", "café", out)
	}
}

func TestDecodeWebBody_UnknownCharsetWarns(t *testing.T) {
	body := "\xc4\xe3\xba\xc3" // GBK "你好" — not valid UTF-8
	out, warn := decodeWebBody("text/html; charset=gbk", body)
	if warn == "" {
		t.Errorf("want warning for GBK passthrough, got clean output %q", out)
	}
	if out != body {
		t.Errorf("want raw passthrough, got transformed %q", out)
	}
}

func TestDecodeWebBody_MetaCharset(t *testing.T) {
	html := "<html><head><meta charset=\"iso-8859-1\"></head><body>caf\xe9</body></html>"
	out, warn := decodeWebBody("text/html", html)
	if warn != "" {
		t.Errorf("want no warning for meta latin-1, got %q", warn)
	}
	if !strings.Contains(out, "café") {
		t.Errorf("meta charset not applied: %q", out)
	}
}

func TestDecodeWebBody_NoCharsetInvalidUTF8(t *testing.T) {
	body := "\xe9\xe0\xff"
	out, warn := decodeWebBody("text/plain", body)
	if warn == "" || !strings.Contains(warn, "latin-1") {
		t.Errorf("want latin-1 warning, got %q", warn)
	}
	if got := []rune(out); len(got) != len([]byte(body)) {
		t.Errorf("latin-1 rune count mismatch: %d runes for %d bytes", len(got), len(body))
	}
}

func TestDecodeWebBody_DeclaredGBButValidUTF8(t *testing.T) {
	body := "你好"
	out, warn := decodeWebBody("text/html; charset=gbk", body)
	if warn == "" {
		t.Error("want warning for mislabeled UTF-8, got none")
	}
	if out != body {
		t.Errorf("want passthrough, got %q", out)
	}
}

func TestWebToText_CollapsesBlankLines(t *testing.T) {
	got := webToText("<p>a</p>\n\n\n\n<p>b</p>", true)
	if got != "a\nb" {
		t.Errorf("blank-line collapse: %q", got)
	}
}

func TestDecodeEntities_NumericForms(t *testing.T) {
	if got := decodeEntities("&#65;&#x42;&unknown;"); got != "AB&unknown;" {
		t.Errorf("numeric entities: %q", got)
	}
}
