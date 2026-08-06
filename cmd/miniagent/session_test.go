package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/justphantom/miniagent/internal/miniagent"
)

// generateSessionID 输出仅含拉丁字母/数字/-（过 ValidateSessionID），且含时间戳前缀。
func TestGenerateSessionID_Format(t *testing.T) {
	id := generateSessionID()
	if err := miniagent.ValidateSessionID(id); err != nil {
		t.Errorf("id %q 不合法: %v", id, err)
	}
	ts := time.Now().Format("20060102-150405")
	if !strings.HasPrefix(id, ts+"-") {
		t.Errorf("id %q 缺时间戳前缀 %q-", id, ts)
	}
}

// 同秒并发生成不应碰撞（随机段 64 bit 保证；回落路径加 pid 也区分同秒不同进程）。
func TestGenerateSessionID_Unique(t *testing.T) {
	seen := map[string]bool{}
	for i := range 1000 {
		id := generateSessionID()
		if seen[id] {
			t.Fatalf("第 %d 次碰撞: %q", i, id)
		}
		seen[id] = true
	}
}

// 无状态：saveNew=false 且 sessionArg="" → 空 path、零 meta、nil history。
func TestResolveSessionForRun_Stateless(t *testing.T) {
	path, meta, history := resolveSessionForRun(false, "", t.TempDir(), "p/m", "p", "/wd", 0)
	if path != "" {
		t.Errorf("path = %q, 想空（无状态不落盘）", path)
	}
	if meta != (miniagent.SessionMeta{}) {
		t.Errorf("meta = %+v, 想零值", meta)
	}
	if history != nil {
		t.Errorf("history 应 nil，got %v", history)
	}
}

// 新建分支：文件不存在时构造 meta，Type 留空（由 AppendMessages 写盘时补 session），
// Provider 独立于 modelSpec 单列，便于会话列举/多 provider 溯源免解析字符串。
func TestResolveSessionForRun_NewSession_FillsProvider(t *testing.T) {
	sessionDir := t.TempDir()
	path, meta, history := resolveSessionForRun(true, "", sessionDir, "openai/gpt-4o", "openai", "/repo", 0)
	wantPath := filepath.Join(sessionDir, meta.ID+".jsonl")
	if path != wantPath {
		t.Errorf("path = %q, want %q", path, wantPath)
	}
	if meta.Type != "" {
		t.Errorf("Type = %q, 想空（AppendMessages 写盘时补 session）", meta.Type)
	}
	if meta.Provider != "openai" {
		t.Errorf("Provider = %q, want openai", meta.Provider)
	}
	if meta.Model != "openai/gpt-4o" {
		t.Errorf("Model = %q, want openai/gpt-4o", meta.Model)
	}
	if meta.Workdir != "/repo" {
		t.Errorf("Workdir = %q, want /repo（absWorkdir 对绝对路径原样返回）", meta.Workdir)
	}
	if meta.ID == "" {
		t.Error("ID 为空")
	}
	if err := miniagent.ValidateSessionID(meta.ID); err != nil {
		t.Errorf("meta.ID %q 不合法: %v", meta.ID, err)
	}
	if _, err := time.Parse(time.RFC3339, meta.Created); err != nil {
		t.Errorf("Created %q 非 RFC3339: %v", meta.Created, err)
	}
	if history != nil {
		t.Errorf("history 应 nil（新会话无历史），got %v", history)
	}
}
