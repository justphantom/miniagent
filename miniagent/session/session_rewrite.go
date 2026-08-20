package session

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/justphantom/miniagent/miniagent"
)

// RewriteMessages performs a full rewrite of the session file (write temp file -> os.Rename atomic swap). Invoked by the cmd save path on every
// turn — the first meta line (LLMRequests) accumulates across turns and append-only cannot rewrite it; compaction additionally relies on the same
// rewrite to truly discard the barriered middle (review P2 session file never compacts). msgs is the full transcript; lock and temp-file strategy
// see withSessionLock; write/rename failures clean up temp files. After rename the next LoadSession reads the slimmed file.
// opts: opts[0] overrides the maxSessionBytes limit (<=0 or omitted falls back to the maxSessionBytes constant).
func RewriteMessages(path string, meta SessionMeta, msgs []miniagent.Message, opts ...int64) error {
	mb := int64(maxSessionBytes)
	if len(opts) > 0 && opts[0] > 0 {
		mb = opts[0]
	}
	var buf bytes.Buffer
	if meta.Type == "" {
		meta.Type = sessionTypeSession
	}
	b, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	buf.Write(b)
	buf.WriteByte('\n')
	for _, m := range msgs {
		//nolint:musttag // sessionLine embedding miniagent.Message is a session file format convention (not wire)
		b, err := json.Marshal(sessionLine{Type: sessionTypeMessage, Message: m})
		if err != nil {
			return err
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	if int64(buf.Len()) > mb {
		return fmt.Errorf("session rewrite exceeds %d bytes limit %d", buf.Len(), mb)
	}
	dir := filepath.Dir(path)
	return withSessionLock(path, os.O_WRONLY|os.O_CREATE, func(*os.File) error {
		// Temp file is in the same directory as path (ensures rename is atomic on the same filesystem); os.CreateTemp defaults to 0o600.
		tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
		if err != nil {
			return err
		}
		tmpPath := tmp.Name()
		_, writeErr := tmp.Write(buf.Bytes())
		if writeErr == nil {
			writeErr = tmp.Sync()
		}
		_ = tmp.Close()
		if writeErr != nil {
			_ = os.Remove(tmpPath)
			return writeErr
		}
		// rename atomically swaps path: the fd held by withSessionLock now points to the unlinked old inode, defer unlock/close still works correctly (closing fd releases lock); the next round takes the lock on the new inode.
		// rename failure (permission/disk full/filesystem error) also cleans up tmp, consistent with write/sync failure (comment promises "write/rename failures both clean up").
		if err := os.Rename(tmpPath, path); err != nil {
			_ = os.Remove(tmpPath)
			return err
		}
		// fsync the parent directory to commit rename's directory metadata: prevents a crash falling in the window between rename and directory metadata commit causing rewrite loss
		// (a different dimension from the reported flock+rename concurrency issue — this is crash durability). Failure is best-effort (rename already succeeded).
		if d, derr := os.Open(dir); derr == nil {
			_ = d.Sync()
			_ = d.Close()
		}
		return nil
	})
}
