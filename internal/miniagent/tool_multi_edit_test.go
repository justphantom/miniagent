package miniagent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// 多处不同 old_string 顺序 apply：全部成功，内容正确。
func TestMultiEdit_SequentialApply(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("foo bar baz"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := MultiEditTool(dir).Call(context.Background(), `{"path":"f.txt","edits":[{"old_string":"foo","new_string":"FOO"},{"old_string":"baz","new_string":"BAZ"}]}`)
	if r.IsError {
		t.Fatalf("multi_edit: %s", r.Output)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "FOO bar BAZ" {
		t.Errorf("content = %q, want FOO bar BAZ", got)
	}
}

// 中途某处 old_string 未匹配 → 整体回滚，文件不变。
func TestMultiEdit_RollbackOnFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	original := "foo bar baz"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	r := MultiEditTool(dir).Call(context.Background(), `{"path":"f.txt","edits":[{"old_string":"foo","new_string":"FOO"},{"old_string":"nope","new_string":"X"}]}`)
	if !r.IsError {
		t.Errorf("should fail on missing 2nd old_string: %s", r.Output)
	}
	got, _ := os.ReadFile(path)
	if string(got) != original {
		t.Errorf("file changed despite rollback: %q", got)
	}
}

// replace_all 与唯一匹配混用：第一处全替后，第二处基于新内容匹配。
func TestMultiEdit_ReplaceAllMixed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	if err := os.WriteFile(path, []byte("aaa"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := MultiEditTool(dir).Call(context.Background(), `{"path":"f.txt","edits":[{"old_string":"a","new_string":"b","replace_all":true},{"old_string":"bbb","new_string":"c"}]}`)
	if r.IsError {
		t.Fatalf("multi_edit: %s", r.Output)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "c" {
		t.Errorf("content = %q, want c", got)
	}
}
