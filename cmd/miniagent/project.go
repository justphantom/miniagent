package main

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/justphantom/miniagent/internal/miniagent/config"
)

// projectRules is the return of loadProjectRules: the persona/rules text under workdir/.miniagent/.
type projectRules struct {
	persona string
	rules   string
}

// hasAny reports whether any project rules were loaded (decides whether to inform the LLM at the end of the system prompt).
func (p projectRules) hasAny() bool {
	return p.persona != "" || p.rules != ""
}

// loadProjectRules reads the project rules (persona.md/rules.md) from <workdir>/.miniagent/.
// Returns the zero value (no error) when workdir is empty or the directory/file does not exist. Project-level single-layer lookup only — global system
// customization uses config defaults.system_prompt instead (the ~/.miniagent/ global rules layer was removed).
func loadProjectRules(workdir string) projectRules {
	var pr projectRules
	if workdir == "" {
		return pr
	}
	dir := filepath.Join(workdir, ".miniagent")
	if _, err := os.Stat(filepath.Join(dir, "persona.md")); err == nil {
		pr.persona = readTrimmedFile(filepath.Join(dir, "persona.md"))
	}
	if _, err := os.Stat(filepath.Join(dir, "rules.md")); err == nil {
		pr.rules = readTrimmedFile(filepath.Join(dir, "rules.md"))
	}
	return pr
}

// maxProjectFileBytes is the byte upper bound for rule files such as persona.md/rules.md.
const maxProjectFileBytes = 1 << 20 // 1 MiB

func readTrimmedFile(path string) string {
	b, err := config.ReadFileLimited(path, maxProjectFileBytes)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// mergeSystemPrompt merges the system prompt by priority persona.md > rules.md > defaults.system_prompt
// (when persona is present it replaces base as the identity baseline, otherwise base=defaults.system_prompt workflow constraints are kept),
// appends the rules section, and explicitly informs the LLM when any project rules were loaded (P0).
func mergeSystemPrompt(base, persona, rules string, hasAny bool) string {
	if persona != "" {
		base = persona
	}
	parts := []string{base}
	if rules != "" {
		parts = append(parts, "## Project Rules\n"+rules)
	}
	if hasAny {
		parts = append(parts, "(.miniagent/ project rules loaded)")
	}
	return strings.Join(parts, "\n\n")
}
