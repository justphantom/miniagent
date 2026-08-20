package openai

import (
	"io"
	"net/http"
	"strings"
)

// testClients builds a ChatClient + StreamClient from a single RoundTripper (ChatURL=http://localhost)
// for Run tests to reuse (after the P4 split Run needs two clients: chat for non-streaming, stream for streaming).
func testClients(tr http.RoundTripper) (*ChatClient, *StreamClient) {
	return &ChatClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}},
		&StreamClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
}

// fakeTransport replays preset non-streaming JSON bodies in call order (definition moved into this
// package from the miniagent package tests: originally defined in miniagent/loop_test.go and
// unexported, inaccessible to a cross-package _test.go, so it must be redeclared here to compile).
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
