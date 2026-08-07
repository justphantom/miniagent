package miniagent

import (
	"context"
	"path/filepath"
	"time"
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

// runWithTimeout 把「ctx 取消检查 + WithTimeout + goroutine + select 兜底」封装为单一 helper，
// 供 read/write/edit/grep/glob/codemap 等文件类工具复用（原 6 处逐字重复样板）。label 入超时/取消
// 文案（如「读取」「搜索」）。fn 接收 runCtx（含超时），可在长操作/遍历中查 runCtx 提前返回——
// 但 Go 无法强制终止 goroutine：单文件 syscall（read/write/edit）陷 D-state 时不可中断（OS 层限制），
// 仅 grep/glob/codemap 的 WalkDir 遍历经 runCtx 可及时终止。fn 须及时返回，否则 select 兜底后
// goroutine 仍跑到 fn 自然结束（done buffered=1 保证发送不阻塞，但不保证 fn 可中断）。
func runWithTimeout(ctx context.Context, timeout time.Duration, label string, fn func(ctx context.Context) ToolResult) ToolResult {
	if err := ctx.Err(); err != nil {
		return ToolResult{IsError: true, Output: "已取消：" + err.Error()}
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	done := make(chan ToolResult, 1)
	go func() { done <- fn(runCtx) }()
	select {
	case r := <-done:
		return r
	case <-runCtx.Done():
		return ToolResult{IsError: true, Output: label + "超时或已取消：" + runCtx.Err().Error()}
	}
}
