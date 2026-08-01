package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// ctxExitCode 是 interact 退出码判定的纯函数（不经 stdin/exit），直接表测覆盖三条规则：
// nil→(0,false) 不触发返回；Canceled→130；DeadlineExceeded→1；其他 ctx 错误→1；wrap 透传。
func TestCtxExitCode(t *testing.T) {
	cases := []struct {
		name     string
		ctxErr   error
		wantCode int
		wantOk   bool
	}{
		{"nil 不返码", nil, 0, false},
		{"Canceled→130", context.Canceled, 130, true},
		{"DeadlineExceeded→1", context.DeadlineExceeded, 1, true},
		{"wrapped Canceled 透传→130", fmt.Errorf("ctx: %w", context.Canceled), 130, true},
		{"wrapped DeadlineExceeded 透传→1", fmt.Errorf("ctx: %w", context.DeadlineExceeded), 1, true},
		{"其他 ctx 错误→1", errors.New("boom"), 1, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, ok := ctxExitCode(c.ctxErr)
			if code != c.wantCode || ok != c.wantOk {
				t.Errorf("ctxExitCode(%v) = (%d, %v), want (%d, %v)", c.ctxErr, code, ok, c.wantCode, c.wantOk)
			}
		})
	}
}

// eofExitCode：无失败轮→0（干净 EOF）；存在失败轮→1。
func TestEofExitCode(t *testing.T) {
	if got := eofExitCode(false); got != 0 {
		t.Errorf("eofExitCode(false) = %d, want 0", got)
	}
	if got := eofExitCode(true); got != 1 {
		t.Errorf("eofExitCode(true) = %d, want 1", got)
	}
}

// P3 eof-on-error 退出码：单轮 Run 失败（500）后 stdin EOF，应 exit 1（hadErr 累积）。
// 修复前此处返回 0（err 分支 eof 直接 return 0），漏报失败。
func TestCLI_InteractiveErrorThenEOFExits1(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	// "q\n"：首轮 readTurn 返 ("q", false)；次轮 read 读空 EOF 返 ("", true) 触发 eof 退出。
	code, out := runMainBin(t, "q\n", chatArgs(srv.URL, "-interactive"), "MINIAGENT_API_KEY=sk-test")
	if code != 1 {
		t.Errorf("code = %d, want 1（EOF 前有 Run 失败轮应 exit 1）; out=%s", code, out)
	}
	if !strings.Contains(out, `"type":"error"`) {
		t.Errorf("失败轮应 emit error 事件: %s", out)
	}
}

// P3 -max-duration 识别 DeadlineExceeded：短 deadline + 慢响应，到期后 ctx.Err()=DeadlineExceeded
// → 干净 exit 1，不 emit error（与 SIGINT 干净退出语义对齐）。修复前每次输入报 deadline error
// 却 continue，循环不退。慢响应给 10x 余量抗 -race 负载，确保 deadline 先于 HTTP 响应触发。
func TestCLI_InteractiveMaxDurationExits1(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		time.Sleep(5 * time.Second)
	}))
	defer srv.Close()
	// 200ms deadline：子进程进入 Run 的 HTTP 阻塞后 ~200ms 到期，ctx.Err()=DeadlineExceeded。
	code, out := runMainBin(t, "q\n", chatArgs(srv.URL, "-interactive", "-max-duration", "200ms"), "MINIAGENT_API_KEY=sk-test")
	if code != 1 {
		t.Errorf("code = %d, want 1（-max-duration 到期 DeadlineExceeded→exit 1）; out=%s", code, out)
	}
	// DeadlineExceeded 走干净退出，不应 emit error NDJSON 事件（区别于真故障）。
	if strings.Contains(out, `"type":"error"`) {
		t.Errorf("-max-duration 到期不应 emit error 事件: %s", out)
	}
}
