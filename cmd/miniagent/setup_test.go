package main

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/justphantom/miniagent/internal/miniagent"
)

// P4：buildLLM 返回 ChatClient（带 120s 总 Timeout，非流式 Do 兜底防挂死 #3）+ StreamClient
// （无 Timeout，body 不被砍 P2-5），两者共享同一 *http.Transport（代理/dial/TLS 超时，#2）。
func TestBuildLLM_ChatTimeoutStreamNoTimeoutSharedTransport(t *testing.T) {
	chat, stream := buildLLM("sk", miniagent.ProviderConfig{ChatURL: "http://localhost:1234/v1/chat/completions"}, nil, 0)
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

// http（非 loopback）端点应警告明文传 key。
func TestWarnInsecureURL(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	warnInsecureURL("http://llm.internal:8000/v1/chat/completions")
	warnInsecureURL("http://localhost:8080/v1/chat/completions")
	warnInsecureURL("http://127.0.0.1:11434/v1/chat/completions")
	warnInsecureURL("https://api.openai.com/v1/chat/completions")
	_ = w.Close()
	os.Stderr = old
	out, _ := io.ReadAll(r)
	if got := strings.Count(string(out), "warning"); got != 1 {
		t.Errorf("warnings = %d, want 1: %s", got, out)
	}
}

func TestResolveAPIKey(t *testing.T) {
	dir := t.TempDir()
	kf := filepath.Join(dir, "k")
	if err := os.WriteFile(kf, []byte("  sk-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := resolveAPIKey(kf, "sk-env")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != "sk-file" {
		t.Errorf("got %q, want sk-file", got)
	}
	got2, err := resolveAPIKey("", "sk-env2")
	if err != nil || got2 != "sk-env2" {
		t.Errorf("got (%q,%v), want sk-env2", got2, err)
	}
	if _, err := resolveAPIKey(filepath.Join(dir, "nope"), ""); err == nil {
		t.Error("expected error for missing key-file")
	}
}

func TestWarnKeyFilePerm(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stderr
	os.Stderr = w
	dir := t.TempDir()
	loose := filepath.Join(dir, "loose")
	_ = os.WriteFile(loose, []byte("x"), 0o644)
	tight := filepath.Join(dir, "tight")
	_ = os.WriteFile(tight, []byte("x"), 0o600)
	warnKeyFilePerm(loose)
	warnKeyFilePerm(tight)
	_ = w.Close()
	os.Stderr = old
	out, _ := io.ReadAll(r)
	if c := strings.Count(string(out), "warning"); c != 1 {
		t.Errorf("warnings = %d, want 1: %s", c, out)
	}
}

// requireConfig：无 -config 且 ~/.miniagent/miniagent.json 不存在时，应返回 error。
// 显式 -config 不存在=硬错误（通过 TestCLI_ExplicitConfigMissingExits1 覆盖）。
func TestRequireConfig_NoConfigExits(t *testing.T) {
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldWD)
	dir := t.TempDir()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	// 用测试目录替代 HOME，避免加载真实的 ~/.miniagent/miniagent.json
	t.Setenv("HOME", dir)
	_, err = requireConfig("")
	if err == nil {
		t.Fatal("requireConfig should error when no config found")
	}
	if !strings.Contains(err.Error(), "config 不存在") {
		t.Errorf("error should mention config missing: %v", err)
	}
}

// requireConfig：默认 config 的 Stat 错误若非 fs.ErrNotExist（如自指符号链接触发
// ELOOP、permission denied）按硬错误返回（审查 P2-6）。
func TestRequireConfig_DefaultStatErrorIsHardError(t *testing.T) {
	oldWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(oldWD)
	dir := t.TempDir()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	// 用测试目录替代 HOME，避免加载真实的 ~/.miniagent/miniagent.json
	t.Setenv("HOME", dir)
	maDir := filepath.Join(dir, ".miniagent")
	if err := os.MkdirAll(maDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(maDir, "miniagent.json")
	// 自指符号链接：Stat 跟随 → ELOOP（非 fs.ErrNotExist）→ 硬错误。
	if err := os.Symlink(cfgPath, cfgPath); err != nil {
		t.Skipf("cannot create self-referential symlink: %v", err)
	}
	_, err = requireConfig("")
	if err == nil {
		t.Fatal("expected hard error for non-ErrNotExist Stat failure, got nil")
	}
	// 错误必须透传原始 stat 失败，而不是被吞成“config 不存在”。
	if !strings.Contains(err.Error(), "stat config") && !strings.Contains(err.Error(), "too many levels of symbolic links") {
		t.Errorf("expected hard stat error, got: %v", err)
	}
}
