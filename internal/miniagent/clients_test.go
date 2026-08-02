package miniagent

import "net/http"

// testClients 从一个 RoundTripper 构造 ChatClient + StreamClient（ChatURL=http://localhost），
// 供 Run 测试复用（P4 拆分后 Run 需两个 client：非流式用 chat，流式用 stream）。
func testClients(tr http.RoundTripper) (*ChatClient, *StreamClient) {
	return &ChatClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}},
		&StreamClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
}
