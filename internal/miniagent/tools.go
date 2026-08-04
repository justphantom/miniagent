package miniagent

import (
	"path/filepath"
)

// resolveToolPath 解析工具路径：workspaceRoot 为空或 p 已是绝对路径时原样返回；
// 否则 join(workspaceRoot, p)。不做 EvalSymlinks 与越界判断——本形态不做
// 路径边界约束，越界保护由具体工具的 openNoFollow / 文件大小上限兜底。
func resolveToolPath(workspaceRoot, p string) string {
	if workspaceRoot == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(workspaceRoot, p)
}

// maxFileResultInHistory 是 read/edit 这类代码内容工具的结果入历史字符上限：
// 代码截断即丢准确性，给高于默认 maxToolResultInHistory 的额度（仍受 read 自身
// maxReadFileChars 输出上限约束）。Tool.ResultLimit 取此值。
const maxFileResultInHistory = 8000

// truncate clamps s to n runes and appends marker when it truncated.
func truncate(s string, n int, marker string) string {
	if n <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + marker
}

// truncateHeadTail 把 s 截到约 n 个 rune，保留「头 headN + 尾 tailN」，中间用 marker 连接。
// 用于 shell/grep 等关键信息在尾部的工具结果：head-only 会丢掉编译/测试错误汇总、命中上限提示等
// 最该让模型看到的诊断信息。头占 n/4（前段提供上下文/命令回显），尾占 3n/4（错误结论集中处）。
// n<=0 原样返回；长度<=n 不截。marker 置于中段省略处（与 truncate 的尾部 marker 语义不同）。
func truncateHeadTail(s string, n int, marker string) string {
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

// object 构造 JSON Schema 的 object 描述。required 为空时省略键：JSON Schema
// 规范规定省略 required 等同空数组，所有合规后端都接受；而把 nil slice 写进
// map 会被序列化成 "required":null，触发严格后端（如 OpenAI）的 400。
func object(props map[string]any, required ...string) map[string]any {
	out := map[string]any{
		"type":       "object",
		"properties": props,
	}
	if len(required) > 0 {
		out["required"] = required
	}
	return out
}
