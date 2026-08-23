package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestTree_ListDirs(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "a"), 0755)
	os.MkdirAll(filepath.Join(dir, "b"), 0755)
	os.WriteFile(filepath.Join(dir, "file.txt"), []byte("x"), 0644)
	os.MkdirAll(filepath.Join(dir, ".hidden"), 0755)
	s := &webServer{}
	req := httptest.NewRequestWithContext(context.Background(), "GET", "/api/tree?path="+dir, nil)
	w := httptest.NewRecorder()
	s.handleTree(w, req)
	if w.Code != 200 {
		t.Fatalf("code = %d, want 200", w.Code)
	}
	var resp treeResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if len(resp.Dirs) != 2 {
		t.Errorf("dirs = %d, want 2 (no hidden, no files)", len(resp.Dirs))
	}
	if resp.Total != 2 {
		t.Errorf("total = %d, want 2", resp.Total)
	}
	if resp.Truncated {
		t.Error("truncated should be false")
	}
	names := []string{resp.Dirs[0].Name, resp.Dirs[1].Name}
	if names[0] != "a" || names[1] != "b" {
		t.Errorf("dirs not sorted: %v", names)
	}
}

func TestTree_ParentPath(t *testing.T) {
	dir := t.TempDir()
	deep := filepath.Join(dir, "x", "y")
	os.MkdirAll(deep, 0755)
	s := &webServer{}
	req := httptest.NewRequestWithContext(context.Background(), "GET", "/api/tree?path="+deep, nil)
	w := httptest.NewRecorder()
	s.handleTree(w, req)
	var resp treeResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if resp.Parent != filepath.Join(dir, "x") {
		t.Errorf("parent = %q, want %q", resp.Parent, filepath.Join(dir, "x"))
	}
	// root parent
	req2 := httptest.NewRequestWithContext(context.Background(), "GET", "/api/tree?path=/", nil)
	w2 := httptest.NewRecorder()
	s.handleTree(w2, req2)
	var resp2 treeResponse
	json.NewDecoder(w2.Body).Decode(&resp2)
	if resp2.Parent != "/" {
		t.Errorf("root parent = %q, want /", resp2.Parent)
	}
}

func TestTree_RejectsRelative(t *testing.T) {
	s := &webServer{}
	req := httptest.NewRequestWithContext(context.Background(), "GET", "/api/tree?path=foo", nil)
	w := httptest.NewRecorder()
	s.handleTree(w, req)
	if w.Code != 400 {
		t.Errorf("code = %d, want 400", w.Code)
	}
}

func TestTree_MissingDir(t *testing.T) {
	s := &webServer{}
	req := httptest.NewRequestWithContext(context.Background(), "GET", "/api/tree?path=/definitely/not/exist", nil)
	w := httptest.NewRecorder()
	s.handleTree(w, req)
	if w.Code != 404 {
		t.Errorf("code = %d, want 404", w.Code)
	}
}

func TestTree_SkipsSymlink(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "real"), 0755)
	os.Symlink(filepath.Join(dir, "real"), filepath.Join(dir, "link"))
	s := &webServer{}
	req := httptest.NewRequestWithContext(context.Background(), "GET", "/api/tree?path="+dir, nil)
	w := httptest.NewRecorder()
	s.handleTree(w, req)
	var resp treeResponse
	json.NewDecoder(w.Body).Decode(&resp)
	for _, d := range resp.Dirs {
		if d.Name == "link" {
			t.Error("symlink should be skipped")
		}
	}
}

func TestTree_Truncates(t *testing.T) {
	dir := t.TempDir()
	for i := range 510 {
		os.MkdirAll(filepath.Join(dir, fmt.Sprintf("d%04d", i)), 0755)
	}
	s := &webServer{}
	req := httptest.NewRequestWithContext(context.Background(), "GET", "/api/tree?path="+dir, nil)
	w := httptest.NewRecorder()
	s.handleTree(w, req)
	var resp treeResponse
	json.NewDecoder(w.Body).Decode(&resp)
	if !resp.Truncated {
		t.Error("truncated should be true")
	}
	if resp.Total != 500 {
		t.Errorf("total = %d, want 500", resp.Total)
	}
	if len(resp.Dirs) > 500 {
		t.Errorf("dirs len = %d, want <= 500", len(resp.Dirs))
	}
}
