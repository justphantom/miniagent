package policy

import (
	"strings"
	"testing"
)

// TrimForHistory：显式 limit 装裁到该值；limit<=0 用默认 MaxToolResultInHistory。split=false 走 head-only。
func TestTrimForHistory_PerLimit(t *testing.T) {
	big := strings.Repeat("x", 10000)
	got := TrimForHistory(big, 8000, false)
	if len(got) <= 8000 || !strings.Contains(got, "截断") {
		t.Errorf("limit=8000: len=%d, marker missing: %q", len(got), got[:min(len(got), 40)])
	}
	got0 := TrimForHistory(big, 0, false)
	// 默认裁到 MaxToolResultInHistory：长度应略大于该值（含 marker），远小于 8000。
	if len(got0) <= MaxToolResultInHistory || len(got0) >= 8000 {
		t.Errorf("limit=0: len=%d, want in (%d, 8000)", len(got0), MaxToolResultInHistory)
	}
}

// TrimForHistory split=true（shell/grep）：头尾分段截断，保留尾部错误结论标记，总长落在 limit 附近。
func TestTrimForHistory_SplitKeepsTail(t *testing.T) {
	// 头部上下文 + 大段中间噪声 + 尾部错误结论。head-only 会丢掉 FAIL 行。
	body := "CMD: build\n" + strings.Repeat("log\n", 2000) + "FAIL: exit status 1"
	got := TrimForHistory(body, 4000, true)
	if !strings.Contains(got, "省略中间段") {
		t.Errorf("split 应含中段省略标记: %q", got[:min(len(got), 60)])
	}
	if !strings.Contains(got, "FAIL: exit status 1") {
		t.Errorf("split 应保留尾部错误结论: tail=%q", got[max(0, len(got)-80):])
	}
	if !strings.Contains(got, "CMD: build") {
		t.Errorf("split 应保留头部上下文: head=%q", got[:min(len(got), 40)])
	}
	if len(got) > 4200 { // 头 n/4 + 尾 3n/4 + marker，应略超 limit
		t.Errorf("split 总长应接近 limit: len=%d", len(got))
	}
}

