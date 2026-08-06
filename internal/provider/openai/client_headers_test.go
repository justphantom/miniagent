package openai

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/justphantom/miniagent/internal/miniagent"
)

// 自定义请求头：Headers 中的键值会随请求发送，不覆盖 Authorization / Content-Type。
func TestChatClient_Do_CustomHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Custom") != "abc" {
			t.Errorf("X-Custom = %q, want abc", r.Header.Get("X-Custom"))
		}
		if r.Header.Get("Authorization") != "Bearer sk" {
			t.Errorf("Authorization = %q, want Bearer sk", r.Header.Get("Authorization"))
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("Content-Type = %q", r.Header.Get("Content-Type"))
		}
		_, _ = fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer srv.Close()
	c := &ChatClient{
		APIKey:  "sk",
		ChatURL: srv.URL,
		HTTP:    &http.Client{Timeout: 5 * time.Second},
		Headers: map[string]string{"X-Custom": "abc"},
	}
	resp, err := c.Do(context.Background(), miniagent.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if resp.Text != "ok" {
		t.Errorf("Text = %q", resp.Text)
	}
}

// 空 Headers：不带自定义头，Authorization / Content-Type 仍然正确。
func TestChatClient_Do_NoCustomHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := r.Header["X-Custom"]; ok {
			t.Error("X-Custom should not be set")
		}
		_, _ = fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer srv.Close()
	c := &ChatClient{APIKey: "sk", ChatURL: srv.URL, HTTP: &http.Client{Timeout: 5 * time.Second}}
	_, err := c.Do(context.Background(), miniagent.Request{Model: "m"})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
}
