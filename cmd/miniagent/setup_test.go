package main

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/justphantom/miniagent/internal/miniagent"
)

// P4：buildLLM 返回 ChatClient（带 120s 总 Timeout，非流式 Do 兜底防挂死 #3）+ StreamClient
// （无 Timeout，body 不被砍 P2-5），两者共享同一 *http.Transport（代理/dial/TLS 超时，#2）。
func TestBuildLLM_ChatTimeoutStreamNoTimeoutSharedTransport(t *testing.T) {
	chat, stream := buildLLM("sk", miniagent.ProviderConfig{ChatURL: "http://localhost:1234/v1/chat/completions"}, nil)
	if chat.HTTP == nil {
		t.Fatal("ChatClient.HTTP is nil, want injected client")
	}
	if chat.HTTP.Timeout != 120*time.Second {
		t.Errorf("ChatClient Timeout = %v, want 120s（非流式 Do 总超时兜底，#3）", chat.HTTP.Timeout)
	}
	if stream.HTTP == nil || stream.HTTP.Timeout != 0 {
		t.Errorf("StreamClient Timeout = %v, want 0（流式 body 不被砍，P2-5）", stream.HTTP.Timeout)
	}
	ctr, ok := chat.HTTP.Transport.(*http.Transport)
	str, ok2 := stream.HTTP.Transport.(*http.Transport)
	if !ok || !ok2 {
		t.Fatalf("Transport = %T / %T, want *http.Transport", chat.HTTP.Transport, stream.HTTP.Transport)
	}
	if ctr != str {
		t.Error("ChatClient 与 StreamClient 应共享同一 *http.Transport")
	}
	if ctr.Proxy == nil {
		t.Error("Transport.Proxy = nil, want http.ProxyFromEnvironment（#2）")
	}
	if ctr.DialContext == nil {
		t.Error("Transport.DialContext not set（拨号超时）")
	}
	if ctr.ResponseHeaderTimeout == 0 {
		t.Error("Transport.ResponseHeaderTimeout = 0, want >0")
	}
}

// P3：key-file 经 O_NOFOLLOW 读取，拒最终分量 symlink——攻击者把 key-file 换成指向
// /etc/shadow 的软链会让机密外发；与 session openNoFollow 行为一致。
func TestResolveAPIKey_RejectsSymlinkAndReadsRegular(t *testing.T) {
	dir := t.TempDir()

	// 普通文件：正常读取（且 strings.TrimSpace 生效）。
	regular := filepath.Join(dir, "regular.key")
	if err := os.WriteFile(regular, []byte("  sk-secret  "), 0o600); err != nil {
		t.Fatalf("write regular key-file: %v", err)
	}
	got, err := resolveAPIKey(regular, "")
	if err != nil {
		t.Fatalf("regular file: unexpected error: %v", err)
	}
	if got != "sk-secret" {
		t.Errorf("regular file: got %q, want %q (TrimSpace)", got, "sk-secret")
	}

	// 符号链接：被 O_NOFOLLOW 拒（ELOOP），返回明确 error。
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("leaked"), 0o600); err != nil {
		t.Fatalf("write symlink target: %v", err)
	}
	link := filepath.Join(dir, "link.key")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("create symlink: %v", err)
	}
	if _, err := resolveAPIKey(link, ""); err == nil {
		t.Fatal("symlink key-file: want error (O_NOFOLLOW 拒最终分量 symlink)，got nil")
	}
}

// P3-5：key-file 超过 64KiB 上限应报错（API key 远小于此，防误读/被构造成巨型文件）。
func TestResolveAPIKey_RejectsOversizedKeyFile(t *testing.T) {
	dir := t.TempDir()
	big := filepath.Join(dir, "big.key")
	// 写 maxKeyFileSize+1 字节，刚好越过上限。
	oversized := make([]byte, maxKeyFileSize+1)
	if err := os.WriteFile(big, oversized, 0o600); err != nil {
		t.Fatalf("write oversized key-file: %v", err)
	}
	if _, err := resolveAPIKey(big, ""); err == nil {
		t.Fatal("oversized key-file: want error（超过 64KiB 上限），got nil")
	}
}
