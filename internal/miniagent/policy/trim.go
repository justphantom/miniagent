package policy

import "github.com/justphantom/miniagent/internal/text"

// MaxToolResultInHistory：单条 tool 结果入历史字符上限，平衡可读性与上下文预算。
const MaxToolResultInHistory = 4000

// TrimForHistory 把工具结果裁到 limit 字符后入历史；limit<=0 用默认上限。
// split=true（shell/grep/script 等尾部关键的工具）走头尾分段截断，保留尾部错误结论；
// 否则 head-only（read/edit 等带行号的代码类工具，前截断符合分段读大文件语义）。
// C-2 的 context 降级复用同一裁剪语义（对更早的 tool content 用更小 limit 再裁）。
func TrimForHistory(s string, limit int, split bool) string {
	if limit <= 0 {
		limit = MaxToolResultInHistory
	}
	if split {
		return text.TruncateHeadTail(s, limit, "…[省略中间段]")
	}
	return text.Truncate(s, limit, "…[tool_result 已截断]")
}
