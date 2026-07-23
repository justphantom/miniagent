package main

import (
	"github.com/justphantom/miniagent/internal/miniagent"
)

// buildTools 无条件注册 4 个工具。workdir 为空时工具内部按各自规则处理
// （read/write/edit 走 resolveToolPath，shell 把 cmd.Dir 留空继承 cwd）。
func buildTools(workdir string) []miniagent.Tool {
	return []miniagent.Tool{
		miniagent.ReadFileTool(workdir),
		miniagent.WriteFileTool(workdir),
		miniagent.EditFileTool(workdir),
		miniagent.ShellTool(workdir),
	}
}
