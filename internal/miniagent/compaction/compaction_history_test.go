package compaction

import (
	"strings"
	"testing"

	"github.com/justphantom/miniagent/internal/miniagent"
)

// compactHistory 是摘要失败/无中段时的有损 fallback（compaction_split.go）：保留「最早 1 轮 + 最近
// keepRecent 轮」，丢弃中段。该路径长期仅被 TestFitHistory_SummarizeErrorFallsBackLossy 间接覆盖，
// 本测试直接锁定首尾保留 + 中段丢弃语义。
func TestCompactHistory_DropsMiddleKeepsEnds(t *testing.T) {
	// 5 轮 user+assistant，keepRecent=2：应保留最早 1 轮 + 最近 2 轮，丢弃中段 2 轮。
	msgs := []miniagent.Message{
		{Role: miniagent.RoleUser, Content: "first"},
		{Role: miniagent.RoleAssistant, Content: "first-a"},
		{Role: miniagent.RoleUser, Content: "mid1"},
		{Role: miniagent.RoleAssistant, Content: "mid1-a"},
		{Role: miniagent.RoleUser, Content: "mid2"},
		{Role: miniagent.RoleAssistant, Content: "mid2-a"},
		{Role: miniagent.RoleUser, Content: "last1"},
		{Role: miniagent.RoleAssistant, Content: "last1-a"},
		{Role: miniagent.RoleUser, Content: "last2"},
		{Role: miniagent.RoleAssistant, Content: "last2-a"},
	}
	out := compactHistory(msgs, 2)
	// 有损：输出须少于输入（中段被丢弃）。
	if len(out) >= len(msgs) {
		t.Errorf("应丢弃中段（有损）：out=%d msgs, in=%d", len(out), len(msgs))
	}
	// 保留最早轮 first。
	if len(out) == 0 || out[0].Content != "first" {
		t.Errorf("应保留最早轮 first：got %+v", out)
	}
	// 保留最近轮 last2-a。
	if len(out) == 0 || out[len(out)-1].Content != "last2-a" {
		t.Errorf("应保留最近轮 last2-a：got %+v", out[len(out)-1])
	}
	// 中段 mid1/mid2 全丢弃。
	for _, m := range out {
		if strings.Contains(m.Content, "mid") {
			t.Errorf("中段应被丢弃但仍存在：%s", m.Content)
		}
	}
}

// 边界：轮数 <= 1+keepRecent 时原样返回（无需有损）。
func TestCompactHistory_NoopWhenFewRounds(t *testing.T) {
	// splitRounds 把无 tool_call 的消息每条独立成轮：3 条 = 3 轮，<= 1+keepRecent(=3) 原样返回。
	msgs := []miniagent.Message{
		{Role: miniagent.RoleUser, Content: "u1"},
		{Role: miniagent.RoleAssistant, Content: "a1"},
		{Role: miniagent.RoleUser, Content: "u2"},
	}
	out := compactHistory(msgs, 2) // 3 轮 <= 1+2=3，原样
	if len(out) != len(msgs) {
		t.Errorf("轮数 <= 1+keepRecent 应原样返回：out=%d, want %d", len(out), len(msgs))
	}
}
