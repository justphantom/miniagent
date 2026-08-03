package miniagent

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// 空 msgs：AppendMessages no-op，不创建文件（main 仅成功轮调用，空 NewMessages 不落盘）。
func TestAppendMessages_EmptyNoop(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.jsonl")
	if err := AppendMessages(path, SessionMeta{ID: "s"}, nil); err != nil {
		t.Fatalf("empty append should be no-op: %v", err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("file should not be created for empty msgs")
	}
}

// 落盘权限 0o600（对话属敏感数据）。
func TestAppendMessages_FileMode0600(t *testing.T) {
	path := filepath.Join(t.TempDir(), "perm.jsonl")
	if err := AppendMessages(path, SessionMeta{ID: "s"}, sampleTranscript()); err != nil {
		t.Fatalf("AppendMessages: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("mode = %o, want 600", info.Mode().Perm())
	}
}

// 多次 Append 累积（append-only），每次仅追加新消息。
func TestAppendMessages_AppendsAcrossCalls(t *testing.T) {
	path := filepath.Join(t.TempDir(), "acc.jsonl")
	meta := SessionMeta{ID: "s"}
	if err := AppendMessages(path, meta, []Message{{Role: "user", Content: "q1"}}); err != nil {
		t.Fatal(err)
	}
	if err := AppendMessages(path, meta, []Message{{Role: "assistant", Content: "a1"}}); err != nil {
		t.Fatal(err)
	}
	_, msgs, err := LoadSession(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 || msgs[0].Content != "q1" || msgs[1].Content != "a1" {
		t.Errorf("appended msgs wrong: %+v", msgs)
	}
}

// P1-4：追加后将超过 maxSessionBytes 时返回 error，不写入。读侧硬封顶，写侧不预判会让
// 某轮落盘后文件刚越 4MiB，下一轮 LoadSession 永久失败致会话不可接续。
func TestAppendMessages_OversizedAppendErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	meta := SessionMeta{ID: "s"}
	big := strings.Repeat("x", int(sessionBytes()/2)+100)
	if err := AppendMessages(path, meta, []Message{{Role: "user", Content: big}}); err != nil {
		t.Fatalf("首次追加应成功: %v", err)
	}
	// 再追加同样大小，总和超 maxSessionBytes → 写侧预判应返回 error。
	if err := AppendMessages(path, meta, []Message{{Role: "user", Content: big}}); err == nil {
		t.Fatal("P1-4：追加后超 maxSessionBytes 应返回 error")
	}
	// 第二次失败不应让文件越过上限（写侧预判在写入前）。
	info, _ := os.Stat(path)
	if info.Size() > sessionBytes() {
		t.Errorf("文件大小 %d 超 maxSessionBytes %d", info.Size(), sessionBytes())
	}
}

// P2-13：并发 AppendMessages 不损坏 session 文件。flock 跨进程互斥由 POSIX 保证；
// 同进程内不同 fd 的 flock 语义不互斥，此测试主要回归保护 AppendMessages 集成 flock
// 后单进程多调用仍正常（不阻塞自己、不残留锁、LoadSession 不报中间损坏）。
func TestAppendMessages_ConcurrentSafe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	meta := SessionMeta{ID: "s"}
	if err := AppendMessages(path, meta, []Message{{Role: "user", Content: "init"}}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for g := range 4 {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			for i := range 10 {
				if err := AppendMessages(path, meta, []Message{
					{Role: "assistant", Content: fmt.Sprintf("g%d-%d", idx, i)},
				}); err != nil {
					t.Errorf("append %d-%d: %v", idx, i, err)
				}
			}
		}(g)
	}
	wg.Wait()
	_, msgs, err := LoadSession(path)
	if err != nil {
		t.Fatalf("P2-13：并发 append 后 load 报错（行交织中间损坏）: %v", err)
	}
	if want := 1 + 4*10; len(msgs) != want {
		t.Errorf("msgs len = %d, want %d", len(msgs), want)
	}
}

// P3 session 硬化：AppendMessages 用 O_NOFOLLOW 拒绝最终分量是 symlink 的目标，
// 防本地攻击者预先用 symlink 指向敏感文件被 append 污染。
func TestAppendMessages_RejectsSymlinkTarget(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.jsonl")
	if err := os.WriteFile(target, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.jsonl")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	if err := AppendMessages(link, SessionMeta{ID: "s"}, []Message{{Role: "user", Content: "x"}}); err == nil {
		t.Error("O_NOFOLLOW 应拒绝 symlink 目标")
	}
}

// P3 session 硬化：MkdirAll 用 0o700（非旧 0o755），防 group-writable 目录下其他用户写入。
func TestAppendMessages_MkdirAll0700(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "a", "b", "s.jsonl")
	if err := AppendMessages(nested, SessionMeta{ID: "s"}, []Message{{Role: "user", Content: "x"}}); err != nil {
		t.Fatalf("AppendMessages: %v", err)
	}
	aDir := filepath.Join(dir, "a")
	info, err := os.Stat(aDir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Errorf("目录权限 %o, want 700", info.Mode().Perm())
	}
}

