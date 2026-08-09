package openai

import (
	"net/url"
	"sync"
	"testing"
	"time"
)

// Cached parse: chatEndpoint returns the same *url.URL across calls (not re-parsed per request, review v3 #10).
func TestChatEndpoint_CachedParse(t *testing.T) {
	c := &ChatClient{ChatURL: "https://api/v1/chat/completions"}
	_, u1, err := c.chatEndpoint(time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_, u2, _ := c.chatEndpoint(time.Second)
	if u1 != u2 {
		t.Error("chatEndpoint should return same cached *url.URL")
	}
}

// Concurrent lazy parsing (direct struct construction, chatURL not cached) must not data-race
// (sync.Once guards it, fixes R4). Verified under go test -race: the lazy parse triggered first by
// multiple goroutines is race-free and all return the same cached pointer.
func TestChatEndpoint_ConcurrentLazyParse(t *testing.T) {
	c := &ChatClient{ChatURL: "https://api/v1/chat/completions"}
	const n = 20
	var wg sync.WaitGroup
	seen := make([]*url.URL, n)
	for i := range seen {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, u, err := c.chatEndpoint(time.Second)
			if err != nil {
				t.Errorf("chatEndpoint: %v", err)
				return
			}
			seen[i] = u
		}(i)
	}
	wg.Wait()
	for i := 1; i < n; i++ {
		if seen[i] != seen[0] {
			t.Error("concurrent chatEndpoint should return same cached *url.URL")
			break
		}
	}
}
