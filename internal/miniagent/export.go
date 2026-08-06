package miniagent

// 本文件暴露核心的共享底层 helper 供外挂子包（如 compaction）复用，保持同一实现、避免分叉。
// 内部调用方继续用未导出版本；导出版本仅是薄包装。

// NowMs 是 nowMs 的导出版本（Unix 毫秒时间戳），供外挂子包给消息打戳（如压缩 summaryMsg.Ts）。
func NowMs() int64 { return nowMs() }

// EstimateTokens 是 estimateTokens 的导出版本，供外挂子包做历史 token 估算（如压缩阈值判定）。
func EstimateTokens(msgs []Message, system string, tools []Tool) int {
	return estimateTokens(msgs, system, tools)
}

// CountCharsLocal 是 countCharsLocal 的导出版本，供外挂子包复用同一 CJK/non-CJK rune 统计口径。
func CountCharsLocal(s string) (nonCJK, cjk int) { return countCharsLocal(s) }

// ValidateToolPairing 是 validateToolPairing 的导出版本，供外挂子包校验 assistant.tool_calls 与
// tool 消息配对（如压缩中段切分前）。
func ValidateToolPairing(msgs []Message) error { return validateToolPairing(msgs) }

// TruncateHeadTail 是 truncateHeadTail 的导出版本，供外挂子包做头尾分段截断（如压缩边界轮 tool 内容）。
func TruncateHeadTail(s string, n int, marker string) string {
	return truncateHeadTail(s, n, marker)
}
