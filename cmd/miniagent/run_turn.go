package main

// run_turn.go is the shared per-turn engine extracted from main.go's procedural pipeline:
// resolve → session → limits autofill → clients → tools → hooks → Run → save, streaming the
// stdout NDJSON contract to an injectable io.Writer. Two consumers: the CLI one-shot path
// (out=os.Stdout, protectSignals=true) and the -serve web path (out=HTTP response, signals
// owned by the server). os.Exit-free so it embeds in the long-running server process.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/justphantom/miniagent/config"
	"github.com/justphantom/miniagent/miniagent"
	"github.com/justphantom/miniagent/miniagent/compaction"
	"github.com/justphantom/miniagent/miniagent/event"
	"github.com/justphantom/miniagent/miniagent/metrics"
	"github.com/justphantom/miniagent/miniagent/session"
)

// turnSpec is one agent-turn request: the CLI fills it from flags, the web layer from the
// POST /api/turn JSON body. Overrides are arbitrated by config.Resolve (cli>config).
type turnSpec struct {
	prompt      string
	workdir     string // required, absolute (validated here — the web layer must not trust input)
	sessionArg  string // resume id; mutually exclusive with saveNew
	saveNew     bool   // create a new session
	resultOnly  bool   // CLI -result-only: plain text, no NDJSON events
	maxIterDef  int    // flag-level max_iterations fallback (web passes 0 = builtin default)
	metricsStep bool   // per-step metrics NDJSON to stderr (CLI-only knob; web ignores)
	overrides   config.CLIOverrides
}

// turnEngine holds turn-independent state: the loaded config, its absolute path (subagent
// guidance injection), the logger, an injectable client builder (tests pass a fake) and the
// per-provider transport cache (N3: web turns rebuild clients per request — the cache stops
// the transport pool from being recreated each time).
type turnEngine struct {
	cfg           *config.Config
	cfgPath       string
	logger        *slog.Logger
	buildClients  func(resolved *config.Resolved, apiKey string, logger *slog.Logger, cache *transportCache) (miniagent.LLM, miniagent.Doer, error)
	protectSignal bool            // CLI: signal.Ignore around session save; serve: signals owned by server
	transports    *transportCache // N3: per-provider *http.Transport reuse for long-running serve mode
}

