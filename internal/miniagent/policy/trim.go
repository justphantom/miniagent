package policy

import (
	"github.com/justphantom/miniagent/internal/miniagent"
	"github.com/justphantom/miniagent/internal/text"
)

// TrimForHistory trims a tool result down to limit characters before it enters history; limit<=0 uses the default cap
// (miniagent.MaxToolResultInHistory, now defined in core).
// split=true (shell/grep-like tools whose tail is critical) uses head+tail segmented truncation, retaining the
// tail error conclusion; otherwise head-only (read/edit-like code tools with line numbers, where front truncation
// matches the semantics of reading large files in segments). The C-2 context downgrade reuses the same trimming
// semantics (re-trims earlier tool content with a smaller limit).
func TrimForHistory(s string, limit int, split bool) string {
	if limit <= 0 {
		limit = miniagent.MaxToolResultInHistory
	}
	if split {
		return text.TruncateHeadTail(s, limit, "…[middle omitted]")
	}
	return text.Truncate(s, limit, "…[tool_result truncated]")
}
