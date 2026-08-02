package main

import (
	"time"

	"github.com/justphantom/miniagent/internal/miniagent"
)

// buildTools 注册 6 个内置工具 + N 个项目脚本工具（P1：.miniagent/scripts.json），按 mode 调整约束：
//   - default：写工具（write/edit）经 confineWrap 限定在 workdir 子树；
//     shell/script 以 mode=default 注册（拒 sudo/su）。workdir 必填（main 入口校验）。
//   - auto：无任何约束（shell/script mode=auto，写工具不包装）。
//
// workdir 为空时文件工具走 resolveToolPath、shell 的 cmd.Dir 留空。shellTimeout<=0 用默认 60s。
// fileResultLimit>0 时覆盖 read/edit 的 Tool.ResultLimit（S4：config run.max_file_result_chars），
// <=0 保留构造器内置默认（maxFileResultInHistory）。scripts 中 name/command 为空者跳过。
func buildTools(workdir string, shellTimeout time.Duration, mode string, fileResultLimit int, scripts []scriptDef) []miniagent.Tool {
	shellMode := mode
	if shellMode == "" {
		shellMode = miniagent.ModeDefault
	}
	read := miniagent.ReadFileTool(workdir)
	write := miniagent.WriteFileTool(workdir)
	edit := miniagent.EditFileTool(workdir)
	if fileResultLimit > 0 {
		// ResultLimit 是导出字段；confineWrap 保留它（仅替换 Call），故先设再包装。
		read.ResultLimit = fileResultLimit
		edit.ResultLimit = fileResultLimit
	}
	if mode == miniagent.ModeDefault && workdir != "" {
		write = confineWrap(write, workdir)
		edit = confineWrap(edit, workdir)
	}
	tools := []miniagent.Tool{
		read,
		write,
		edit,
		miniagent.GrepTool(workdir),
		miniagent.GlobTool(workdir),
		miniagent.ShellTool(workdir, shellTimeout, shellMode),
	}
	// P1：项目脚本注册为 script_<name> 工具，复用 shell 的安全策略（runShellCommand）。
	for _, s := range scripts {
		if s.Name == "" || s.Command == "" {
			continue
		}
		tools = append(tools, miniagent.ScriptTool(s.Name, s.Description, s.Command, workdir, shellTimeout, shellMode))
	}
	return tools
}
