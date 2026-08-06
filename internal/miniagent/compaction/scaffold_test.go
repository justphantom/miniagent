package compaction

import (
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/justphantom/miniagent/internal/provider/openai"
)

// 本文件为压缩包测试提供共享脚手架（与核心 loop_test 的同名 helper 等价，独立以避免跨包循环）。

// fakeTransport 把预设的非流式 JSON body 按调用顺序回放；lastBody/bodies 记录请求体供断言。
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

// textResponse 构造非流式 chat completions JSON：单条 choice，纯文本回复。
func textResponse(text string) string {
	return fmt.Sprintf(`{"choices":[{"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`, text)
}

// testClients 从一个 RoundTripper 构造 ChatClient + StreamClient（ChatURL=http://localhost）。
func testClients(tr http.RoundTripper) (*openai.ChatClient, *openai.StreamClient) {
	return &openai.ChatClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}},
		&openai.StreamClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
}
