package miniagent

import (
	"io"
	"net/http"
	"strings"
)

// fakeTransport replays preset non-streaming JSON bodies in call order. lastBody
// records the most recent request body, bodies records all of them for multi-step
// assertions, and calls accumulates the number of invocations.
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

// recordingTransport replays preset (status, body) pairs in order and records each request body.
type recordingTransport struct {
	plan   []transportResp
	bodies []string
	calls  int
}

type transportResp struct {
	status int
	body   string
}

func (r *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil {
		b, _ := io.ReadAll(req.Body)
		r.bodies = append(r.bodies, string(b))
		_ = req.Body.Close()
	}
	idx := r.calls
	r.calls++
	resp := transportResp{status: http.StatusOK, body: ""}
	if idx < len(r.plan) {
		resp = r.plan[idx]
	}
	return &http.Response{
		StatusCode: resp.status,
		Body:       io.NopCloser(strings.NewReader(resp.body)),
		Header:     make(http.Header),
		Request:    req,
	}, nil
}
