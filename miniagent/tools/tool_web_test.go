package tools

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	miniagent "github.com/justphantom/miniagent/miniagent"
)

func newWebTool(t *testing.T) miniagent.Tool {
	t.Helper()
	return WebTool(0, 0, 0, true)
}

func callWeb(t *testing.T, tl miniagent.Tool, args string) miniagent.ToolResult {
	t.Helper()
	return tl.Call(context.Background(), args)
}

func TestWeb_UserAgent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ua := r.Header.Get("User-Agent")
		if want := miniagent.UserAgent(); ua != want {
			t.Errorf("User-Agent = %q, want %q", ua, want)
		}
	}))
	defer srv.Close()
	res := callWeb(t, newWebTool(t), `{"url":"`+srv.URL+`"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
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
