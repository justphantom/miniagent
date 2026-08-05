package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// e2e：会话结束自动抽取记忆。主模型先回 tool_call 再回 final；抽取调用（请求体无 tools）
// 回 JSON 记录，验证 workdir/.miniagent/memory.jsonl 被写入。
func TestCLI_AutoMemoryExtract(t *testing.T) {
	var toolCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		if !strings.Contains(string(body), `"tools"`) {
			// 抽取调用（无 tools 字段）：content 内放 JSON 数组。
			_, _ = fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"[{\"type\":\"note\",\"topic\":\"构建\",\"content\":\"go build 用 ./...\"}]"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
			return
		}
		toolCalls++
		if toolCalls == 1 {
			_, _ = fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"write","arguments":"{\"path\":\"a.txt\",\"content\":\"hello\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
			return
		}
		_, _ = fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer srv.Close()

	workdir := t.TempDir()
	code, out := runMainBin(t, "跑构建并总结", configArgs(t, srv.URL, "-workdir", workdir), "MINIAGENT_API_KEY=sk-test")
	if code != 0 {
		t.Fatalf("code=%d out=%s", code, out)
	}
	data, err := os.ReadFile(filepath.Join(workdir, ".miniagent", "memory.jsonl"))
	if err != nil {
		t.Fatalf("memory file not written: %v\nout=%s", err, out)
	}
	if !strings.Contains(string(data), "go build 用 ./...") {
		t.Errorf("expected extracted memory, got: %s", data)
	}
}
