package tools

import (
	miniagent "github.com/justphantom/miniagent/internal/miniagent"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

const maxEditFileBytes = 10 << 20

// pathLocks serializes read-modify-write edits to the same path within one process, preventing lost updates when runToolsParallel runs parallel edits on the same file
// (A and B each read the old content then write, and the later write overwrites the earlier one). write is an atomic overwrite (writeFileAtomic) and needs no lock;
// cross-process file races are out of this tool's scope (multiple writers in the same session should be avoided anyway). Not package-level: each
// EditFileTool instance holds its own copy, shared by its sole Call closure across parallel invocations of that tool, conforming to the "no package-level mutable state" rule.
// Tradeoff: the map is never reclaimed — long sessions editing many distinct files accumulate entries (one mutex each); a single-process CLI session is
// short-lived, so this tradeoff is acceptable (cross-process / long-running fork scenarios are the caller's responsibility).
type pathLocks struct {
	mu sync.Mutex
	m  map[string]*sync.Mutex
}

// acquire takes (creating if necessary) the mutex for the path, locks it, and returns an unlock function.
func (p *pathLocks) acquire(path string) func() {
	p.mu.Lock()
	pm, ok := p.m[path]
	if !ok {
		pm = &sync.Mutex{}
		if p.m == nil {
			p.m = make(map[string]*sync.Mutex)
		}
		p.m[path] = pm
	}
	p.mu.Unlock()
	pm.Lock()
	return pm.Unlock
}

type editFileArgs struct {
	Path       string `json:"path"`
	OldString  string `json:"old_string,omitempty"`
	NewString  string `json:"new_string,omitempty"`
	ReplaceAll bool   `json:"replace_all,omitempty"`
	// When Edits is non-empty, runs a transactional multi-segment replace (former multi_edit semantics), mutually exclusive with old_string/new_string.
	Edits []editOne `json:"edits,omitempty"`
}

// editOne is one entry of the edits array: applied in order, each match based on the previous result.
type editOne struct {
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all,omitempty"`
}

// EditFileTool precisely replaces text in a file: single segment (old_string/new_string) or multi-segment transaction (edits array).
// old_string must match the file exactly; by default it must be unique (0 or multiple matches fail), replace_all=true replaces all occurrences.
// When edits is non-empty it applies them as an ordered transaction, writing to disk only if all succeed and leaving the file untouched if any fails. Rejects symlinks and non-regular files.
// timeout<=0 uses the default fileOpTimeout.
func EditFileTool(workspaceRoot string, timeout time.Duration) miniagent.Tool {
	if timeout <= 0 {
		timeout = fileOpTimeout
	}
	locks := &pathLocks{} // held by the sole closure of this edit tool, shared across parallel invocations, serializing read-modify-write on the same path
	return miniagent.Tool{
		Name:        "edit",
		Description: "Precisely replace one or more segments of text in a file. Single segment: old_string+new_string (unique match required by default, replace_all=true replaces all). Multi-segment transaction: the edits array is applied in order, written to disk only if all succeed, left unchanged if any fails. old_string must match the file exactly (including indentation and newlines). Rejects symlinks and non-regular files. Read the file to view its contents before editing.",
		Parameters: object(map[string]any{
			"path":        map[string]any{"type": "string", "description": "Path of the file to edit, relative to workdir or absolute"},
			"old_string":  map[string]any{"type": "string", "description": "The original text to be replaced (must match the file contents exactly, including indentation and newlines)"},
			"new_string":  map[string]any{"type": "string", "description": "The new text to replace with"},
			"replace_all": map[string]any{"type": "boolean", "description": "When true, replaces all matches; default (false) requires old_string to match uniquely"},
			"edits": map[string]any{
				"type":        "array",
				"description": "Multi-segment transactional replacement list (mutually exclusive with old_string/new_string); applied in order, written to disk only if all succeed",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"old_string":  map[string]any{"type": "string", "description": "The original text to be replaced (exact match)"},
						"new_string":  map[string]any{"type": "string", "description": "The new text to replace with"},
						"replace_all": map[string]any{"type": "boolean", "description": "true replaces all matches at this entry; default requires a unique match"},
					},
					"required": []string{"old_string", "new_string"},
				},
			},
		}, "path"),
		ResultLimit: maxFileResultInHistory,
		Call: func(ctx context.Context, args string) miniagent.ToolResult {
			return runWithTimeout(ctx, timeout, "edit", func(_ context.Context) miniagent.ToolResult { return runEditFile(workspaceRoot, args, locks) })
		},
	}
}

