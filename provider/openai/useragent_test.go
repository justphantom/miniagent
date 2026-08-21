package openai

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/justphantom/miniagent/miniagent"
)

// All provider requests carry User-Agent "miniagent/{miniagent.Version}"; config Headers can override UA
// (some gateways require a specific UA), but cannot override Authorization/Content-Type.
func TestUserAgent_AllRequests(t *testing.T) {
	const sse = `data: {"choices":[{"delta":{"content":"Hi"}}]}
data: [DONE]
`
	var gotUA, gotModelUA, gotStreamUA, overriddenUA string
	mux := http.NewServeMux()
	mux.HandleFunc("POST /chat", func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	})
	mux.HandleFunc("GET /models", func(w http.ResponseWriter, r *http.Request) {
		gotModelUA = r.Header.Get("User-Agent")
		fmt.Fprint(w, `{"data":[]}`)
	})
	mux.HandleFunc("POST /stream", func(w http.ResponseWriter, r *http.Request) {
		gotStreamUA = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, sse)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	want := miniagent.UserAgent()
	c := &ChatClient{APIKey: "sk", ChatURL: srv.URL + "/chat", ModelsURL: srv.URL + "/models", HTTP: &http.Client{}}
	if _, err := c.Do(context.Background(), miniagent.Request{Model: "m"}); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if _, err := c.ListModels(context.Background()); err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	sc := &StreamClient{APIKey: "sk", ChatURL: srv.URL + "/stream"}
	if _, err := sc.DoStream(context.Background(), miniagent.Request{Model: "m"}, func(miniagent.Delta) error { return nil }); err != nil {
		t.Fatalf("DoStream: %v", err)
	}
	if gotUA != want || gotModelUA != want || gotStreamUA != want {
		t.Errorf("UA chat=%q models=%q stream=%q, want %q", gotUA, gotModelUA, gotStreamUA, want)
	}

	// Config Headers can override UA.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		overriddenUA = r.Header.Get("User-Agent")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer srv2.Close()
	c2 := &ChatClient{APIKey: "sk", ChatURL: srv2.URL, Headers: map[string]string{"User-Agent": "gateway-needs-this"}, HTTP: &http.Client{}}
	if _, err := c2.Do(context.Background(), miniagent.Request{Model: "m"}); err != nil {
		t.Fatalf("Do with UA header: %v", err)
	}
	if overriddenUA != "gateway-needs-this" {
		t.Errorf("overridden UA = %q", overriddenUA)
	}
}
