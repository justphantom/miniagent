package tools

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	miniagent "github.com/justphantom/miniagent/internal/miniagent"
)

// newWebTool builds a web tool with allowPrivate=true so httptest's 127.0.0.1
// listener passes the SSRF guard; the guard itself is covered separately below.
func newWebTool(t *testing.T) miniagent.Tool {
	t.Helper()
	return WebTool(0, 0, 0, true)
}

func callWeb(t *testing.T, tl miniagent.Tool, args string) miniagent.ToolResult {
	t.Helper()
	return tl.Call(context.Background(), args)
}

func TestWeb_PlainText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("hello docs\nline two\n"))
	}))
	defer srv.Close()
	res := callWeb(t, newWebTool(t), `{"url":"`+srv.URL+`"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if !strings.Contains(res.Output, "hello docs") || !strings.Contains(res.Output, "line two") {
		t.Errorf("output missing body: %q", res.Output)
	}
}

func TestWeb_HTMLStripsScriptStyleTags(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html><html><head><style>.a{color:red}</style><title>T</title></head>
<body><h1>Heading</h1><script>var x="secret()";</script><p>Para &amp; more</p><!-- comment --></body></html>`))
	}))
	defer srv.Close()
	res := callWeb(t, newWebTool(t), `{"url":"`+srv.URL+`"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	for _, banned := range []string{"secret()", "color:red", "<", "</", "script", "style", "comment"} {
		if strings.Contains(res.Output, banned) {
			t.Errorf("output leaked %q: %q", banned, res.Output)
		}
	}
	if !strings.Contains(res.Output, "Heading") || !strings.Contains(res.Output, "Para & more") {
		t.Errorf("output missing visible text: %q", res.Output)
	}
}

func TestWeb_JSONBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"a":1}`))
	}))
	defer srv.Close()
	res := callWeb(t, newWebTool(t), `{"url":"`+srv.URL+`"}`)
	if res.IsError || !strings.Contains(res.Output, `{"a":1}`) {
		t.Errorf("json body mangled: %+v", res)
	}
}

func TestWeb_RedirectChainFollowed(t *testing.T) {
	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("arrived"))
	}))
	defer final.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, final.URL+"/x", http.StatusFound)
	}))
	defer srv.Close()
	res := callWeb(t, newWebTool(t), `{"url":"`+srv.URL+`"}`)
	if res.IsError || !strings.Contains(res.Output, "arrived") {
		t.Errorf("redirect not followed: %+v", res)
	}
}

func TestWeb_HTTPErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer srv.Close()
	res := callWeb(t, newWebTool(t), `{"url":"`+srv.URL+`"}`)
	if !res.IsError || !strings.Contains(res.Output, "404") {
		t.Errorf("want 404 error, got %+v", res)
	}
}

func TestWeb_SchemeAndArgsValidation(t *testing.T) {
	tl := newWebTool(t)
	cases := []struct {
		name, args, wantIn string
	}{
		{"ftp scheme rejected", `{"url":"ftp://example.com/f"}`, "scheme"},
		{"file scheme rejected", `{"url":"file:///etc/passwd"}`, "scheme"},
		{"missing url", `{"url":""}`, "missing argument"},
		{"unknown field rejected", `{"url":"http://example.com","path":"x"}`, "parsing"},
		{"trailing data rejected", `{"url":"http://example.com"}{"url":"http://evil.com"}`, "trailing"},
	}
	for _, c := range cases {
		res := callWeb(t, tl, c.args)
		if !res.IsError {
			t.Errorf("%s: want error", c.name)
		} else if !strings.Contains(res.Output, c.wantIn) {
			t.Errorf("%s: error %q missing %q", c.name, res.Output, c.wantIn)
		}
	}
}

func TestWeb_PublicHostBlockedCases(t *testing.T) {
	// Guard-only tests (no HTTP): the tool with allowPrivate=false must deny
	// loopback / private / link-local targets before any request goes out.
	tl := WebTool(0, 0, 0, false)
	cases := []struct {
		name, host string
	}{
		{"loopback literal", "http://127.0.0.1/x"},
		{"loopback name", "http://localhost/x"},
		{"private v4 literal", "http://10.1.2.3/x"},
		{"private v4 literal 192.168", "http://192.168.0.1/x"},
		{"link-local metadata", "http://169.254.169.254/latest/meta-data/"},
		{"ipv6 loopback", "http://[::1]/x"},
		{"ipv6 unique-local", "http://[fd00::1]/x"},
	}
	for _, c := range cases {
		res := callWeb(t, tl, `{"url":"`+c.host+`"}`)
		if !res.IsError {
			t.Errorf("%s (%s): want block", c.name, c.host)
		} else if !strings.Contains(res.Output, "blocked") && !strings.Contains(res.Output, "resolve") {
			t.Errorf("%s: error %q lacks block reason", c.name, res.Output)
		}
	}
}

func TestCheckWebHost_RejectsLoopbackAndV4Mapped(t *testing.T) {
	// Public-looking first hop (allowPrivate=true only for the first hop via test
	// server), then redirect to a loopback literal: CheckRedirect re-checks and
	// must fail despite allowPrivate... allowPrivate skips redirect checks too,
	// so here we assert the guarded variant rejects via checkWebHost directly.
	if err := checkWebHost(context.Background(), mustParseURL(t, "http://127.0.0.1:9/x"), false); err == nil {
		t.Error("checkWebHost must reject loopback literal")
	}
	if err := checkWebHost(context.Background(), mustParseURL(t, "http://[::ffff:127.0.0.1]/x"), false); err == nil {
		t.Error("checkWebHost must reject v4-mapped loopback")
	}
}

func mustParseURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	return u
}

func TestWeb_BinaryRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte{0x00, 0x01, 0x02, 0x00, 0x03})
	}))
	defer srv.Close()
	res := callWeb(t, newWebTool(t), `{"url":"`+srv.URL+`"}`)
	if !res.IsError || !strings.Contains(res.Output, "binary") {
		t.Errorf("want binary rejection, got %+v", res)
	}
}

func TestWeb_BodyTruncationMarker(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("a", 3000)))
	}))
	defer srv.Close()
	tl := WebTool(0, 2000, 0, true) // maxBytes 2000 < body
	res := callWeb(t, tl, `{"url":"`+srv.URL+`"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if !strings.Contains(res.Output, "truncated") {
		t.Errorf("missing truncation marker: %q", res.Output[:min(len(res.Output), 120)])
	}
}

func TestWeb_OutputCharCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", 100000)))
	}))
	defer srv.Close()
	tl := WebTool(0, 0, 5000, true)
	res := callWeb(t, tl, `{"url":"`+srv.URL+`"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if len([]rune(res.Output)) > 6000 {
		t.Errorf("output %d runes exceeds cap+marker", len([]rune(res.Output)))
	}
}

// TestDecodeWebBody_UTF16LEBOM decodes a body starting with a UTF-16 LE BOM into
// readable UTF-8, verifying the BOM-sniffing path end-to-end.
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

// TestDecodeWebBody_Latin1 verifies declared latin-1 is decoded via identity mapping
// (bytes 0xE0..0xFF become the corresponding runes, not mojibake).
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

// TestDecodeWebBody_UnknownCharsetWarns asserts that an unsupported multi-byte
// charset (GBK) yields a warning rather than silent mojibake.
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

// TestDecodeWebBody_MetaCharset sniffs charset from <meta charset> when absent from
// the Content-Type header.
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

// TestDecodeWebBody_NoCharsetInvalidUTF8 covers the unknown-charset fallback: a
// body that is neither valid UTF-8 nor has a declaration is decoded as latin-1 with
// a warning.
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

// TestDecodeWebBody_DeclaredGBButValidUTF8 guards against a wrong charset
// declaration: a UTF-8 body mislabeled as GBK is used as-is with a warning.
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

// remapPublicDial replaces http.DefaultTransport with one that dials the public literal
// 8.8.8.8 as 127.0.0.1, so httptest servers (bound to 127.0.0.1) can be reached via a
// public-looking first-hop URL — letting checkWebHost pass the first hop while
// CheckRedirect re-validates redirect targets end-to-end. Restored on cleanup.
// Not safe under t.Parallel (mutates a global); these tests do not parallelize.
func remapPublicDial(t *testing.T) {
	t.Helper()
	orig := http.DefaultTransport
	http.DefaultTransport = &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, _ := net.SplitHostPort(addr)
			if host == "8.8.8.8" {
				addr = net.JoinHostPort("127.0.0.1", port)
			}
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
	}
	t.Cleanup(func() { http.DefaultTransport = orig })
}

// TestWeb_RedirectToPrivateLiteralBlocked exercises the CheckRedirect SSRF branch end-to-end:
// first hop is a public IP literal (passes checkWebHost), the server 302s to a loopback
// literal — CheckRedirect must re-validate and reject. Previously only checkWebHost itself
// was unit-tested, leaving the redirect callback's guard unverified.
func TestWeb_RedirectToPrivateLiteralBlocked(t *testing.T) {
	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("should not reach"))
	}))
	defer final.Close()
	finalURL, _ := url.Parse(final.URL)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://127.0.0.1:"+finalURL.Port()+"/secret", http.StatusFound)
	}))
	defer srv.Close()
	srvURL, _ := url.Parse(srv.URL)

	remapPublicDial(t)
	tl := WebTool(0, 0, 0, false) // allowPrivate=false: CheckRedirect re-checks each hop
	res := callWeb(t, tl, `{"url":"http://8.8.8.8:`+srvURL.Port()+`/redirect"}`)
	if !res.IsError {
		t.Fatalf("want redirect-to-private blocked, got: %s", res.Output)
	}
	if !strings.Contains(res.Output, "blocked") {
		t.Errorf("error %q lacks redirect-block reason", res.Output)
	}
	if strings.Contains(res.Output, "should not reach") {
		t.Error("redirect to private target was followed")
	}
}

// TestWeb_RedirectToPublicLiteralFollowed is the positive counterpart: a redirect to a
// public IP literal must be followed (CheckRedirect returns nil), proving the guard does
// not over-block legitimate public hops.
func TestWeb_RedirectToPublicLiteralFollowed(t *testing.T) {
	final := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("arrived"))
	}))
	defer final.Close()
	finalURL, _ := url.Parse(final.URL)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://8.8.8.8:"+finalURL.Port()+"/x", http.StatusFound)
	}))
	defer srv.Close()
	srvURL, _ := url.Parse(srv.URL)

	remapPublicDial(t)
	tl := WebTool(0, 0, 0, false)
	res := callWeb(t, tl, `{"url":"http://8.8.8.8:`+srvURL.Port()+`/redirect"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if !strings.Contains(res.Output, "arrived") {
		t.Errorf("redirect to public literal not followed: %q", res.Output)
	}
}
