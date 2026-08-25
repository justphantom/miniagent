package session

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/justphantom/miniagent/miniagent"
)

// 端到端：真实 minisession 二进制 + Client 全流程（创建→追加→读取→覆写→删除）。
// MINISESSION_BIN 未设置时跳过（跨仓库依赖，非 CI 必需）。
func TestClientE2EWithMinisession(t *testing.T) {
	bin := os.Getenv("MINISESSION_BIN")
	if bin == "" {
		t.Skip("MINISESSION_BIN not set; set it to the minisession binary path")
	}

	dir := t.TempDir()
	cmd := exec.CommandContext(context.Background(), bin, "-listen", "127.0.0.1:9799", "-data-dir", dir, "-key", "e2e-key") //nolint:noctx // 测试进程生命周期由 t.Cleanup 管理
	if err := cmd.Start(); err != nil {
		t.Fatalf("start minisession: %v", err)
	}
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	// 等服务就绪
	c := NewClient("http://127.0.0.1:9799", "e2e-key")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := c.ListSessions(ctx); err == nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// 完整生命周期
	meta, err := c.CreateSession(ctx, miniagent.SessionMeta{ID: "e2e-1", Model: "gpt-4o", Provider: "openai"})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if meta.ID != "e2e-1" {
		t.Fatalf("meta.ID = %q", meta.ID)
	}

	msgs := []miniagent.Message{
		{Role: miniagent.RoleUser, Content: "hello"},
		{Role: miniagent.RoleAssistant, Content: "hi", Usage: &miniagent.Usage{InputTokens: 10, OutputTokens: 5}},
	}
	if err := c.AppendMessages(ctx, "e2e-1", msgs); err != nil {
		t.Fatalf("AppendMessages: %v", err)
	}

	_, got, err := c.LoadSession(ctx, "e2e-1")
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if len(got) != 2 || got[0].Content != "hello" || got[1].Usage.InputTokens != 10 || got[1].Usage.OutputTokens != 5 {
		t.Fatalf("round trip mismatch: %+v", got)
	}

	if err := c.RewriteMessages(ctx, "e2e-1", meta, msgs[:1]); err != nil {
		t.Fatalf("RewriteMessages: %v", err)
	}
	_, got, err = c.LoadSession(ctx, "e2e-1")
	if err != nil {
		t.Fatalf("LoadSession after rewrite: %v", err)
	}
	if len(got) != 1 || got[0].Content != "hello" {
		t.Fatalf("rewrite mismatch: %+v", got)
	}

	if err := c.DeleteSession(ctx, "e2e-1"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}
	if _, _, err := c.LoadSession(ctx, "e2e-1"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected ErrNotExist after delete, got %v", err)
	}
}
