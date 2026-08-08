package tools

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

// edits 数组多处不同 old_string 顺序 apply：全部成功，内容正确。
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

// edits 中途某处 old_string 未匹配 → 整体回滚，文件不变。
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

// edits 中 replace_all 与唯一匹配混用：第一处全替后，第二处基于新内容匹配。
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

// S3 向后兼容：单段 old_string/new_string 仍可用（不传 edits）。
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

// S3：edits 与 old_string/new_string 同传应报错（互斥）。
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

// pathLocks 同路径 acquire 互斥串行：并发 acquire 同一路径时，任一时刻至多一个持锁。
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
		t.Fatalf("同路径 acquire 应串行，最大并发 = %d（want <=1）", maxInUse.Load())
	}
}

// 并行同文件 edit（不同 old_string）不应丢更新：持锁串行化使两处替换都落盘。
// 回归：此前 A/B 各 read 旧内容→各自写，后写覆盖先写，丢一处。
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
		t.Errorf("并行同文件 edit 应两处都落盘（不丢更新）: %q", string(got))
	}
}
