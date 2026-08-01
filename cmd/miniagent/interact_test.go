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

// P3-1：管道灌入超过 maxPromptBytes 的无换行输入——readTurn 必须在读取过程中封顶，
// emit 拒绝信号并跳过该轮，绝不当作 prompt 喂给 Run（防无界分配 OOM）。修复前 readTurn 用
// ReadString('\n') 无界读取，会把整段超长内容当 prompt 喂给 Run（旧实现本测试必失败：
// 既无「超过大小上限」拒绝信号，又会产生 result/error 事件）。
func TestCLI_InteractiveOversizedLineRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// 超限输入不应到达 LLM；若到达则返回 200 让旧实现产生 result 事件，便于断言判别。
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	// 超过 maxPromptBytes 的无换行输入：触发读取过程中封顶（drain 到行尾即 EOF）。
	big := strings.Repeat("a", maxPromptBytes+1)
	code, out := runMainBin(t, big, chatArgs(srv.URL, "-interactive"), "MINIAGENT_API_KEY=sk-test")
	// 不 panic：超限行被丢弃 + 干净 EOF，无 Run 失败轮 → exit 0。
	if code != 0 {
		t.Errorf("code = %d, want 0（超限行跳过 + 干净 EOF）; out=%s", code, out)
	}
	// 拒绝信号：NDJSON error 事件含上限提示（新实现独有，旧实现无此串）。
	if !strings.Contains(out, "超过大小上限") {
		t.Errorf("超限行应 emit 含上限提示的 error 事件: %s", out)
	}
	// 不当作 prompt 喂给 Run：不应出现 result 事件（旧实现会喂给 Run 产生 result/error）。
	if strings.Contains(out, `"type":"result"`) {
		t.Errorf("超限行不应产生 result 事件（不该被当 prompt 喂给 Run）: %s", out)
	}
}

// P3-1 回归守卫：正常短输入不受单行封顶影响，仍被当作 prompt 喂给 Run。
// 与 TestCLI_InteractiveErrorThenEOFExits1 互补：聚焦「短输入不被误判为超限」。
func TestCLI_InteractiveShortLineStillProcessed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	// "hi\n"：readTurn 返 ("hi", false, false)，喂给 Run → LLM 500 → emit Run 错误。
	code, out := runMainBin(t, "hi\n", chatArgs(srv.URL, "-interactive"), "MINIAGENT_API_KEY=sk-test")
	// 短输入不应触发 oversized 拒绝信号。
	if strings.Contains(out, "超过大小上限") {
		t.Errorf("短输入不应触发 oversized 拒绝: %s", out)
	}
	// 短输入应被当 prompt 处理（Run 调 LLM 500 emit error），证明未误判。
	if !strings.Contains(out, `"type":"error"`) {
		t.Errorf("短输入应被当 prompt 处理（Run 失败 emit error）: %s", out)
	}
	if code != 1 {
		t.Errorf("code = %d, want 1（EOF 前有 Run 失败轮）", code)
	}
}
