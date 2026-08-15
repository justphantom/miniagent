package main

import (
	"time"

	"github.com/justphantom/miniagent/internal/miniagent"
	"github.com/justphantom/miniagent/internal/miniagent/tools"
)

// buildTools registers 8 builtin tools and adjusts constraints by mode:
//   - default: file tools (read/write/edit/grep/glob) are confined to the workdir subtree via confineWrap;
//     shell is registered with mode=default (rejects sudo/su). workdir is required (validated at the main entry).
//   - auto: no constraints (shell mode=auto, file tools are not wrapped) unless confineAuto is opted in.
func buildTools(workdir string, shellTimeout, fileOpTimeout, writeTimeout time.Duration,
	mode string, fileResultLimit int, limits miniagent.Limits,
	confineAuto, evalSymlinks, shellConfineCD bool, shellAllowlist []string) []miniagent.Tool {
	shellMode := mode
	if shellMode == "" {
		shellMode = miniagent.ModeDefault
	}
	// confine wraps the file tools when in ModeDefault, OR in ModeAuto when confineAuto is opted in
	// (auto keeps shell free; this is deterministic-file-primitive defense-in-depth for long sessions).
	confine := mode == miniagent.ModeDefault || (mode == miniagent.ModeAuto && confineAuto)
	read := tools.ReadFileTool(workdir, fileOpTimeout, limits.MaxReadFileBytes)
	write := tools.WriteFileTool(workdir, writeTimeout)
	edit := tools.EditFileTool(workdir, writeTimeout)
	if fileResultLimit > 0 {
		// ResultLimit is an exported field; confineWrap preserves it (only replacing Call), so set it before wrapping.
		read.ResultLimit = fileResultLimit
		edit.ResultLimit = fileResultLimit
	}
	if confine && workdir != "" {
		read = confineWrap(read, workdir, true, evalSymlinks)
		write = confineWrap(write, workdir, false, evalSymlinks)
		edit = confineWrap(edit, workdir, false, evalSymlinks)
	}
	grep := tools.GrepTool(workdir, fileOpTimeout, limits.MaxGrepMatches, limits.MaxShellOutputChars)
	glob := tools.GlobTool(workdir, fileOpTimeout, limits.MaxShellOutputChars)
	if confine && workdir != "" {
		grep = confineWrap(grep, workdir, true, evalSymlinks)
		glob = confineWrap(glob, workdir, true, evalSymlinks)
	}
	shell := tools.ShellTool(workdir, shellTimeout, shellMode, limits.MaxShellOutputChars, limits.ShellStreamWindowBytes)
	if shellMode == miniagent.ModeDefault && workdir != "" && (shellConfineCD || len(shellAllowlist) > 0) {
		shell = tools.GuardShell(shell, workdir, shellAllowlist, shellConfineCD)
	}
	git := tools.GitTool(workdir, fileOpTimeout, false)
	goT := tools.GoTool(workdir, fileOpTimeout)
	if confine && workdir != "" {
		git = confineWrap(git, workdir, true, evalSymlinks)
		goT = confineWrap(goT, workdir, false, evalSymlinks)
	}
	return []miniagent.Tool{
		read, write, edit, grep, glob, shell, git, goT,
	}
}
