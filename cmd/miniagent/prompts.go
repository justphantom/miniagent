package main

import (
	"strings"
)

// defaultSystemPrompt is the default system prompt oriented toward engineering code development:
// it constrains the ReAct workflow (observe first -> then modify -> verify after changes ->
// review on failure), reducing the chance of the model making blind/speculative changes.
// Users can override it via config defaults.system_prompt. The prompt only states "why/how"
// constraints; tool syntax lives in each tool's description.
const defaultSystemPrompt = `You are a pragmatic software engineer working inside a real code repository. Follow this workflow:

- Observe before acting: before changing any file, use read/grep/glob to confirm the current content and structure; locate first when the path or symbol is uncertain, do not guess.
- Verify after changes: after code changes, run the relevant build/test via shell (e.g. go build, go test); do not claim "done" without verification.
- Review failures first: when a command or tool returns an error, first read the error message and related files, understand the root cause before changing; do not repeatedly blind-edit the same spot.
- Precise edits: when using edit, old_string must match the file exactly and be unique; use replace_all for multiple identical changes; use write to create new files.
- Segment large files: read returns lines with numbers; for large files use offset/limit to read in segments, do not swallow it all at once.`

// assembleSystemPrompt assembles the final system prompt: empty base falls back to defaultSystemPrompt,
// then subagent guidance is appended. Centralizing the two steps makes the default fallback unit-testable
// (NEW-1 regression).
//
// Under the default config (no defaults.system_prompt) resolved.System is empty:
// it must fall back to defaultSystemPrompt. Otherwise injectSubagentGuidance appends subagent guidance to the
// empty string making it non-empty, the loopCfg `if system == ""` fallback never triggers (dead code), and the
// agent silently loses all ReAct constraints.
func assembleSystemPrompt(base, guidance, configAbsPath, mode string) string {
	if base == "" {
		base = defaultSystemPrompt
	}
	return injectSubagentGuidance(base, guidance, configAbsPath, mode)
}

// injectSubagentGuidance appends the subagent fork guidance to the system prompt: injects the absolute path
// to config (reviewed v1 #12 + v2 #9 + v3 #6/#8). If configAbsPath is empty, nothing is injected. mode is passed
// through from the parent session's permission mode (review v3 P3): default is no longer hardcoded; a subagent
// forked from an auto parent session inherits auto; an empty value falls back to default.
// The subagent is a stateless single invocation (session is not persisted, stdout is the result), so the parent
// session id is no longer injected.
func injectSubagentGuidance(system, guidance, configAbsPath, mode string) string {
	if configAbsPath == "" {
		return system
	}
	if mode == "" {
		mode = "default"
	}
	return system + "\n\n" + renderSubagentGuidance(guidance, configAbsPath, mode)
}

// shellSingleQuote wraps s in single quotes and escapes embedded single quotes so that a config path
// containing spaces/metacharacters (e.g. macOS /Users/First Last/.miniagent/miniagent.json) can be used safely
// as an argument in the subagent guidance command.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// defaultSubagentGuidance is the default subagent fork guidance template; the placeholders {config_path}/{mode}
// are replaced by renderSubagentGuidance (config_path is wrapped via shellSingleQuote; mode is passed through from
// the parent session's permission mode; when the parent is auto the subagent command uses -mode auto).
// It can be overridden via config defaults.subagent_guidance.
const defaultSubagentGuidance = `- Subtask delegation: for parallelizable subtasks, invoke miniagent once more via shell (fork only when necessary, recommended nesting depth <= 2):
  echo "<subtask>" | miniagent -config {config_path} -workdir . -mode {mode} -result-only
  The subagent is a stateless single invocation (session is not persisted); stdout plain text is the result.`

// renderSubagentGuidance renders the subagent guidance: if guidance is empty it uses the default template;
// the placeholders {config_path}/{mode} are replaced by shellSingleQuote(configAbsPath)/mode.
func renderSubagentGuidance(guidance, configAbsPath, mode string) string {
	if guidance == "" {
		guidance = defaultSubagentGuidance
	}
	return strings.NewReplacer(
		"{config_path}", shellSingleQuote(configAbsPath),
		"{mode}", mode,
	).Replace(guidance)
}
