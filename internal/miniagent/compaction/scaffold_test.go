package compaction

import (
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/justphantom/miniagent/internal/provider/openai"
)

// This file provides shared scaffolding for the compaction package tests (equivalent to the same-named helper in
// the core loop_test, kept independent to avoid cross-package cycles).

// fakeTransport replays the preset non-streaming JSON bodies in call order; lastBody/bodies record the request bodies for assertions.
type fakeTransport struct {
	responses []string
	statuses  []int
	calls     int
	lastBody  string
	bodies    []string
}

func (f *fakeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		b, _ := io.ReadAll(req.Body)
		f.lastBody = string(b)
		f.bodies = append(f.bodies, string(b))
		_ = req.Body.Close()
	}
	idx := f.calls
	f.calls++
	status := http.StatusOK
	if idx < len(f.statuses) {
		status = f.statuses[idx]
	}
	body := ""
	if idx < len(f.responses) {
		body = f.responses[idx]
	}
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
		Request:    req,
	}, nil
}

// textResponse builds a non-streaming chat completions JSON: a single choice, plain-text reply.
func textResponse(text string) string {
	return fmt.Sprintf(`{"choices":[{"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`, text)
}

// testClients builds a ChatClient + StreamClient from a RoundTripper (ChatURL=http://localhost).
func testClients(tr http.RoundTripper) (*openai.ChatClient, *openai.StreamClient) {
	return &openai.ChatClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}},
		&openai.StreamClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
}
