package policy

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// toolOutputRetentionDefault is the default retention time for persisted tool output (taken from opencode RETENTION=7d).
const toolOutputRetentionDefault = 7 * 24 * time.Hour

// toolOutputMaxBytes is the byte hard cap for a single persisted file (prevents OOM; above the shell entry 100KB cap, with headroom).
const toolOutputMaxBytes = 1 << 20 // 1 MiB

// toolOutputMaxFiles is the count hard cap on persisted tool_*.txt files kept under dir. cleanup evicts the oldest-mtime
// files beyond this cap even when not yet expired by retention, bounding the steady-state disk footprint across processes:
// a single run's own accumulation is reclaimed at the start of the next process (cleanup runs once at construction), and
// without this cap dense large-output runs would accumulate without bound within the retention window (RL-3 / §P1-3).
// Eviction is oldest-mtime-first so the most recently saved files — those the model most recently got a "full output
// saved" hint for and may still read back via read/grep — survive (read-back contract safe). 500 leaves headroom over a
// typical run and caps worst-case disk at ~500 × toolOutputMaxBytes.
const toolOutputMaxFiles = 500

// toolOutputStore is the on-disk storage for tool output; private; constructed once at the Run entry and reused across steps.
// Tool output exceeding the limit is written to disk in full; the history Content is replaced with "existing preview +
// absolute path hint", letting the model use the existing read(offset/limit)/grep to read it back on demand, instead of
// permanently losing the full text (§P1-A, fixes the hard loss of trimForHistory's one-shot hard truncation).
type toolOutputStore struct {
	dir       string        // on-disk root directory; verified non-empty at construction time
	retention time.Duration // expiry cleanup threshold; <=0 uses toolOutputRetentionDefault
	counter   atomic.Int64  // collision resolution for the same (step,callID)
	logger    *slog.Logger
}

// newToolOutputStore is the Run entry constructor. retention<=0 falls back to toolOutputRetentionDefault. dir is checked for emptiness by the caller.
func newToolOutputStore(dir string, retention time.Duration, logger *slog.Logger) *toolOutputStore {
	if retention <= 0 {
		retention = toolOutputRetentionDefault
	}
	return &toolOutputStore{dir: dir, retention: retention, logger: logger}
}

// bound is the core method. truncated=false returns preview as-is (no persistence). truncated=true: writes output
// (byte-capped to toolOutputMaxBytes) to <dir>/tool_<step>_<sanitize(callID)>_<counter>.txt (O_CREATE|O_EXCL 0o600,
// mirroring session.go write-side guard; on failure best-effort log warn and returns preview without a marker, equivalent
// to the current hard truncation); on success returns preview + path hint (model reads back via read(offset/limit)/grep).
func (s *toolOutputStore) bound(step int, callID, output, preview string, truncated bool) string {
	if !truncated {
		return preview
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		s.warnf("tool-output mkdir failed: %v", err)
		return preview
	}
	name := fmt.Sprintf("tool_%d_%s_%d.txt", step, sanitizeFileSegment(callID), s.counter.Add(1))
	path := filepath.Join(s.dir, name)
	data := []byte(output)
	byteCapped := false
	if len(data) > toolOutputMaxBytes {
		// Above the 1 MiB byte hard cap: fall back to the nearest UTF-8 rune boundary then truncate, ensuring the persisted
		// file is always valid UTF-8 (otherwise a multi-byte rune is split in the middle, the model read-back gets garbled
		// text, and feeding it back to the API may trigger a 400).
		byteCapped = true
		end := toolOutputMaxBytes
		for end > 0 && !utf8.RuneStart(data[end]) {
			end--
		}
		data = data[:end]
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		s.warnf("tool-output create failed: %v", err)
		return preview
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		s.warnf("tool-output write failed: %v", err)
		return preview
	}
	// Sync fulfills the semantics of the "full output saved" marker below: after a crash the model reads back
	// complete data rather than an empty/residual file.
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		s.warnf("tool-output sync failed: %v", err)
		return preview
	}
	if err := f.Close(); err != nil {
		// Align with write-failure semantics: on close failure delete the orphan file and return preview without a marker,
		// to avoid the model reading back a residual file based on the marker.
		_ = os.Remove(path)
		s.warnf("tool-output close failed: %v", err)
		return preview
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	// The persisted file may have been truncated by the 1 MiB byte cap (byteCapped) — in that case do not claim "complete",
	// otherwise the model reads back truncated data but believes it is the full text.
	marker := "…[full output saved"
	if byteCapped {
		marker = "…[output saved (over 1 MiB, partially truncated)"
	}
	return preview + "\n\n" + marker + ": " + abs + "; read it back via read(offset/limit) or grep, do not read the entire file]"
}

// cleanup scans tool_*.txt under dir and:
//  1. removes files with mtime < now-retention (age-based expiry);
//  2. enforces toolOutputMaxFiles: if more files remain, evicts oldest-mtime-first down to the cap — bounding disk
//     usage when dense large-output runs accumulate across turns within the retention window (RL-3 / §P1-3).
//
// Eviction is oldest-mtime-first to protect the read-back contract: the most recently saved files (those the model most
// recently got a "full output saved: <abs>" hint for and may still read back via read/grep) are kept; only the oldest are
// removed. cleanup runs once at process start, before the current run writes anything, so no current-run file is present.
// Best-effort (on failure only warn). Run calls it once opportunistically at startup, fitting the single-run CLI form
// (no background timer); every new prompt is a new process, so this self-heals residue.
func (s *toolOutputStore) cleanup() {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return // directory does not exist etc.: no persisted files to clean, silent.
	}
	cutoff := time.Now().Add(-s.retention)
	type keptEntry struct {
		name    string
		modTime time.Time
	}
	var kept []keptEntry
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "tool_") || !strings.HasSuffix(e.Name(), ".txt") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		// Age-based expiry first.
		if info.ModTime().Before(cutoff) {
			if err := os.Remove(filepath.Join(s.dir, e.Name())); err != nil && s.logger != nil {
				s.logger.Warn("tool-output cleanup remove failed", "file", e.Name(), "error", err)
			}
			continue
		}
		kept = append(kept, keptEntry{name: e.Name(), modTime: info.ModTime()})
	}
	// Count cap: evict oldest-mtime-first down to toolOutputMaxFiles (contract-safe; see const doc).
	if len(kept) <= toolOutputMaxFiles {
		return
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].modTime.Before(kept[j].modTime) })
	surplus := len(kept) - toolOutputMaxFiles
	for i := range surplus {
		if err := os.Remove(filepath.Join(s.dir, kept[i].name)); err != nil && s.logger != nil {
			s.logger.Warn("tool-output cleanup remove failed", "file", kept[i].name, "error", err)
		}
	}
}

// sanitizeFileSegment compresses callID into a filename-safe segment: keeps [A-Za-z0-9_-], replaces the rest with '_',
// truncates to <=32 bytes (b.Len() counts bytes, not runes; a multi-byte rune may make the segment slightly exceed 32B,
// the counter suffix resolves collisions). Prevents path traversal (strips / .. etc.).
func sanitizeFileSegment(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
		if b.Len() >= 32 {
			break
		}
	}
	return b.String()
}

func (s *toolOutputStore) warnf(format string, args ...any) {
	if s.logger != nil {
		s.logger.Warn(fmt.Sprintf(format, args...))
	}
}
