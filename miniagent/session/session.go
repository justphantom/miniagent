package session

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/justphantom/miniagent/miniagent"
)

// SessionMeta is the metadata first line of the session jsonl. Re-exported from core.
type SessionMeta = miniagent.SessionMeta

// maxSessionBytes is the default size cap for session files: 50MB covers long sessions while preventing unbounded growth.
// Overridden at runtime via Limits.MaxSessionBytes (<=0 uses this default); injected by LoadSession/AppendMessages/RewriteMessages.
const maxSessionBytes = 50 << 20 // 50MB

// metaLineMaxBytes caps the first-line read of LoadSessionMeta: the metadata line is tiny; a
// truncated (over-cap) line fails json.Unmarshal and degrades to zero meta, never a full-buffer read.
const metaLineMaxBytes = 64 << 10

const (
	sessionTypeSession = "session"
	sessionTypeMessage = "message"
)

// sessionLine is the write wrapper for message lines: embeds miniagent.Message to surface role/content/kind fields,
// and adds type=message discrimination (the read side dispatches by type to SessionMeta or miniagent.Message).
type sessionLine struct {
	Type string `json:"type"`
	miniagent.Message
}

// ResolveSessionPath validates the id (allowlist) then joins {dir}/{id}.jsonl. It only resolves the path, not file existence —
// existence semantics for new (-save-session) and resume (-session) are decided by the caller (resolveSessionForRun).
func ResolveSessionPath(arg, dir string) (string, error) {
	if arg == "" {
		return "", errors.New("session argument is empty")
	}
	if err := ValidateSessionID(arg); err != nil {
		return "", err
	}
	if dir == "" {
		return "", fmt.Errorf("session %q is valid but session.dir is not configured", arg)
	}
	return filepath.Join(dir, arg+".jsonl"), nil
}

// LoadSession reads the jsonl: the first line is session metadata (zero-value meta if absent), the rest are message lines.
// A non-existent file returns (zero meta, nil, nil), equivalent to a new session. Corruption (illegal JSON line, unknown role,
// tool message missing tool_call_id, broken pairing, exceeding size limit) returns an error; the caller should fail loudly rather
// than silently drop history. opts: opts[0] overrides the maxSessionBytes limit (<=0 or omitted falls back to the maxSessionBytes constant).
func LoadSession(path string, opts ...int64) (SessionMeta, []miniagent.Message, error) {
	mb := int64(maxSessionBytes)
	if len(opts) > 0 && opts[0] > 0 {
		mb = opts[0]
	}
	f, err := miniagent.OpenNoFollow(path, os.O_RDONLY, 0)
	if errors.Is(err, os.ErrNotExist) {
		return SessionMeta{}, nil, nil
	}
	if err != nil {
		return SessionMeta{}, nil, err
	}
	defer func() { _ = f.Close() }()
	// Single open + LimitReader: eliminates Stat/ReadFile TOCTOU, and hard-caps the read volume to prevent memory blowup.
	data, err := io.ReadAll(io.LimitReader(f, mb+1))
	if err != nil {
		return SessionMeta{}, nil, err
	}
	if int64(len(data)) > mb {
		return SessionMeta{}, nil, fmt.Errorf("session file %q exceeds size limit %d bytes", path, mb)
	}
	var meta SessionMeta
	var msgs []miniagent.Message
	sc := bufio.NewScanner(bytes.NewReader(data))
	// Per-line limit aligned with mb: prevents a single large message from triggering ErrTooLong, which would make the entire session unreadable and unfixable under append-only (P2-7).
	sc.Buffer(make([]byte, 64*1024), int(mb+1))
	var corruptLine int // pending illegal JSON line number (1-based), 0=none
	var corruptErr error
	for i := 0; sc.Scan(); i++ {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var probe struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(line, &probe); err != nil {
			// Illegal JSON line: an append-only crash only corrupts the last line; suspend and confirm whether it is the tail line. If there is already a pending line, this is mid-file corruption.
			if corruptLine != 0 {
				return SessionMeta{}, nil, fmt.Errorf("session file %q line %d illegal JSON: %w", path, corruptLine, corruptErr)
			}
			corruptLine = i + 1
			corruptErr = err
			continue
		}
		// This line is valid: if there is a pending illegal line, it is not at the end of file -> mid-file corruption, fail strictly.
		if corruptLine != 0 {
			return SessionMeta{}, nil, fmt.Errorf("session file %q line %d illegal JSON (mid-file corruption): %w", path, corruptLine, corruptErr)
		}
		if probe.Type == sessionTypeSession {
			if err := json.Unmarshal(line, &meta); err != nil {
				return SessionMeta{}, nil, fmt.Errorf("session file %q metadata line parse failed: %w", path, err)
			}
			continue
		}
		// message line (type=message or legacy with no type): deserialize into miniagent.Message, unknown fields ignored.
		var m miniagent.Message
		//nolint:musttag // miniagent.Message already has json tags; sessionLine embedding miniagent.Message is a session file format convention (not wire)
		if err := json.Unmarshal(line, &m); err != nil {
			return SessionMeta{}, nil, fmt.Errorf("session file %q line %d parse failed: %w", path, i+1, err)
		}
		if err := validateSessionMessage(m); err != nil {
			return SessionMeta{}, nil, fmt.Errorf("session file %q message %d is invalid: %w", path, i+1, err)
		}
		msgs = append(msgs, m)
	}
	if err := sc.Err(); err != nil {
		return SessionMeta{}, nil, fmt.Errorf("failed to read session file %q: %w", path, err)
	}
	// Scan finished: corruptLine still non-zero -> last line is half-written (append-only crash residual), tolerate and discard it,
	// return the valid history so far. validateToolPairing still runs strictly: if the residual line broke pairing, report a clear error.
	if err := miniagent.ValidateToolPairing(msgs); err != nil {
		return SessionMeta{}, nil, fmt.Errorf("session file %q: %w", path, err)
	}
	return meta, msgs, nil
}

