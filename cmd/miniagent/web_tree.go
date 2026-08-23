package main

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
)

const maxTreeEntries = 500

type treeDirEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type treeResponse struct {
	Path      string         `json:"path"`
	Parent    string         `json:"parent"`
	Dirs      []treeDirEntry `json:"dirs"`
	Total     int            `json:"total"`
	Truncated bool           `json:"truncated"`
}

func (s *webServer) handleTree(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Query().Get("path")
	if p == "" || !filepath.IsAbs(p) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "path must be an absolute path"})
		return
	}
	entries, err := os.ReadDir(p)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "readdir " + p + ": " + err.Error()})
		return
	}
	parent := filepath.Dir(p)
	if parent == p {
		parent = "/"
	}
	var dirs []treeDirEntry
	total := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if e.Type()&fs.ModeSymlink != 0 {
			continue
		}
		name := e.Name()
		// skip hidden directories unless ?hidden=1
		if name[0] == '.' && r.URL.Query().Get("hidden") != "1" {
			continue
		}
		total++
		if len(dirs) < maxTreeEntries {
			dirs = append(dirs, treeDirEntry{Name: name, Path: filepath.Join(p, name)})
		}
	}
	sort.Slice(dirs, func(i, j int) bool { return dirs[i].Name < dirs[j].Name })

	resp := treeResponse{Path: p, Parent: parent, Dirs: dirs, Total: min(total, maxTreeEntries), Truncated: total > maxTreeEntries}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
