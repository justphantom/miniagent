package main

import (
	"time"

	"github.com/justphantom/miniagent/internal/miniagent"
)

// buildTools 注册 7 个内置工具，按 mode 调整约束：
//   - default：写工具（write/edit）经 confineWrap 限定在 workdir 子树；
//     shell 以 mode=default 注册（拒 sudo/su）。workdir 必填（main 入口校验）。
//   - auto：无任何约束（shell mode=auto，写工具不包装）。
//
// workdir 为空时文件工具走 resolveToolPath、shell 的 cmd.Dir 留空。shellTimeout<=0 用默认 60s。
// fileOpTimeout<=0 用默认 30s；writeTimeout<=0 用默认 30s。
// fileResultLimit>0 时覆盖 read/edit 的 Tool.ResultLimit（S4：config run.max_file_result_chars），
// <=0 保留构造器内置默认（maxFileResultInHistory）。
func buildTools(workdir string, shellTimeout, fileOpTimeout, writeTimeout time.Duration, mode string, fileResultLimit int, limits miniagent.Limits) []miniagent.Tool {
	shellMode := mode
	if shellMode == "" {
		shellMode = miniagent.ModeDefault
	}
	read := miniagent.ReadFileTool(workdir, fileOpTimeout, limits.MaxReadFileBytes)
	write := miniagent.WriteFileTool(workdir, writeTimeout)
	edit := miniagent.EditFileTool(workdir, fileOpTimeout)
	if fileResultLimit > 0 {
		// ResultLimit 是导出字段；confineWrap 保留它（仅替换 Call），故先设再包装。
		read.ResultLimit = fileResultLimit
		edit.ResultLimit = fileResultLimit
	}
	if mode == miniagent.ModeDefault && workdir != "" {
		read = confineWrap(read, workdir)
		write = confineWrap(write, workdir)
		edit = confineWrap(edit, workdir)
	}
	grep := miniagent.GrepTool(workdir, fileOpTimeout, limits.MaxGrepMatches, limits.MaxShellOutputChars)
	glob := miniagent.GlobTool(workdir, fileOpTimeout, limits.MaxShellOutputChars)
	codemap := miniagent.CodemapTool(workdir, fileOpTimeout)
	if mode == miniagent.ModeDefault && workdir != "" {
		grep = confineWrap(grep, workdir)
		glob = confineWrap(glob, workdir)
		codemap = confineWrap(codemap, workdir)
	}
	tools := []miniagent.Tool{
		read,
		write,
		edit,
		grep,
		glob,
		codemap,
		miniagent.ShellTool(workdir, shellTimeout, shellMode, limits.MaxShellOutputChars, limits.ShellStreamWindowBytes),
	}
	// todo 工具（单 Run 内存）：每轮 buildTools 新建 *TodoList，跨 step 共享、跨 Run 重置。
	return append(tools, miniagent.TodoTools(&miniagent.TodoList{})...)
}