// LoadSessionMeta reads only the first-line metadata of the jsonl (zero-value meta if absent,
// non-existent file included). Listing callers must not pay LoadSession's full-file read per entry.
// No corruption checks: the message body is not read here; LoadSession remains the strict validator.
func LoadSessionMeta(path string) (SessionMeta, error) {
	f, err := miniagent.OpenNoFollow(path, os.O_RDONLY, 0)
	if errors.Is(err, os.ErrNotExist) {
		return SessionMeta{}, nil
	}
	if err != nil {
		return SessionMeta{}, err
	}
	defer func() { _ = f.Close() }()
	line, err := bufio.NewReader(io.LimitReader(f, metaLineMaxBytes)).ReadBytes('\n')
	// A missing trailing '\n' (single-line file) still yields the bytes with io.EOF — that is meta material.
	if err != nil && (len(line) == 0 || !errors.Is(err, io.EOF)) {
		return SessionMeta{}, err
	}
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(line), &probe); err != nil || probe.Type != sessionTypeSession {
		return SessionMeta{}, nil // first line is a message or unparseable (crash residue): no meta to report
	}
	var meta SessionMeta
	if err := json.Unmarshal(bytes.TrimSpace(line), &meta); err != nil {
		return SessionMeta{}, nil
	}
	return meta, nil
}

