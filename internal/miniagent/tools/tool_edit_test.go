package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

// edits array with several distinct old_strings applied in order: all succeed, content is correct.
func TestEdit_EditsArray_SequentialApply(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("foo bar baz"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := EditFileTool(dir, 0).Call(context.Background(), `{"path":"f.txt","edits":[{"old_string":"foo","new_string":"FOO"},{"old_string":"baz","new_string":"BAZ"}]}`)
	if r.IsError {
		t.Fatalf("edit edits: %s", r.Output)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "FOO bar BAZ" {
		t.Errorf("content = %q, want FOO bar BAZ", got)
	}
}

// If some old_string in the middle of edits does not match -> the whole transaction rolls back, file unchanged.
func TestEdit_EditsArray_RollbackOnFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	original := "foo bar baz"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	r := EditFileTool(dir, 0).Call(context.Background(), `{"path":"f.txt","edits":[{"old_string":"foo","new_string":"FOO"},{"old_string":"nope","new_string":"X"}]}`)
	if !r.IsError {
		t.Errorf("should fail on missing 2nd old_string: %s", r.Output)
	}
	got, _ := os.ReadFile(path)
	if string(got) != original {
		t.Errorf("file changed despite rollback: %q", got)
	}
}

// Mixing replace_all with a unique match in edits: the first segment replaces all, then the second matches against the new content.
func TestEdit_EditsArray_ReplaceAllMixed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("aaa"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := EditFileTool(dir, 0).Call(context.Background(), `{"path":"f.txt","edits":[{"old_string":"a","new_string":"b","replace_all":true},{"old_string":"bbb","new_string":"c"}]}`)
	if r.IsError {
		t.Fatalf("edit edits: %s", r.Output)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "c" {
		t.Errorf("content = %q, want c", got)
	}
}

// S3 backward compatibility: single-segment old_string/new_string still works (no edits passed).
func TestEdit_SingleString_BackwardCompat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("hello world"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := EditFileTool(dir, 0).Call(context.Background(), `{"path":"f.txt","old_string":"hello","new_string":"hi"}`)
	if r.IsError {
		t.Fatalf("edit single: %s", r.Output)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "hi world" {
		t.Errorf("content = %q, want hi world", got)
	}
}

// S3: passing both edits and old_string/new_string should error (mutually exclusive).
func TestEdit_EditsAndSingleStringMutuallyExclusive(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := EditFileTool(dir, 0).Call(context.Background(), `{"path":"f.txt","old_string":"hello","new_string":"hi","edits":[{"old_string":"x","new_string":"y"}]}`)
	if !r.IsError {
		t.Fatal("expected ambiguity error when both edits and old_string supplied")
	}
	got, _ := os.ReadFile(path)
	if string(got) != "hello" {
		t.Errorf("file should be unchanged on ambiguity error: %q", got)
	}
}

// A typo'd new_string field must be a loud parse error, not a silent deletion: json.Unmarshal
// ignored the unknown key, NewString fell back to "", and the matched text was replaced with
// nothing while the tool reported success.
func TestEdit_RejectsTypoedNewStringField(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("TODO: fix me"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := EditFileTool(dir, 0).Call(context.Background(), `{"path":"f.txt","old_string":"TODO: ","new_str":"DONE"}`)
	if !r.IsError {
		t.Fatalf("typo'd new_string must be rejected, got success: %s", r.Output)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "TODO: fix me" {
		t.Errorf("file was modified despite the rejection: %q", got)
	}
	// trailing data (duplicated payload) is likewise rejected
	r = EditFileTool(dir, 0).Call(context.Background(), `{"path":"f.txt","old_string":"TODO: ","new_string":"DONE"}{"path":"f.txt"}`)
	if !r.IsError {
		t.Error("concatenated second object must be rejected")
	}
}

// pathLocks serializes same-path acquire: when concurrently acquiring the same path, at most one holds the lock at any time.
func TestPathLocks_SamePathSerializes(t *testing.T) {
	var l pathLocks
	var inUse, maxInUse atomic.Int32
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			defer l.acquire("/same")()
			cur := inUse.Add(1)
			for {
				old := maxInUse.Load()
				if cur <= old || maxInUse.CompareAndSwap(old, cur) {
					break
				}
			}
			inUse.Add(-1)
		})
	}
	wg.Wait()
	if maxInUse.Load() > 1 {
		t.Fatalf("same-path acquire should serialize, max concurrency = %d (want <=1)", maxInUse.Load())
	}
}

// Parallel edits to the same file (different old_strings) should not lose updates: lock-based serialization makes both replacements land on disk.
// Regression: previously A/B each read the old content -> each wrote, the later write overwrote the earlier one, losing one edit.
func TestEdit_ParallelSameFileNoLostUpdate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("A\nB\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := EditFileTool(dir, 0)
	var wg sync.WaitGroup
	wg.Go(func() { tool.Call(context.Background(), `{"path":"f.txt","old_string":"A","new_string":"X"}`) })
	wg.Go(func() { tool.Call(context.Background(), `{"path":"f.txt","old_string":"B","new_string":"Y"}`) })
	wg.Wait()
	got, _ := os.ReadFile(path)
	if string(got) != "X\nY\n" {
		t.Errorf("parallel same-file edit should land both replacements (no lost update): %q", string(got))
	}
}

// 不同拼写指向同一文件必须取同一把锁：resolveToolPath 对绝对路径原样返回（无 Clean），
// "/w/./f" 与 "f" 若按原串作键会各持一把锁，读-改-写交错丢失更新。
func TestEdit_LockKeyNormalizesEquivalentPaths(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("A\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := EditFileTool(dir, 0)
	// 用两个拼写交替编辑同一路径（A→B、B→C、C→D、D→E），全部成功即两拼写命中同一文件。
	spellings := []string{"f.txt", filepath.Join(dir, ".", "f.txt")}
	for i := range 4 {
		cur := string(rune('A' + i))
		next := string(rune('A' + i + 1))
		res := tool.Call(context.Background(), fmt.Sprintf(`{"path":%q,"old_string":%q,"new_string":%q}`, spellings[i%2], cur, next))
		if res.IsError {
			t.Fatalf("edit #%d via %q failed: %s", i, spellings[i%2], res.Output)
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, "f.txt"))
	if err != nil || string(data) != "E\n" {
		t.Fatalf("expected E (4 chained replacements across both spellings), got %q (err=%v)", data, err)
	}
}
