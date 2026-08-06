// Package text 提供核心与外挂子包（compaction/event/builtin/provider/openai）共享的
// 纯文本 helper：rune 截断（头截 / 尾截 / 头尾分段）、CJK/非 CJK 字符统计、Unix 毫秒时间戳。
// 全部为不依赖 agent 领域类型的纯函数，故独立成仓库级共享层（internal/text），
// 消除各包对同一截断/统计逻辑的分叉实现。
package text

import (
	"time"
	"unicode"
)

// NowMs 返回 Unix 毫秒时间戳，供消息打戳（如压缩 summaryMsg.Ts）。
func NowMs() int64 {
	return time.Now().UnixMilli()
}

// CountCharsLocal 统计 s 的 non-CJK / CJK rune 数（与 token 估算同口径）。
func CountCharsLocal(s string) (nonCJK, cjk int) {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) ||
			unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r) {
			cjk++
		} else {
			nonCJK++
		}
	}
	return nonCJK, cjk
}

// Truncate clamps s to n runes and appends marker when it truncated. n<=0 原样返回。
func Truncate(s string, n int, marker string) string {
	if n <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + marker
}

// TruncateTail 保留 s 末尾 n 个 rune，截断时前置 marker。
// 服务尾部兜底（shell 累积器保尾丢中段后，再把窗口尾部截到 maxChars）。n<=0 原样返回；len<=n 不截。
func TruncateTail(s string, n int, marker string) string {
	if n <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return marker + string(r[len(r)-n:])
}

// TruncateHeadTail 把 s 截到约 n 个 rune，保留「头 headN + 尾 tailN」，中间用 marker 连接。
// 用于关键信息在尾部的工具结果：head-only 会丢掉编译/测试错误汇总、命中上限提示等诊断信息。
// 头占 n/4（前段上下文/命令回显），尾占 3n/4（错误结论集中处）。n<=0 原样返回；长度<=n 不截。
// marker 置于中段省略处（与 Truncate 的尾部 marker 语义不同）。
func TruncateHeadTail(s string, n int, marker string) string {
	if n <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	headN := max(n/4, 1)
	tailN := max(n-headN, 1)
	if headN+tailN >= len(r) {
		return s // 头尾窗口已覆盖全部，无需截断（marker 反而增噪）
	}
	return string(r[:headN]) + marker + string(r[len(r)-tailN:])
}
