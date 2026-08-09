package tools

import (
	"os"
	"strings"

	"github.com/justphantom/miniagent/internal/text"
)

// chunkBuf is a single raw fragment of the streaming accumulator; text and byte
// size are tracked separately to avoid repeated len([]byte) when the sliding window
// evicts (mirrors opencode shell.ts {text,size}). Private, used only within output_accum.go.
type chunkBuf struct {
	text string
	size int
}

// outputAccum accumulates stdout+stderr via a byte sliding window during command
// execution: only the most recent `keep` bytes (tail) are kept in memory; once the
// window is exceeded the oldest chunks are evicted and `cut` is set (drops the middle,
// keeps the tail — exactly preserving shell error/exit code). When headSpillBytes>0,
// exceeding the threshold persists the head to disk via O_APPEND (phase-2, off by
// default). Private, constructed inside runShellLimited.
type outputAccum struct {
	keep           int        // sliding window byte cap (keeps tail); <=0 unlimited
	headSpillBytes int        // spill-to-disk threshold; <=0 disabled (phase-1 default)
	spillDir       string     // spill directory
	spillPrefix    string     // spill filename prefix
	chunks         []chunkBuf // oldest to newest
	used           int        // total bytes in chunks
	total          int        // cumulative bytes read (including evicted middle)
	cut            bool       // whether the middle was dropped
	file           string     // spill file path (when spill is on)
	sink           *os.File   // spill handle
}

// newOutputAccum constructs the accumulator. keep<=0 means unlimited;
// headSpillBytes<=0 disables spill (phase-1 default).
// headSpillBytes must be <= keep (otherwise the sliding window evicts the head before
// the spill threshold triggers, losing the head entirely — contradicting the "head
// persisted" promise); when keep<=0 (unlimited window, no eviction) any positive
// headSpillBytes is safe. Violations are clamped to keep (the only current call site
// passes 0).
func newOutputAccum(keep, headSpillBytes int, spillDir, spillPrefix string) *outputAccum {
	if keep > 0 && headSpillBytes > keep {
		headSpillBytes = keep
	}
	return &outputAccum{keep: keep, headSpillBytes: headSpillBytes, spillDir: spillDir, spillPrefix: spillPrefix}
}

// write pushes a chunk: accumulates total, appends to chunks, spills when over
// headSpillBytes (first dump writes the entire head, subsequent ones append), then
// while used>keep && len>1 evicts chunks[0] and sets cut=true.
// A single chunk exceeding keep with len==1 is not evicted (guards against empty
// sliding window). Spill failures are surfaced best-effort (caller catches without
// aborting).
func (a *outputAccum) write(chunk string) error {
	if len(chunk) == 0 {
		return nil
	}
	a.chunks = append(a.chunks, chunkBuf{text: chunk, size: len(chunk)})
	a.used += len(chunk)
	a.total += len(chunk)
	// Spill: when cumulative bytes cross the threshold, create the sink on first time
	// and dump the entire current head, then append each subsequent chunk.
	// phase-1 with headSpillBytes<=0 keeps this disabled.
	if a.headSpillBytes > 0 {
		if a.sink == nil && a.total >= a.headSpillBytes {
			if err := a.createSink(); err != nil {
				return err
			}
			for _, c := range a.chunks {
				if _, err := a.sink.WriteString(c.text); err != nil {
					return err
				}
			}
			a.file = a.sink.Name() // set file only after the first full dump succeeds, so finalize can decide whether the banner marks "full output: <file>"
		} else if a.sink != nil {
			if _, err := a.sink.WriteString(chunk); err != nil {
				return err
			}
		}
	}
	// Sliding window: when over keep, evict from the oldest (drop middle, keep tail).
	// A single chunk exceeding keep with len==1 is not evicted (guards against empty window).
	for a.keep > 0 && a.used > a.keep && len(a.chunks) > 1 {
		old := a.chunks[0]
		a.chunks = a.chunks[1:]
		a.used -= old.size
		a.cut = true
	}
	return nil
}

// createSink creates a temp file on first spill (os.CreateTemp defaults to 0600),
// sets sink and cut.
// It does NOT set a.file here — write sets it after the first full chunk dump succeeds;
// otherwise, if the dump fails midway, finalize would still mark "full output: <file>"
// pointing at a truncated file.
func (a *outputAccum) createSink() error {
	f, err := os.CreateTemp(a.spillDir, a.spillPrefix+"*.log")
	if err != nil {
		return err
	}
	a.sink = f
	a.cut = true
	return nil
}

// closeSink idempotently closes the spill handle; called before finalize to ensure
// data is persisted.
func (a *outputAccum) closeSink() error {
	if a.sink == nil {
		return nil
	}
	err := a.sink.Close()
	a.sink = nil
	return err
}

// finalize returns the final Output: when cut, prepends
// "…[output over limit, only tail kept[, full output: <file>]]\n", then joins the chunks
// and applies text.TruncateTail(maxChars) as a fallback. Replaces the old
// truncate(out.String(),shellOutputChars(),"…").
func (a *outputAccum) finalize(maxChars int) string {
	body := strings.Join(chunkTexts(a.chunks), "")
	body = text.TruncateTail(body, maxChars, "…[output truncated]")
	if !a.cut {
		return body
	}
	banner := "…[output over limit, only tail kept"
	if a.file != "" {
		banner += ", full output: " + a.file
	}
	banner += "]\n"
	return banner + body
}

// chunkTexts extracts the text slice of chunks (used only by finalize for joining).
func chunkTexts(chunks []chunkBuf) []string {
	out := make([]string, len(chunks))
	for i, c := range chunks {
		out[i] = c.text
	}
	return out
}
