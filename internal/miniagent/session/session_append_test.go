package session

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/justphantom/miniagent/internal/miniagent"
)

// Empty msgs: AppendMessages is a no-op, does not create a file (main only calls on successful turns; empty NewMessages is not persisted).
func TestAppendMessages_EmptyNoop(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.jsonl")
	if err := AppendMessages(path, SessionMeta{ID: "s"}, nil); err != nil {
		t.Fatalf("empty append should be no-op: %v", err)
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("file should not be created for empty msgs")
	}
}

// Persist permission 0o600 (conversation is sensitive data).
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

// Multiple Appends accumulate (append-only); each call only appends new messages.
func TestAppendMessages_AppendsAcrossCalls(t *testing.T) {
	path := filepath.Join(t.TempDir(), "acc.jsonl")
	meta := SessionMeta{ID: "s"}
	if err := AppendMessages(path, meta, []miniagent.Message{{Role: "user", Content: "q1"}}); err != nil {
		t.Fatal(err)
	}
	if err := AppendMessages(path, meta, []miniagent.Message{{Role: "assistant", Content: "a1"}}); err != nil {
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

// P1-4: when the append would exceed maxSessionBytes, return an error and do not write. The read side
// has a hard cap; if the write side did not pre-check, a turn could persist a file just over 4MiB, and the
// next turn's LoadSession would permanently fail, making the session impossible to resume.
func TestAppendMessages_OversizedAppendErrors(t *testing.T) {
	const maxSz = int64(1 << 20)
	path := filepath.Join(t.TempDir(), "s.jsonl")
	meta := SessionMeta{ID: "s"}
	big := strings.Repeat("x", int(maxSz/2)+100)
	if err := AppendMessages(path, meta, []miniagent.Message{{Role: "user", Content: big}}, maxSz); err != nil {
		t.Fatalf("first append should succeed: %v", err)
	}
	// Append the same size again; the total exceeds maxSz -> the write side pre-check should return an error.
	if err := AppendMessages(path, meta, []miniagent.Message{{Role: "user", Content: big}}, maxSz); err == nil {
		t.Fatal("P1-4: append exceeding maxSz should return an error")
	}
	// The second failed attempt should not let the file exceed the cap (write-side pre-check happens before writing).
	info, _ := os.Stat(path)
	if info.Size() > maxSz {
		t.Errorf("file size %d exceeds maxSz %d", info.Size(), maxSz)
	}
}

// P2-13: concurrent AppendMessages must not corrupt the session file. flock cross-process mutual
// exclusion is guaranteed by POSIX; within the same process, flock semantics on different fds are not
// mutually exclusive, so this test is mainly a regression guard ensuring AppendMessages still works
// correctly for multiple calls within a single process after integrating flock (does not block itself,
// leaves no stale locks, and LoadSession reports no intermediate corruption).
func TestAppendMessages_ConcurrentSafe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	meta := SessionMeta{ID: "s"}
	if err := AppendMessages(path, meta, []miniagent.Message{{Role: "user", Content: "init"}}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for g := range 4 {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			for i := range 10 {
				if err := AppendMessages(path, meta, []miniagent.Message{
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
		t.Fatalf("P2-13: load failed after concurrent append (interleaved lines indicate intermediate corruption): %v", err)
	}
	if want := 1 + 4*10; len(msgs) != want {
		t.Errorf("msgs len = %d, want %d", len(msgs), want)
	}
}

// P3 session hardening: AppendMessages uses O_NOFOLLOW to reject a target whose final component is a symlink,
// preventing a local attacker from pre-placing a symlink pointing at a sensitive file and polluting it via append.
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
	if err := AppendMessages(link, SessionMeta{ID: "s"}, []miniagent.Message{{Role: "user", Content: "x"}}); err == nil {
		t.Error("O_NOFOLLOW should reject the symlink target")
	}
}

// P3 session hardening: MkdirAll uses 0o700 (not the old 0o755), preventing other users from writing under a group-writable directory.
func TestAppendMessages_MkdirAll0700(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "a", "b", "s.jsonl")
	if err := AppendMessages(nested, SessionMeta{ID: "s"}, []miniagent.Message{{Role: "user", Content: "x"}}); err != nil {
		t.Fatalf("AppendMessages: %v", err)
	}
	aDir := filepath.Join(dir, "a")
	info, err := os.Stat(aDir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Errorf("dir permission %o, want 700", info.Mode().Perm())
	}
}

// P3-10: validateToolPairing error message index is 1-based (for locating lines in the session file).
func TestValidateToolPairing_ErrorMessageIsOneBased(t *testing.T) {
	// The 2nd message (0-based index 1) has a duplicate tool_call id.
	msgs := []miniagent.Message{
		{Role: miniagent.RoleAssistant, ToolCalls: []miniagent.ToolCall{{ID: "dup", Name: "x", Args: "{}"}}},
		{Role: miniagent.RoleAssistant, ToolCalls: []miniagent.ToolCall{{ID: "dup", Name: "x", Args: "{}"}}},
	}
	err := ValidateToolPairing(msgs)
	if err == nil {
		t.Fatal("expected pairing error")
	}
	if !strings.Contains(err.Error(), "message 2") {
		t.Errorf("error message should be 1-based using 'message 2', got: %v", err)
	}
}

// RewriteMessages full rewrite: correct content, temp file cleanup, permission 0o600, and the old middle
// segment (the old turns blocked by the barrier) is truly discarded. This is the core fix for the P2 issue
// "session files are never compacted" -- append-only persisted newMsgs contain the old summary blocked by
// the barrier and the compacted middle segment; rewrite atomically replaces the file with the full transcript
// (result.Messages), truly discarding them.
func TestRewriteMessages_AtomicRewrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	// First append old content (including the old summary + old middle segment that rewrite will discard).
	if err := AppendMessages(path, SessionMeta{ID: "s"}, []miniagent.Message{
		{Role: "user", Kind: miniagent.KindSummary, Content: "[Previous conversation summary] old"},
		{Role: "user", Content: "old blocked turn"},
	}); err != nil {
		t.Fatal(err)
	}
	// Rewrite to new content (new summary + recent turns).
	want := []miniagent.Message{
		{Role: "user", Kind: miniagent.KindSummary, Content: "[Previous conversation summary] new"},
		{Role: "user", Content: "recent turn question"},
		{Role: "assistant", Content: "recent turn answer"},
	}
	if err := RewriteMessages(path, SessionMeta{ID: "s"}, want); err != nil {
		t.Fatalf("RewriteMessages: %v", err)
	}
	// Temp files have been cleaned up.
	matches, err := filepath.Glob(filepath.Join(dir, "s.jsonl.tmp-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Errorf("temp files not cleaned up: %v", matches)
	}
	// Permission 0o600.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("permission %o, want 600", info.Mode().Perm())
	}
	// Content: the old "old blocked turn" is gone; after load only the new msgs remain.
	_, got, err := LoadSession(path)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("content wrong after rewrite:\n got %+v\nwant %+v", got, want)
	}
}

// RewriteMessages with empty msgs is also valid (writes a metadata-only file, equivalent to a "reset").
func TestRewriteMessages_EmptyMsgsWritesMeta(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	if err := RewriteMessages(path, SessionMeta{ID: "s"}, nil); err != nil {
		t.Fatalf("RewriteMessages with empty msgs: %v", err)
	}
	meta, msgs, err := LoadSession(path)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if len(msgs) != 0 {
		t.Errorf("after empty rewrite, msgs should be empty: %+v", msgs)
	}
	if meta.ID != "s" {
		t.Errorf("meta.ID = %q, want s", meta.ID)
	}
}

// RewriteMessages exceeding maxSessionBytes returns an error and does not create/replace the file.
func TestRewriteMessages_OversizedFails(t *testing.T) {
	const maxSz = int64(1 << 20)
	dir := t.TempDir()
	path := filepath.Join(dir, "s.jsonl")
	big := strings.Repeat("x", int(maxSz)+1)
	if err := RewriteMessages(path, SessionMeta{ID: "s"}, []miniagent.Message{{Role: "user", Content: big}}, maxSz); err == nil {
		t.Fatal("exceeding maxSz should error")
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("an over-limit rewrite should not create a file (pre-check before writing the temp file)")
	}
}

// P3 session hardening: RewriteMessages likewise uses O_NOFOLLOW to reject symlink targets.
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
	if err := RewriteMessages(link, SessionMeta{ID: "s"}, []miniagent.Message{{Role: "user", Content: "x"}}); err == nil {
		t.Error("O_NOFOLLOW should reject the symlink target")
	}
}
