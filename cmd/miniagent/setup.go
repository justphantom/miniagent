package main

import (
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"log/slog"

	"github.com/justphantom/miniagent/internal/miniagent"
	"github.com/justphantom/miniagent/internal/miniagent/config"
	"github.com/justphantom/miniagent/internal/miniagent/event"
)

func mustParseLogLevel(s string) slog.Level {
	var level slog.Level
	if err := level.UnmarshalText([]byte(s)); err != nil {
		fmt.Fprintf(os.Stderr, "miniagent: invalid -log-level %q (want debug|info|warn|error)\n", s)
		os.Exit(1)
	}
	return level
}

func requireConfig(configPath string) (*config.Config, error) {
	if configPath != "" {
		return config.LoadConfig(configPath)
	}
	p, err := findDefaultConfigPath()
	if err != nil {
		return nil, err
	}
	if p != "" {
		return config.LoadConfig(p)
	}
	return nil, errors.New("miniagent config not found (-config <path> or ~/.miniagent/miniagent.json)")
}

// findDefaultConfigPath locates the default config path: only ~/.miniagent/miniagent.json.
// Returns the absolute path if found; an empty string if none; the stat error if stat fails.
func findDefaultConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locate home directory failed: %w", err)
	}
	p := filepath.Join(home, ".miniagent", "miniagent.json")
	if _, err := os.Stat(p); err == nil {
		abs, _ := filepath.Abs(p)
		return abs, nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("stat config %q: %w", p, err)
	}
	return "", nil
}

// collectOverrides uses flag.Visit to collect explicitly-set flags (unset left nil), for Resolve to arbitrate.
// After P2 only core CLI params remain; policy params (summary/duration/window etc.) live only in config, not via CLI.
func collectOverrides(f *cliFlags) config.CLIOverrides {
	set := map[string]bool{}
	flag.Visit(func(fl *flag.Flag) { set[fl.Name] = true })
	o := config.CLIOverrides{}
	if set["provider"] {
		o.Provider = f.provider
	}
	if set["model"] {
		o.Model = f.model
	}
	if set["thinking"] {
		o.Thinking = f.thinking
	}
	if set["max-iterations"] {
		o.MaxIterations = f.maxIterations
	}
	if set["stream"] {
		o.Stream = f.stream
	}
	if set["result-only"] {
		o.ResultOnly = f.resultOnly
	}
	if set["confirm-destructive"] {
		o.ConfirmDestructive = f.confirmDestructive
	}
	return o
}

// resolveFinalKey: config(provider.Key) > env. Secrets should be injected via env vars to avoid plaintext in config.
func resolveFinalKey(providerKey string) string {
	if providerKey != "" {
		return providerKey
	}
	return os.Getenv("MINIAGENT_API_KEY")
}

func validateConversation(resolved *config.Resolved, f *cliFlags) {
	// Check mutex on the resolved stream (cli>config): config run.stream=true + CLI -result-only must also trigger,
	// otherwise loopCfg.Stream resolves true while buildHooks returns an empty event hook, SSE is fetched then discarded, violating documented mutual-exclusion semantics.
	if intoBool(resolved.Run.Stream, *f.stream) && *f.resultOnly {
		fmt.Fprintln(os.Stderr, "miniagent: -stream (or config run.stream) is mutually exclusive with -result-only")
		os.Exit(1)
	}
	if *f.saveSession && *f.session != "" {
		fmt.Fprintln(os.Stderr, "miniagent: -save-session is mutually exclusive with -session")
		os.Exit(1)
	}
	if *f.saveSession && *f.resultOnly {
		fmt.Fprintln(os.Stderr, "miniagent: -save-session is mutually exclusive with -result-only (subagent fork is stateless, does not persist session)")
		os.Exit(1)
	}
	// workdir is required unconditionally, must be an absolute path, and is sourced
	// ONLY from -workdir (no config run.workdir, no cwd fallback). effectiveWorkdir +
	// absWorkdir honour the same single-source contract.
	wd := effectiveWorkdir(f)
	if wd == "" {
		fmt.Fprintln(os.Stderr, "miniagent: -workdir is required")
		os.Exit(1)
	}
	if !filepath.IsAbs(wd) {
		fmt.Fprintf(os.Stderr, "miniagent: -workdir must be an absolute path (got %q)\n", wd)
		os.Exit(1)
	}
}

