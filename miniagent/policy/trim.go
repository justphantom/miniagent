package policy

import (
	"github.com/justphantom/miniagent/miniagent"
)

// TrimForHistory trims a tool result down to limit chars before it enters history. The implementation
// lives in miniagent.TrimForHistory (core): it is a pure string operation with no strategy, shared by
// compaction and policy. Kept as a thin re-export for backward compatibility with callers referencing
// policy.TrimForHistory.
func TrimForHistory(s string, limit int, split bool) string {
	return miniagent.TrimForHistory(s, limit, split)
}
