package miniagent

// Limits centralizes all runtime-tunable thresholds, replacing the per-module package-level atomic
// overrides (Set*). It is injected explicitly via the tool-builder / session / hook factories, eliminating
// package-level mutable state — this supports multiple instances (e.g. a subagent fork with different
// limits), has no race risk (no atomic needed), and gives test isolation (passing args rather than Set on a
// global). Zero-valued fields fall back to the module built-in default at each injection point (<=0 uses the
// default). Rolled out in steps: step 1 tools, step 2 session, step 3 context-trim, step 4 compaction.
type Limits struct {
	// MaxReadFileBytes is the per-file byte cap for the read tool (default maxReadFileBytes=1MB).
	MaxReadFileBytes int
	// MaxShellOutputChars is the shared tool-output character cap for shell/glob/grep (default maxShellOutputChars=100KB).
	MaxShellOutputChars int
	// ShellStreamWindowBytes is the sliding-window byte cap for shell output (default MaxShellOutputChars*8).
	ShellStreamWindowBytes int
	// MaxGrepMatches is the cap on grep matched lines (default maxGrepMatches=500).
	MaxGrepMatches int
	// The fields below are wired in later steps (defined here for future use; zero value = module built-in default):
	// MaxSessionBytes: byte cap for the session file (step 2).
	MaxSessionBytes int
	// ContextTrimToolChars: compaction cap for tool content when context is exceeded (step 3).
	ContextTrimToolChars int
}
