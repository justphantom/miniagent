// tool_helpers.go：工具构造/执行专用 helper（仅 tool_*.go 使用）。原位于核心 tools.go，
// 移此修正「核心含工具专用 helper」的物理错放，为工具子包化（库化 5.0.0）铺路。
// 逻辑上属工具侧，非核心循环职责；同包仅为历史物理布局，不构成逻辑耦合（只用公共类型 + 本组 helper）。

package miniagent

import (
	"context"
	"path/filepath"
	"time"
)

// resolveToolPath 解析工具路径：workspaceRoot 为空或 p 已是绝对路径时原样返回；
// 否则 join(workspaceRoot, p)（join 内含 Clean，但 ../ 向上逃逸会被解析到 workdir 外）。
// free 模式**无路径边界约束**：../ 与绝对路径均可越出 workdir，隔离由调用方（容器/低权限用户）保证
// （README §运行隔离）。openNoFollow 仅拒最终分量符号链接，不构成边界；文件大小上限与边界无关。
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
	// goroutine 内自保 recover：fn 在此 goroutine 运行，调用方 safeCall 的 recover 捕获不到——
	// 与 safeCall（loop_tools.go）/callLLMOnce 对称，文件工具内部 panic 转 IsError 结果而非崩进程。
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- ToolResult{IsError: true, ExitCode: exitCodeNotSet, Output: label + "内部错误"}
			}
		}()
		done <- fn(runCtx)
	}()
	select {
	case r := <-done:
		return r
	case <-runCtx.Done():
		return ToolResult{IsError: true, Output: label + "超时或已取消：" + runCtx.Err().Error()}
	}
}
