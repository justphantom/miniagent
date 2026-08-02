package main

import (
	"time"

	"github.com/justphantom/miniagent/internal/miniagent"
)

// buildTools 注册 7 个工具，按 mode 调整约束（审查 v3 §6）：
//   - default：写工具（write/edit/multi_edit）经 confineWrap 限定在 workdir 子树；
//     shell 以 mode=default 注册（拒 sudo/su）。workdir 必填（main 入口校验）。
//   - auto：无任何约束（shell mode=auto，写工具不包装）。
//
// workdir 为空时文件工具走 resolveToolPath、shell 的 cmd.Dir 留空。shellTimeout<=0 用默认 60s。
func buildTools(workdir string, shellTimeout time.Duration, mode string) []miniagent.Tool {
	shellMode := mode
	if shellMode == "" {
		shellMode = miniagent.ModeDefault
	}
	write := miniagent.WriteFileTool(workdir)
	edit := miniagent.EditFileTool(workdir)
	multiEdit := miniagent.MultiEditTool(workdir)
	if mode == miniagent.ModeDefault && workdir != "" {
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
		miniagent.ShellTool(workdir, shellTimeout, shellMode),
	}
}
