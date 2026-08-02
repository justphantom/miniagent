package miniagent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// edits 数组多处不同 old_string 顺序 apply：全部成功，内容正确。
func TestEdit_EditsArray_SequentialApply(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("foo bar baz"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := EditFileTool(dir).Call(context.Background(), `{"path":"f.txt","edits":[{"old_string":"foo","new_string":"FOO"},{"old_string":"baz","new_string":"BAZ"}]}`)
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
	r := EditFileTool(dir).Call(context.Background(), `{"path":"f.txt","edits":[{"old_string":"foo","new_string":"FOO"},{"old_string":"nope","new_string":"X"}]}`)
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
	r := EditFileTool(dir).Call(context.Background(), `{"path":"f.txt","edits":[{"old_string":"a","new_string":"b","replace_all":true},{"old_string":"bbb","new_string":"c"}]}`)
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
	r := EditFileTool(dir).Call(context.Background(), `{"path":"f.txt","old_string":"hello","new_string":"hi"}`)
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
	r := EditFileTool(dir).Call(context.Background(), `{"path":"f.txt","old_string":"hello","new_string":"hi","edits":[{"old_string":"x","new_string":"y"}]}`)
	if !r.IsError {
		t.Fatal("expected ambiguity error when both edits and old_string supplied")
	}
	got, _ := os.ReadFile(path)
	if string(got) != "hello" {
		t.Errorf("file should be unchanged on ambiguity error: %q", got)
	}
}
