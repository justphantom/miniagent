package main

import (
	"time"

	"github.com/justphantom/miniagent/internal/miniagent"
	"github.com/justphantom/miniagent/internal/miniagent/tools"
)

// buildTools registers 6 builtin tools and adjusts constraints by mode:
//   - default: write tools (write/edit) are confined to the workdir subtree via confineWrap;
//     shell is registered with mode=default (rejects sudo/su). workdir is required (validated at the main entry).
//   - auto: no constraints (shell mode=auto, write tools are not wrapped).
//
// When workdir is empty the file tools go through resolveToolPath and shell's cmd.Dir is left empty. shellTimeout<=0 uses the default 120s.
// fileOpTimeout<=0 uses the default 30s; writeTimeout<=0 uses the default 30s.
// When fileResultLimit>0 it overrides read/edit's Tool.ResultLimit (S4: config run.max_file_result_chars);
// <=0 keeps the constructor builtin default (maxFileResultInHistory).
func buildTools(workdir string, shellTimeout, fileOpTimeout, writeTimeout time.Duration, mode string, fileResultLimit int, limits miniagent.Limits, confineAuto, evalSymlinks bool, shellConfineCD bool, shellAllowlist []string) []miniagent.Tool {
	shellMode := mode
	if shellMode == "" {
		shellMode = miniagent.ModeDefault
	}
	// confine wraps the file tools when in ModeDefault, OR in ModeAuto when confineAuto is opted in (shell stays free either way —
	// S-1 root cause untouched; this is deterministic-file-primitive defense-in-depth for long sessions).
	confine := mode == miniagent.ModeDefault || (mode == miniagent.ModeAuto && confineAuto)
	read := tools.ReadFileTool(workdir, fileOpTimeout, limits.MaxReadFileBytes)
	write := tools.WriteFileTool(workdir, writeTimeout)
	edit := tools.EditFileTool(workdir, fileOpTimeout)
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
	// Opt-in default-mode shell guardrails (allowlist + cd-confine). Applied only in ModeDefault with a real workdir,
	// so auto mode stays fully unrestricted and the empty-workdir degenerate case is untouched.
	if shellMode == miniagent.ModeDefault && workdir != "" && (shellConfineCD || len(shellAllowlist) > 0) {
		shell = tools.GuardShell(shell, workdir, shellAllowlist, shellConfineCD)
	}
	return []miniagent.Tool{
		read,
		write,
		edit,
		grep,
		glob,
		shell,
	}
}
