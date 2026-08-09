package main

import (
	"time"

	"github.com/justphantom/miniagent/internal/miniagent"
	"github.com/justphantom/miniagent/internal/miniagent/tools"
)

// buildTools registers 7 builtin tools and adjusts constraints by mode:
//   - default: write tools (write/edit) are confined to the workdir subtree via confineWrap;
//     shell is registered with mode=default (rejects sudo/su). workdir is required (validated at the main entry).
//   - auto: no constraints (shell mode=auto, write tools are not wrapped).
//
// When workdir is empty the file tools go through resolveToolPath and shell's cmd.Dir is left empty. shellTimeout<=0 uses the default 120s.
// fileOpTimeout<=0 uses the default 30s; writeTimeout<=0 uses the default 30s.
// When fileResultLimit>0 it overrides read/edit's Tool.ResultLimit (S4: config run.max_file_result_chars);
// <=0 keeps the constructor builtin default (maxFileResultInHistory).
func buildTools(workdir string, shellTimeout, fileOpTimeout, writeTimeout time.Duration, mode string, fileResultLimit int, limits miniagent.Limits) []miniagent.Tool {
	shellMode := mode
	if shellMode == "" {
		shellMode = miniagent.ModeDefault
	}
	read := tools.ReadFileTool(workdir, fileOpTimeout, limits.MaxReadFileBytes)
	write := tools.WriteFileTool(workdir, writeTimeout)
	edit := tools.EditFileTool(workdir, fileOpTimeout)
	if fileResultLimit > 0 {
		// ResultLimit is an exported field; confineWrap preserves it (only replacing Call), so set it before wrapping.
		read.ResultLimit = fileResultLimit
		edit.ResultLimit = fileResultLimit
	}
	if mode == miniagent.ModeDefault && workdir != "" {
		read = confineWrap(read, workdir)
		write = confineWrap(write, workdir)
		edit = confineWrap(edit, workdir)
	}
	grep := tools.GrepTool(workdir, fileOpTimeout, limits.MaxGrepMatches, limits.MaxShellOutputChars)
	glob := tools.GlobTool(workdir, fileOpTimeout, limits.MaxShellOutputChars)
	codemap := tools.CodemapTool(workdir, fileOpTimeout)
	if mode == miniagent.ModeDefault && workdir != "" {
		grep = confineWrap(grep, workdir)
		glob = confineWrap(glob, workdir)
		codemap = confineWrap(codemap, workdir)
	}
	built := []miniagent.Tool{
		read,
		write,
		edit,
		grep,
		glob,
		codemap,
		tools.ShellTool(workdir, shellTimeout, shellMode, limits.MaxShellOutputChars, limits.ShellStreamWindowBytes),
	}
	// todo tools (in-memory for a single Run): each buildTools call creates a new *TodoList, shared across steps, reset across Runs.
	return append(built, tools.TodoTools(&tools.TodoList{})...)
}
