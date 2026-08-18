package responses

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/justphantom/miniagent/internal/miniagent"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func testClient(fn roundTripFunc) *Client {
	return &Client{APIKey: "sk", Endpoint: "https://api.test/v1/responses", HTTP: &http.Client{Transport: fn}}
}

func TestClient_DoRequestAndParse(t *testing.T) {
	var gotPath, auth, contentType, custom string
	client := testClient(func(req *http.Request) (*http.Response, error) {
		gotPath = req.URL.Path
		auth = req.Header.Get("Authorization")
		contentType = req.Header.Get("Content-Type")
		custom = req.Header.Get("X-Custom")
		body, _ := io.ReadAll(req.Body)
		if !strings.Contains(string(body), `"input"`) {
			t.Errorf("request body is not Responses shape: %s", body)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(responseBody())), Header: http.Header{}, Request: req}, nil
	})
	client.Headers = map[string]string{"X-Custom": "yes", "Authorization": "must-not-override", "Content-Type": "text/plain"}
	resp, err := client.Do(context.Background(), miniagent.Request{Model: "m", Messages: []miniagent.Message{{Role: miniagent.RoleUser, Content: "q"}}})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/responses" || auth != "Bearer sk" || contentType != "application/json" || custom != "yes" {
		t.Errorf("path/auth/content/custom = %q/%q/%q/%q", gotPath, auth, contentType, custom)
	}
	if resp.Text != "answer" || len(resp.ToolCalls) != 1 {
		t.Errorf("resp = %+v", resp)
	}
}

func TestClient_ErrorSentinels(t *testing.T) {
	client := testClient(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusBadRequest, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"context_length_exceeded"}}`)), Header: http.Header{}, Request: req}, nil
	})
	_, err := client.Do(context.Background(), miniagent.Request{Model: "m"})
	if !errors.Is(err, miniagent.ErrContextLength) {
		t.Fatalf("err = %v, want context length", err)
	}
}