// AppendMessages append-only appends msgs to the jsonl (writes the metadata line first when creating/empty). Write-side guardrails: flock
// cross-process lock prevents line-boundary interleaving illegal JSON (P2-13); pre-serialization rejects when size+pending exceeds limit, avoiding
// successful write that fails later at LoadSession causing permanent stuck state (P1-4); ensureTrailingNewline before write truncates crash half-written residual lines (H3-1). withSessionLock
// unifies O_NOFOLLOW + MkdirAll 0o700 + flock (P3). opts: opts[0] overrides maxSessionBytes limit (<=0 or omitted falls back to the constant).
func AppendMessages(path string, meta SessionMeta, msgs []miniagent.Message, opts ...int64) error {
	mb := int64(maxSessionBytes)
	if len(opts) > 0 && opts[0] > 0 {
		mb = opts[0]
	}
	if len(msgs) == 0 {
		return nil
	}
	// O_RDWR: needs to read the last byte before writing to detect and truncate crash half-written tail incomplete lines (ensureTrailingNewline).
	return withSessionLock(path, os.O_APPEND|os.O_CREATE|os.O_RDWR, func(f *os.File) error {
		info, err := f.Stat()
		if err != nil {
			return err
		}
		// Files already exceeding mb are not healed: ensureTrailingNewline slow path LimitReader(mb+1) only reads the first
		// mb+1 bytes when size>mb, LastIndexByte mislocates in the incomplete window and would truncate and drop legal lines with no rollback (R4-1). Consistent with LoadSession
		// -> directly report error, zero modification to the file.
		if info.Size() > mb {
			return fmt.Errorf("session file %q has reached %d bytes, exceeds limit %d (please compact history or create a new session)", path, info.Size(), mb)
		}
		// Truncate crash half-written tail incomplete lines: otherwise O_APPEND blind write would concatenate new messages onto a residual line without a trailing newline,
		// turning a residual line tolerated by LoadSession's last-line tolerance into mid-file corruption on subsequent saves (permanently losing the session).
		size, err := ensureTrailingNewline(f, info.Size(), mb)
		if err != nil {
			return err
		}
		// Pre-serialize pending content: both for precise size estimation on the write side and to reuse a single marshal avoiding duplicate work.
		var buf bytes.Buffer
		if size == 0 {
			if meta.Type == "" {
				meta.Type = sessionTypeSession
			}
			b, err := json.Marshal(meta)
			if err != nil {
				return err
			}
			buf.Write(b)
			buf.WriteByte('\n')
		}
		for _, m := range msgs {
			//nolint:musttag // miniagent.Message already has json tags; sessionLine embedding miniagent.Message is a session file format convention (not wire)
			b, err := json.Marshal(sessionLine{Type: sessionTypeMessage, Message: m})
			if err != nil {
				return err
			}
			buf.Write(b)
			buf.WriteByte('\n')
		}
		if size+int64(buf.Len()) > mb {
			return fmt.Errorf("session file %q would reach %d bytes after append, exceeds limit %d (please compact history or create a new session)", path, size+int64(buf.Len()), mb)
		}
		w := bufio.NewWriter(f)
		if _, err := w.Write(buf.Bytes()); err != nil {
			return err
		}
		if err := w.Flush(); err != nil {
			return err
		}
		// Sync flush reduces the "written residual line + unflushed" crash window (combined with LoadSession tail-line tolerance).
		return f.Sync()
	})
}

// ensureTrailingNewline truncates crash half-written trailing incomplete lines: O_APPEND blind write would concatenate new messages onto a
// line with no trailing newline and break line boundaries. Fast path only reads the last byte; only when the file does not end with '\n'
// (rare recovery scenario) does it scan back to the last '\n' and truncate the bytes after it, returning the truncated file size for the caller to decide
// whether a metadata header line needs to be rewritten.
func ensureTrailingNewline(f *os.File, size, mb int64) (int64, error) {
	if size == 0 {
		return 0, nil
	}
	var last [1]byte
	if _, err := f.ReadAt(last[:], size-1); err != nil {
		return size, err
	}
	if last[0] == '\n' {
		return size, nil
	}
	// Residual line has no trailing newline: read existing content (capped at mb) to locate the last '\n' and truncate the bytes after it.
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return size, err
	}
	data, err := io.ReadAll(io.LimitReader(f, mb+1))
	if err != nil {
		return size, err
	}
	cutAt := int64(0)
	if idx := bytes.LastIndexByte(data, '\n'); idx >= 0 {
		cutAt = int64(idx) + 1
	}
	if err := f.Truncate(cutAt); err != nil {
		return size, err
	}
	return cutAt, nil
}