func runEditFile(workspaceRoot, args string, locks *pathLocks) miniagent.ToolResult {
	a, err := parseEditArgs(args)
	if err != nil {
		return miniagent.ToolResult{IsError: true, Output: err.Error()}
	}
	full := resolveToolPath(workspaceRoot, a.Path)
	info, err := os.Lstat(full)
	if err != nil {
		return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("failed to read %q: %v", a.Path, err)}
	}
	if !info.Mode().IsRegular() {
		// Reject non-regular files: FIFO/character devices make openNoFollow block permanently when there is no writer,
		// and directories/sockets likewise; aligned with the read tool.
		return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("%q is not a regular file (mode=%s); only regular files are supported", a.Path, info.Mode().String())}
	}
	if info.Size() > maxEditFileBytes {
		return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("file %q exceeds the maximum edit limit of %d bytes", a.Path, maxEditFileBytes)}
	}
	// Hold the lock across the entire read-modify-write: prevents a later parallel edit to the same file from overwriting an earlier write (lost update).
	// defer releases it after applyEdit/applyEdits returns (including on error).
	defer locks.acquire(full)()
	if len(a.Edits) > 0 {
		return applyEdits(full, info, a.Path, a.Edits)
	}
	return applyEdit(full, info, a)
}

// parseEditArgs validates both the single-segment and multi-segment forms: edits is mutually exclusive with old_string/new_string (passing both is an error).
// The single-segment form requires old_string to be non-empty and different from new_string; each multi-segment entry likewise.
func parseEditArgs(args string) (editFileArgs, error) {
	var a editFileArgs
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return editFileArgs{}, fmt.Errorf("argument parsing failed: %w (received %q)", err, args)
	}
	if a.Path == "" {
		return editFileArgs{}, errors.New("missing argument: path")
	}
	hasSingle := a.OldString != "" || a.NewString != ""
	if len(a.Edits) > 0 && hasSingle {
		return editFileArgs{}, errors.New("edits is mutually exclusive with old_string/new_string (pick one)")
	}
	if len(a.Edits) > 0 {
		for i, e := range a.Edits {
			if e.OldString == "" {
				return editFileArgs{}, fmt.Errorf("entry %d old_string is empty", i+1)
			}
			if e.OldString == e.NewString {
				return editFileArgs{}, fmt.Errorf("entry %d old_string is identical to new_string", i+1)
			}
		}
		return a, nil
	}
	if a.OldString == "" {
		return editFileArgs{}, errors.New("missing argument: old_string (cannot be empty)")
	}
	if a.OldString == a.NewString {
		return editFileArgs{}, errors.New("old_string is identical to new_string; no replacement needed")
	}
	return a, nil
}

// checkEditSize pre-checks replacement amplification exceeding the output limit: a short old + long new + multiple matches (replaceAll), or a single long new (single segment),
// can make the output far exceed the read-side maxBytes cap; strings.Replace/ReplaceAll builds a giant string in memory causing OOM. Estimate via count*(len(new)-len(old)):
// replaceAll uses the actual hit count, single segment counts as 1 (applyOne already guarantees a unique single-segment hit). Reject before the allocation if it exceeds maxBytes.
func checkEditSize(content, old, newText string, replaceAll bool, maxBytes int) error {
	count := 1
	if replaceAll {
		count = strings.Count(content, old)
	}
	est := len(content) + count*(len(newText)-len(old))
	if est > maxBytes {
		return fmt.Errorf("replacement would produce about %d bytes (exceeds %d limit); write refused", est, maxBytes)
	}
	return nil
}

// applyOne applies one replacement to content, returning the new content and the hit count. Pure in-memory, does not write to disk.
// Returns (content, 0, error) on 0 hits; returns (content, n, error) when not replaceAll and multiple hits occur.
// The caller uses count to distinguish "not found" from "multiple matches" to give a specific message. The edits transaction reuses this for each segment.
func applyOne(content, old, newText string, replaceAll bool) (string, int, error) {
	count := strings.Count(content, old)
	if count == 0 {
		return content, 0, errors.New("not found")
	}
	if !replaceAll && count > 1 {
		return content, count, fmt.Errorf("occurs %d times", count)
	}
	if replaceAll {
		return strings.ReplaceAll(content, old, newText), count, nil
	}
	return strings.Replace(content, old, newText, 1), count, nil
}

