package main

import (
	"time"

	"github.com/justphantom/miniagent/internal/miniagent"
)

// buildTools 注册 9 个工具。workdir 为空时工具内部按各自规则处理（文件工具走
// resolveToolPath，shell 把 cmd.Dir 留空继承 cwd）。shellTimeout<=0 时 ShellTool 用默认 60s。
func buildTools(workdir string, shellTimeout time.Duration, confine string) []miniagent.Tool {
	tasks := &miniagent.TaskList{}
	write := miniagent.WriteFileTool(workdir)
	edit := miniagent.EditFileTool(workdir)
	multiEdit := miniagent.MultiEditTool(workdir)
	// confine=workdir 时包装写工具，拒绝越出 workdir 的路径（防误写）。读工具与 shell
	// 不约束：free 读无副作用，shell 沙箱只能靠 OS（见 README「运行隔离」）。
	if confine == "workdir" && workdir != "" {
		write = confineWrap(write, workdir)
		edit = confineWrap(edit, workdir)
		multiEdit = confineWrap(multiEdit, workdir)
	}
	return []miniagent.Tool{
		miniagent.ReadFileTool(workdir),
		write,
		edit,
		multiEdit,
		miniagent.GrepTool(workdir),
		miniagent.GlobTool(workdir),
		miniagent.ShellTool(workdir, shellTimeout),
		miniagent.FetchTool(),
		miniagent.TodoTool(tasks),
	}
}
