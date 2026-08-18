package main

import (
	"time"

	"github.com/justphantom/miniagent/internal/miniagent"
	"github.com/justphantom/miniagent/internal/miniagent/tools"
)

// buildTools registers the 8 builtin tools (read/write/edit/grep/glob/ast/shell/web).
// Single permission mode: isolation is delegated entirely to the OS layer (dedicated
// low-privilege user / container); no agent-level confinement or subcommand allow-lists.
func buildTools(workdir string, shellTimeout, fileOpTimeout, writeTimeout, webTimeout time.Duration,
	fileResultLimit int, limits miniagent.Limits) []miniagent.Tool {
	read := tools.ReadFileTool(workdir, fileOpTimeout, limits.MaxReadFileBytes)
	edit := tools.EditFileTool(workdir, writeTimeout)
	if fileResultLimit > 0 {
		read.ResultLimit = fileResultLimit
		edit.ResultLimit = fileResultLimit
	}
	return []miniagent.Tool{
		read,
		tools.WriteFileTool(workdir, writeTimeout),
		edit,
		tools.GrepTool(workdir, fileOpTimeout, limits.MaxGrepMatches, limits.MaxShellOutputChars),
		tools.GlobTool(workdir, fileOpTimeout, limits.MaxShellOutputChars),
		tools.AstTool(workdir, fileOpTimeout, limits.MaxShellOutputChars),
		tools.WebTool(webTimeout, 0, limits.MaxShellOutputChars, false),
		tools.ShellTool(workdir, shellTimeout, limits.MaxShellOutputChars, limits.ShellStreamWindowBytes),
	}
}
