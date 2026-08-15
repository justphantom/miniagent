package main

import (
	"time"

	"github.com/justphantom/miniagent/internal/miniagent"
	"github.com/justphantom/miniagent/internal/miniagent/tools"
)

func buildTools(workdir string, shellTimeout, fileOpTimeout, writeTimeout time.Duration,
	shellMode string, fileResultLimit int, limits miniagent.Limits,
	confine, evalSymlinks, shellConfineCD bool, shellAllowlist []string) []miniagent.Tool {
	read := tools.ReadFileTool(workdir, fileOpTimeout, limits.MaxReadFileBytes)
	write := tools.WriteFileTool(workdir, writeTimeout)
	edit := tools.EditFileTool(workdir, writeTimeout)
	grep := tools.GrepTool(workdir, fileOpTimeout, limits.MaxGrepMatches, limits.MaxShellOutputChars)
	glob := tools.GlobTool(workdir, fileOpTimeout, limits.MaxShellOutputChars)
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
