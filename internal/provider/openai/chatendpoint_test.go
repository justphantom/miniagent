package openai

import (
	"net/url"
	"sync"
	"testing"
	"time"
)

// 缓存 parse：chatEndpoint 多次调用返回同一 *url.URL（不每请求重做，审查 v3 #10）。
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

// 并发懒解析（直接 struct 构造、chatURL 未缓存）不应数据竞争（sync.Once 保护，修复 R4）。
// go test -race 下验证：多 goroutine 首次触发的懒解析无竞争，且都返回同一缓存指针。
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