// runTurn executes one agent turn and streams NDJSON events to out. Error paths mirror the
// CLI semantics: non-canceled errors emit an error event to out and still save the partially
// executed work (pairing-safe). Cancellation (context.Canceled) is returned silently — the
// caller maps it to exit code 130 / a closed HTTP stream.
func (e *turnEngine) runTurn(ctx context.Context, spec turnSpec, out io.Writer) error {
	var meta session.SessionMeta
	if spec.workdir == "" {
		return errors.New("workdir is required")
	}
	if !filepath.IsAbs(spec.workdir) {
		return fmt.Errorf("workdir must be an absolute path (got %q)", spec.workdir)
	}
	if spec.saveNew && spec.sessionArg != "" {
		return errors.New("save-session is mutually exclusive with session")
	}

	resolved, err := config.Resolve(e.cfg, spec.overrides)
	if err != nil {
		return err
	}
	apiKey := resolveFinalKey(resolved.Provider.Key)
	if apiKey == "" {
		return errors.New("API key missing (provider.key / $MINIAGENT_API_KEY)")
	}
	warnInsecureURL(resolved.Provider.ChatURL)

	// Best-effort auto-fill of model limits from the models endpoint (same semantics as the CLI path):
	// only when config left them unset; failures warn and continue.
	if resolved.MaxTokens == nil || resolved.ContextWindow == nil {
		limits, fetchErr := FetchModelLimits(ctx, resolved.Provider, resolved.ModelID, apiKey, httpTimeoutOf(resolved), e.logger)
		if fetchErr != nil {
			e.logger.Warn("auto-fill model limits skipped", "model", resolved.ModelID, "error", fetchErr)
		} else {
			if resolved.MaxTokens == nil && limits.MaxOutputTokens != nil {
				resolved.MaxTokens = limits.MaxOutputTokens
			}
			if resolved.ContextWindow == nil && limits.ContextWindow != nil {
				resolved.ContextWindow = limits.ContextWindow
			}
		}
	}

	sessionDir := defaultSessionDir()
	if resolved.Session.Dir != "" {
		sessionDir = resolved.Session.Dir
	}
	modelSpec := resolved.Provider.Name + "/" + resolved.ModelID
	sessPath, meta, history, err := resolveSession(spec.saveNew, spec.sessionArg, sessionDir, modelSpec, resolved.Provider.Name, spec.workdir, int64(maxSessionBytesOf(resolved)))
	if err != nil {
		return err
	}
	if spec.saveNew {
		// Mirrors the CLI: the session event precedes Run so consumers can capture the id early;
		// the jsonl itself is only persisted after Run (id may not exist on disk if the turn fails).
		if err := event.EmitSession(out, meta); err != nil {
			return fmt.Errorf("emit session: %w", err)
		}
	}
	resolved.System = assembleSystemPrompt(resolved.System, resolved.SubagentGuidance, e.cfgPath, spec.workdir, resolved.RulesFile)

	limits := miniagent.Limits{
		MaxReadFileBytes:       maxReadFileBytesOf(resolved),
		MaxShellOutputChars:    maxShellOutputCharsOf(resolved),
		ShellStreamWindowBytes: into(resolved.RunConfig.ShellStreamWindowBytes, 0),
		MaxGrepMatches:         into(resolved.RunConfig.GrepMaxMatches, 0),
		MaxSessionBytes:        maxSessionBytesOf(resolved),
		ContextTrimToolChars:   into(resolved.RunConfig.ContextTrimToolChars, 0),
	}

	llm, compChat, err := e.buildClients(resolved, apiKey, e.logger, e.transports)
	if err != nil {
		return err
	}

	toolOutputDir := ""
	if resolved.RunConfig.ToolOutputDir != nil && *resolved.RunConfig.ToolOutputDir != "" {
		toolOutputDir = *resolved.RunConfig.ToolOutputDir
	} else if sessPath != "" {
		toolOutputDir = filepath.Join(filepath.Dir(sessPath), strings.TrimSuffix(filepath.Base(sessPath), ".jsonl")+".tool-output")
	}

	tools := buildTools(spec.workdir, shellTimeoutOf(resolved), fileOpTimeoutOf(resolved), writeTimeoutOf(resolved), webTimeoutOf(resolved), into(resolved.RunConfig.MaxFileResultChars, 0), limits)
	baseCfg := loopCfg(resolved, spec.maxIterDef, intoBool(resolved.Run.Stream, false), history, tools)
	warnNoBudgetFuse(resolved, spec.sessionArg, spec.maxIterDef, e.logger)
	baseCfg.ToolOutputDir = toolOutputDir
	if resolved.Run.ToolOutputRetention != nil {
		baseCfg.ToolOutputRetention = *resolved.Run.ToolOutputRetention
	}
	compBefore, compAfter := compaction.NewCompaction(compactionOptions(resolved, meta, llm, compChat, baseCfg.System, tools, e.logger))
	hooks := assembleHooks(compBefore, compAfter, out, spec.resultOnly, intoBool(resolved.Run.ConfirmDestructive, false), baseCfg, tools, limits, e.logger)
	if spec.metricsStep {
		hooks.OnStep = metrics.NewStepEmitter(os.Stderr).Emit
	}

	runCtx := ctx
	if d := maxDurationOf(resolved); d > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(runCtx, d)
		defer cancel()
	}
	var result miniagent.Result
	result, err = miniagent.Run(runCtx, llm, baseCfg, spec.prompt, hooks, e.logger)

	// saveSession persists the executed part on success, error AND cancel paths (pairing is
	// completed by fillPlaceholderTail), recovering this turn for later resume.
	saveErr := e.saveSession(sessPath, meta, result, limits.MaxSessionBytes)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			if saveErr != nil {
				fmt.Fprintf(os.Stderr, "miniagent: save session: %v\n", saveErr)
			}
			return err // caller maps to 130 / closed stream; no error event
		}
		emitRunError(out, err, spec.resultOnly, e.logger)
		if saveErr != nil {
			fmt.Fprintf(os.Stderr, "miniagent: save session: %v\n", saveErr)
		}
		return err
	}
	if rerr := emitRunResult(out, result, resolved.ModelID, spec.resultOnly, e.logger); rerr != nil {
		return rerr
	}
	if saveErr != nil {
		return saveErr
	}
	return nil
}

// saveSession rewrites the session jsonl (first meta line accumulates LLMRequests across turns).
// CLI path shields the write from SIGINT/SIGTERM (protectSignal=true); the serve path leaves
// signal disposition to the server — os.Rename keeps the file either old or new, never torn.
func (e *turnEngine) saveSession(sessPath string, meta session.SessionMeta, result miniagent.Result, maxSessionBytes int) error {
	if sessPath == "" || len(result.NewMessages) == 0 {
		return nil
	}
	if e.protectSignal {
		signal.Ignore(syscall.SIGINT, syscall.SIGTERM)
		defer signal.Reset(syscall.SIGINT, syscall.SIGTERM)
	}
	meta.LLMRequests += result.LLMRequests
	return session.RewriteMessages(sessPath, meta, result.Messages, int64(maxSessionBytes))
}
