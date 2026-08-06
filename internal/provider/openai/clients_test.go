package openai

import (
	"io"
	"net/http"
	"strings"
)

// testClients 从一个 RoundTripper 构造 ChatClient + StreamClient（ChatURL=http://localhost），
// 供 Run 测试复用（P4 拆分后 Run 需两个 client：非流式用 chat，流式用 stream）。
func testClients(tr http.RoundTripper) (*ChatClient, *StreamClient) {
	return &ChatClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}},
		&StreamClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
}

// fakeTransport 把预设的非流式 JSON body 按调用顺序回放（定义从 miniagent 包测试迁入本包：
// 原定义在 miniagent/loop_test.go 且未导出，跨包 _test.go 不可访问，须在此重声明以编译）。
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