func applyEdit(full string, info os.FileInfo, a editFileArgs) miniagent.ToolResult {
	// openNoFollow only rejects the final path component being a symlink (O_NOFOLLOW); intermediate directories
	// are not resolved or validated and do not form a path boundary (free mode, see README).
	f, err := openNoFollow(full, os.O_RDONLY, 0)
	if err != nil {
		return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("failed to read %q: %v", a.Path, err)}
	}
	defer func() { _ = f.Close() }()
	// Read cap: a TOCTOU exists between Lstat's Size and ReadAll (the file is concurrently swapped for larger
	// content or is a character device); guard against unbounded allocation using the actual bytes read; aligned with the read tool.
	data, err := io.ReadAll(io.LimitReader(f, maxEditFileBytes+1))
	if err != nil {
		return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("failed to read %q: %v", a.Path, err)}
	}
	if int64(len(data)) > maxEditFileBytes {
		return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("file %q read exceeds the maximum edit limit of %d bytes", a.Path, maxEditFileBytes)}
	}
	content := string(data)
	if cerr := checkEditSize(content, a.OldString, a.NewString, a.ReplaceAll, maxEditFileBytes); cerr != nil {
		return miniagent.ToolResult{IsError: true, Output: cerr.Error()}
	}
	updated, count, err := applyOne(content, a.OldString, a.NewString, a.ReplaceAll)
	if err != nil {
		// Keep the specific message: not found vs multiple matches, with the filename and a read suggestion.
		if count == 0 {
			return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("old_string not found in %q. The file may have been modified; please read it first to view the current contents.", a.Path)}
		}
		return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("old_string occurs %d times in %q. Please provide more context (enlarge the old_string range) so it matches uniquely, or set replace_all=true to replace all occurrences.", count, a.Path)}
	}
	mode := os.FileMode(0o644)
	if info != nil {
		mode = info.Mode().Perm()
	}
	if err := writeFileAtomic(full, []byte(updated), mode); err != nil {
		return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("failed to write %q: %v", a.Path, err)}
	}
	return miniagent.ToolResult{Output: fmt.Sprintf("replaced %d segment(s) of text in %q (%d → %d bytes)", count, a.Path, len(content), len(updated))}
}

// applyEdits applies multi-segment replacements in order (transaction: written to disk only if all succeed; on any failure the file is unchanged).
// Each segment matches against the previous segment's result; open+read is the same as applyEdit, reusing applyOne in the loop.
func applyEdits(full string, info os.FileInfo, path string, edits []editOne) miniagent.ToolResult {
	f, err := openNoFollow(full, os.O_RDONLY, 0)
	if err != nil {
		return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("failed to read %q: %v", path, err)}
	}
	data, err := io.ReadAll(io.LimitReader(f, maxEditFileBytes+1))
	_ = f.Close()
	if err != nil {
		return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("failed to read %q: %v", path, err)}
	}
	if int64(len(data)) > maxEditFileBytes {
		return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("file %q read exceeds the maximum edit limit of %d bytes", path, maxEditFileBytes)}
	}
	updated := string(data)
	originLen := len(updated)
	totalMatches := 0
	for i, e := range edits {
		if cerr := checkEditSize(updated, e.OldString, e.NewString, e.ReplaceAll, maxEditFileBytes); cerr != nil {
			return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("segment %d replacement failed: %s (file %q)", i+1, cerr, path)}
		}
		u, count, aerr := applyOne(updated, e.OldString, e.NewString, e.ReplaceAll)
		if aerr != nil {
			return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("segment %d replacement failed: %s (file %q, %d matches)", i+1, aerr, path, count)}
		}
		updated = u
		totalMatches += count
	}
	mode := os.FileMode(0o644)
	if info != nil {
		mode = info.Mode().Perm()
	}
	if err := writeFileAtomic(full, []byte(updated), mode); err != nil {
		return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("failed to write %q: %v", path, err)}
	}
	return miniagent.ToolResult{Output: fmt.Sprintf("applied %d replacement(s) in %q (%d total matches, %d → %d bytes)", len(edits), path, totalMatches, originLen, len(updated))}
}