// effectiveWorkdir returns the workdir the agent runs in. It is sourced ONLY from
// the -workdir flag — never from config (run.workdir is removed) and never from the
// process cwd. validateConversation enforces non-empty + absolute before any turn.
func effectiveWorkdir(f *cliFlags) string {
	return *f.workdir
}

func maxDurationOf(resolved *config.Resolved) time.Duration {
	if resolved.Run.MaxDuration != nil {
		return *resolved.Run.MaxDuration
	}
	return 0
}

func shellTimeoutOf(resolved *config.Resolved) time.Duration {
	if resolved.Run.ShellTimeout != nil {
		return *resolved.Run.ShellTimeout
	}
	return 0
}

func fileOpTimeoutOf(resolved *config.Resolved) time.Duration {
	if resolved.Run.FileOpTimeout != nil {
		return *resolved.Run.FileOpTimeout
	}
	return 0
}

func writeTimeoutOf(resolved *config.Resolved) time.Duration {
	if resolved.Run.WriteTimeout != nil {
		return *resolved.Run.WriteTimeout
	}
	return 0
}

// webTimeoutOf returns run.web_timeout (0 = WebTool's built-in 30s default).
func webTimeoutOf(resolved *config.Resolved) time.Duration {
	if resolved.Run.WebTimeout != nil {
		return *resolved.Run.WebTimeout
	}
	return 0
}

func httpTimeoutOf(resolved *config.Resolved) time.Duration {
	if resolved.HTTPTimeout != nil {
		return *resolved.HTTPTimeout
	}
	return 0
}

func maxReadFileBytesOf(resolved *config.Resolved) int {
	if resolved.RunConfig.MaxReadFileBytes != nil {
		return *resolved.RunConfig.MaxReadFileBytes
	}
	return 0
}

func maxShellOutputCharsOf(resolved *config.Resolved) int {
	if resolved.RunConfig.MaxShellOutputChars != nil {
		return *resolved.RunConfig.MaxShellOutputChars
	}
	return 0
}

func maxSessionBytesOf(resolved *config.Resolved) int {
	if resolved.RunConfig.MaxSessionBytes != nil {
		return *resolved.RunConfig.MaxSessionBytes
	}
	return 0
}

func buildHooks(resultOnly bool) miniagent.LoopHooks {
	if resultOnly {
		// subagent fork: stdout is plain-text result, no NDJSON events emitted.
		return miniagent.LoopHooks{}
	}
	emit := event.ToolUseWriter(os.Stdout)
	return miniagent.LoopHooks{
		OnToolUse: func(name, input string) error { return emit(name, input) },
		OnToolResult: func(name, callID string, r miniagent.ToolResult) error {
			return event.EmitToolResult(os.Stdout, name, callID, r)
		},
		OnDelta: func(step int, kind miniagent.DeltaKind, text string) error {
			return event.EmitDelta(os.Stdout, step, kind, text)
		},
	}
}

func emitRunError(err error, resultOnly bool, logger *slog.Logger) {
	if resultOnly {
		fmt.Printf("error: %s\n", err.Error())
		return
	}
	if eerr := event.EmitError(os.Stdout, err.Error()); eerr != nil {
		logger.Warn("emit error failed", "error", eerr)
		fmt.Fprintf(os.Stderr, "miniagent: %v\n", err)
	}
}

func emitRunResult(result miniagent.Result, model string, resultOnly bool, logger *slog.Logger) {
	if resultOnly {
		fmt.Println(result.Text)
		return
	}
	if err := event.EmitResult(os.Stdout, result, model); err != nil {
		logger.Warn("emit result failed", "error", err)
		fmt.Fprintf(os.Stderr, "miniagent: emit result failed: %v (text: %.200q)\n", err, result.Text)
		os.Exit(1)
	}
}

// absConfigPath returns the absolute path of the actually-loaded config (explicit -config or default ~/.miniagent/miniagent.json),
// for subagent fork bootstrap injection. cfg is always non-nil (S1 removed bare mode).
// Logic mirrors requireConfig: explicit -config > ~/.miniagent/miniagent.json.
func absConfigPath(configPath string) string {
	if configPath != "" {
		abs, _ := filepath.Abs(configPath)
		return abs
	}
	p, _ := findDefaultConfigPath()
	return p
}
