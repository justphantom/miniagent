package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// defaultSystemPrompt is the default system prompt oriented toward engineering code development:
// it constrains the ReAct workflow (observe first -> then modify -> verify after changes ->
// review on failure), reducing the chance of the model making blind/speculative changes.
// Users can override it via config defaults.system_prompt. The prompt only states "why/how"
// constraints; tool syntax lives in each tool's description.
const defaultSystemPrompt = `You are a pragmatic software engineer working inside a real code repository. Follow this workflow:

- Observe before acting: before changing any file, use read/grep/glob to confirm the current content and structure; locate first when the path or symbol is uncertain, do not guess.
- Verify after changes: after code changes, run the relevant build/test via shell (e.g. go build/test, golangci-lint, npm test); do not claim "done" without verification.
- Review failures first: when a command or tool returns an error, first read the error message and related files, understand the root cause before changing; do not repeatedly blind-edit the same spot.
- Precise edits: when using edit, old_string must match the file exactly and be unique; use replace_all for multiple identical changes; use write to create new files.
- Segment large files: read returns lines with numbers; for large files use offset/limit to read in segments, do not swallow it all at once.
- Stay inside the working directory: treat the working directory as the project root. Operate only on files under it; never access paths outside it (no absolute paths elsewhere, no .. escapes). If a task seems to require files outside the working directory, state the need instead of accessing them.`

// assembleSystemPrompt assembles the final system prompt: empty base falls back to defaultSystemPrompt,
// then subagent guidance is appended.
func assembleSystemPrompt(base, guidance, configAbsPath, workdir, rulesFile string) string {
	if base == "" {
		base = defaultSystemPrompt
	}
	base = appendProjectRules(base, workdir, rulesFile)
	base = appendWorkdirLine(base, workdir)
	return injectSubagentGuidance(base, guidance, configAbsPath)
}

// appendWorkdirLine tells the model the absolute workdir so it does not need a pwd round-trip; all tools
// resolve relative paths against it. Empty workdir (unit-test degenerate case) is skipped; it sits after
// project rules so environment info stays adjacent, with subagent guidance always last.
func appendWorkdirLine(system, workdir string) string {
	if workdir == "" {
		return system
	}
	return system + "\n\nWorking directory (absolute): " + workdir + "\nTreat it as the project root: all file tools operate only under it; relative paths resolve against it, absolute paths and .. must not escape it."
}

// maxRulesFileBytes caps the rules file injected into the system prompt; an oversized file is truncated (with a stderr
// notice) rather than rejected, so a large rules file never blocks startup.
const maxRulesFileBytes = 64 * 1024 // 64KiB

// appendProjectRules appends the optional workdir rules file to the system prompt. rulesFile must be a bare filename
// (no separators, no "."/"..") resolved strictly under workdir — prevents reading files outside workdir into the prompt.
// Missing file is silently skipped; a read error is reported on stderr but not fatal.
func appendProjectRules(system, workdir, rulesFile string) string {
	if rulesFile == "" || workdir == "" {
		return system
	}
	if filepath.Base(rulesFile) != rulesFile || rulesFile == "." || rulesFile == ".." {
		fmt.Fprintf(os.Stderr, "miniagent: rules_file %q must be a bare filename under workdir, skipped\n", rulesFile)
		return system
	}
	path := filepath.Join(workdir, rulesFile)
	data, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "miniagent: read rules_file %q: %v\n", path, err)
		}
		return system
	}
	if len(data) > maxRulesFileBytes {
		data = data[:maxRulesFileBytes]
		fmt.Fprintf(os.Stderr, "miniagent: rules_file %q exceeds %d bytes, truncated\n", path, maxRulesFileBytes)
	}
	return system + "\n\n" + string(data)
}

// injectSubagentGuidance appends the subagent fork guidance to the system prompt: injects the absolute path
// to config. If configAbsPath is empty, nothing is injected.
// The subagent is a stateless single invocation (session is not persisted, stdout is the result).
func injectSubagentGuidance(system, guidance, configAbsPath string) string {
	if configAbsPath == "" {
		return system
	}
	return system + "\n\n" + renderSubagentGuidance(guidance, configAbsPath)
}

// shellSingleQuote wraps s in single quotes and escapes embedded single quotes so that a config path
// containing spaces/metacharacters (e.g. macOS /Users/First Last/.miniagent/miniagent.json) can be used safely
// as an argument in the subagent guidance command.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// defaultSubagentGuidance is the default subagent fork guidance template; the placeholder {config_path}
// is replaced by shellSingleQuote(configAbsPath). It can be overridden via config defaults.subagent_guidance.
const defaultSubagentGuidance = `- Subtask delegation: for parallelizable subtasks, invoke miniagent once more via shell (fork only when necessary, recommended nesting depth <= 2):
  echo "<subtask>" | miniagent -config {config_path} -workdir . -result-only
  The subagent is a stateless single invocation (session is not persisted); stdout plain text is the result.`

// renderSubagentGuidance renders the subagent guidance: if guidance is empty it uses the default template;
// the placeholder {config_path} is replaced by shellSingleQuote(configAbsPath).
func renderSubagentGuidance(guidance, configAbsPath string) string {
	if guidance == "" {
		guidance = defaultSubagentGuidance
	}
	return strings.NewReplacer(
		"{config_path}", shellSingleQuote(configAbsPath),
	).Replace(guidance)
}