// P3-10：validateToolPairing 错误消息索引为 1-based（便于按行号定位 session 文件）。
func TestValidateToolPairing_ErrorMessageIsOneBased(t *testing.T) {
	// 第 2 条（0-based 索引 1）出现重复 tool_call id。
	msgs := []Message{
		{Role: roleAssistant, ToolCalls: []ToolCall{{ID: "dup", Name: "x", Args: "{}"}}},
		{Role: roleAssistant, ToolCalls: []ToolCall{{ID: "dup", Name: "x", Args: "{}"}}},
	}
	err := validateToolPairing(msgs)
	if err == nil {
		t.Fatal("expected pairing error")
	}
	if !strings.Contains(err.Error(), "第 2 条") {
		t.Errorf("错误消息应为 1-based「第 2 条」，got: %v", err)
	}
}

// RewriteMessages 全量重写：内容正确、临时文件清理、权限 0o600、旧中段（被屏障的旧轮）真丢弃。
// 这是 P2「session 文件永不压缩」的核心修复——append-only 落盘的 newMsgs 含被屏障旧 summary
// 与被压中段，rewrite 用全量 transcript（result.Messages）原子替换文件，把它们真正丢掉。
func TestRewriteMessages_AtomicRewrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	// 先 append 旧内容（含将被 rewrite 丢弃的旧 summary + 旧中段）。
	if err := AppendMessages(path, SessionMeta{ID: "s"}, []Message{
		{Role: "user", Kind: KindSummary, Content: "[既往对话摘要] 旧"},
		{Role: "user", Content: "被屏障的旧轮"},
	}); err != nil {
		t.Fatal(err)
	}
	// Rewrite 为新内容（新 summary + 最近轮）。
	want := []Message{
		{Role: "user", Kind: KindSummary, Content: "[既往对话摘要] 新"},
		{Role: "user", Content: "最近轮提问"},
		{Role: "assistant", Content: "最近轮回答"},
	}
	if err := RewriteMessages(path, SessionMeta{ID: "s"}, want); err != nil {
		t.Fatalf("RewriteMessages: %v", err)
	}
	// 临时文件已清理。
	matches, err := filepath.Glob(filepath.Join(dir, "s.jsonl.tmp-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Errorf("临时文件未清理: %v", matches)
	}
	// 权限 0o600。
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("权限 %o, want 600", info.Mode().Perm())
	}
	// 内容：旧"被屏障的旧轮"已丢，load 后只剩新 msgs。
	_, got, err := LoadSession(path)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("rewrite 后内容错:\n got %+v\nwant %+v", got, want)
	}
}

// RewriteMessages 空 msgs 也合法（写仅 metadata 文件，等价于「重置」）。
func TestRewriteMessages_EmptyMsgsWritesMeta(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	if err := RewriteMessages(path, SessionMeta{ID: "s"}, nil); err != nil {
		t.Fatalf("RewriteMessages 空 msgs: %v", err)
	}
	meta, msgs, err := LoadSession(path)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("空 rewrite 后 msgs 应为空: %+v", msgs)
	}
	if meta.ID != "s" {
		t.Errorf("meta.ID = %q, want s", meta.ID)
	}
}

// RewriteMessages 超 maxSessionBytes 报错，不创建/不替换文件。
func TestRewriteMessages_OversizedFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	big := strings.Repeat("x", int(sessionBytes())+1)
	if err := RewriteMessages(path, SessionMeta{ID: "s"}, []Message{{Role: "user", Content: big}}); err == nil {
		t.Fatal("超 maxSessionBytes 应报错")
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("超限 rewrite 不应创建文件（写临时文件前预判）")
	}
}

// P3 session 硬化：RewriteMessages 同样用 O_NOFOLLOW 拒绝 symlink 目标。
func TestRewriteMessages_RejectsSymlinkTarget(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.jsonl")
	if err := os.WriteFile(target, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.jsonl")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	if err := RewriteMessages(link, SessionMeta{ID: "s"}, []Message{{Role: "user", Content: "x"}}); err == nil {
		t.Error("O_NOFOLLOW 应拒绝 symlink 目标")
	}
}
