package main

import (
	"time"

	"github.com/justphantom/miniagent/internal/miniagent"
	"github.com/justphantom/miniagent/internal/miniagent/tools"
)

// buildTools registers builtin tools and adjusts constraints by mode:
//   - default (12 tools + opt-in web, NO shell): file tools (read/write/edit/grep/glob/ast) are confined to the workdir
//     subtree via confineWrap; a misfired shell call returns "unknown tool" since it is not registered.
//     workdir is required (validated at the main entry).
//   - auto (13 tools + opt-in web): no constraints (file tools are not wrapped) unless confineAuto is opted in.
//
// git and go tools are inherently read-only/constrained by their own allow-list, so they are NOT wrapped
// in confineWrap (which only understands a "path" JSON field, not "subcommand"). Their timeout aligns
// with shellTimeout (120s) rather than fileOpTimeout (30s), since go test/build can exceed 30s.
// web is opt-in via run.web_fetch (registered in both modes when enabled) — it has no workdir-bound
// path argument, so confineWrap does not apply; its SSRF guard lives inside WebTool.
func buildTools(workdir string, shellTimeout, fileOpTimeout, writeTimeout, webTimeout time.Duration,
	mode string, fileResultLimit int, limits miniagent.Limits,
	confineAuto, evalSymlinks, webFetch bool) []miniagent.Tool {
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
	astT := tools.AstTool(workdir, fileOpTimeout, limits.MaxShellOutputChars)
	if confine && workdir != "" {
		grep = confineWrap(grep, workdir, true, evalSymlinks)
		glob = confineWrap(glob, workdir, true, evalSymlinks)
		astT = confineWrap(astT, workdir, true, evalSymlinks)
	}
	git := tools.GitTool(workdir, shellTimeout, limits.MaxShellOutputChars)
	goT := tools.GoTool(workdir, shellTimeout, limits.MaxShellOutputChars)
	npm := tools.NpmTool(workdir, shellTimeout, limits.MaxShellOutputChars)
	lint := tools.LintTool(workdir, shellTimeout, limits.MaxShellOutputChars)
	rename := tools.RenameTool(workdir, writeTimeout)
	deleteT := tools.DeleteTool(workdir, writeTimeout)
	if confine && workdir != "" {
		rename = confineWrap(rename, workdir, false, evalSymlinks)
		deleteT = confineWrap(deleteT, workdir, false, evalSymlinks)
	}
	builtins := []miniagent.Tool{
		read, write, edit, grep, glob, astT, git, goT, npm, lint, rename, deleteT,
	}
	// shell is auto-only: default mode does not register it (12 tools); a misfired call fails
	// dispatch with "unknown tool". mode=="" resolves to default at the config layer, so only
	// an explicit auto registers shell — mirrors the shellMode resolution that used to live here.
	if mode == miniagent.ModeAuto {
		builtins = append(builtins, tools.ShellTool(workdir, shellTimeout, limits.MaxShellOutputChars, limits.ShellStreamWindowBytes))
	}
	if webFetch {
		builtins = append(builtins, tools.WebTool(webTimeout, 0, limits.MaxShellOutputChars, false))
	}
	return builtins
}
