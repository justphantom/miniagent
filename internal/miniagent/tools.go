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
