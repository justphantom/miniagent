package tools

import (
	"context"
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
