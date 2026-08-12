// Command miniagent runs an agent turn from stdin and
// emits NDJSON events to stdout.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/justphantom/miniagent/internal/miniagent"
	"github.com/justphantom/miniagent/internal/miniagent/compaction"
	"github.com/justphantom/miniagent/internal/miniagent/config"
	"github.com/justphantom/miniagent/internal/miniagent/event"
	"github.com/justphantom/miniagent/internal/miniagent/metrics"
	"github.com/justphantom/miniagent/internal/miniagent/policy"
	"github.com/justphantom/miniagent/internal/miniagent/session"
)

// version is injected at build time via make build using -ldflags "-X main.version=$(git describe --tags)";
// we do not hardcode a literal here to avoid stale values after release. Empty string when not injected.
var version string

type cliFlags struct {
	provider           *string
	model              *string
	workdir            *string
	session            *string
	saveSession        *bool
	logLevel           *string
	showVer            *bool
	maxIterations      *int
	stream             *bool
	listModels         *bool
	configPath         *string
	mode               *string
	thinking           *string
	resultOnly         *bool
	confirmDestructive *bool
	metricsStep        *bool
	replay             *string
}

func parseFlags() *cliFlags {
	f := &cliFlags{}
	f.configPath = flag.String("config", "", "path to config file (default looks up ~/.miniagent/miniagent.json; errors if not found)")
	f.mode = flag.String("mode", "", "permission mode default|auto (workdir required when default); defaults to default")
	f.thinking = flag.String("thinking", "", "thinking level off|minimal|low|medium|high|xhigh|max (default off)")
	f.resultOnly = flag.Bool("result-only", false, "output only result.text (for subagent fork); mutually exclusive with -stream")
	f.confirmDestructive = flag.Bool("confirm-destructive", false, "opt-in: prompt before write/edit and dangerous shell; in non-interactive/subagent mode destructive tools are denied unless MINIAGENT_AUTO_APPROVE=1")
	f.metricsStep = flag.Bool("metrics-step", false, "emit per-step metrics NDJSON to stderr (step, transcript len, token spend, compaction, llm requests)")
	f.provider = flag.String("provider", "", "LLM provider name (pairs with -model to override the defaults pair; standalone for filtering with -list-models)")
	f.model = flag.String("model", "", "LLM model id (pairs with -provider to override the defaults pair)")
	f.workdir = flag.String("workdir", "", "working directory (REQUIRED, must be absolute; constrains write-tool boundaries + shell cwd)")
	f.session = flag.String("session", "", "resume an existing session by id (resolved to a .jsonl file under session.dir; errors if not found)")
	f.saveSession = flag.Bool("save-session", false, "create a new session and persist it (id generated internally; mutually exclusive with -session)")
	f.replay = flag.String("replay", "", "replay the specified session (reads the session file and replays the process without calling the LLM; mutually exclusive with -save-session/-session/-result-only)")
	f.logLevel = flag.String("log-level", "info", "log level: debug|info|warn|error")
	f.maxIterations = flag.Int("max-iterations", 0, "upper bound on LLM calls per turn (0=default 20)")
	f.stream = flag.Bool("stream", false, "stream output (SSE); non-streaming by default")
	f.listModels = flag.Bool("list-models", false, "list available model ids on the endpoint then exit")
	f.showVer = flag.Bool("version", false, "show version")
	flag.Parse()
	return f
}

func main() {
	f := parseFlags()
	if *f.showVer {
		fmt.Printf("miniagent %s\n", version)
		os.Exit(0)
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: mustParseLogLevel(*f.logLevel)}))

	cfg, err := requireConfig(*f.configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "miniagent: config: %v\n", err)
		os.Exit(1)
	}

	// Both SIGINT and SIGTERM route to ctx cancellation → exit code 130 (128+SIGINT) as a generic "interrupted by signal" code;
	// we do not distinguish POSIX SIGTERM=143: consumers only need to recognize "interrupted by signal" rather than the specific signal, most tools handle them together.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if *f.listModels {
		// list-models does not require -provider/-model (its purpose is to discover models), so it skips Resolve;
		// it outputs one NDJSON line per model {"type":"model","provider","model"}.
		runListModels(ctx, cfg, *f.provider, logger)
		return
	}

	resolved, err := config.Resolve(cfg, collectOverrides(f))
	if err != nil {
		fmt.Fprintf(os.Stderr, "miniagent: %v\n", err)
		os.Exit(1)
	}
	// -replay: offline replay of the specified session, does not call the LLM / does not need a key / does not persist / does not read stdin. Short-circuit location chosen deliberately:
	// after Resolve (needs resolved.Session.Dir to resolve the session directory), before validateConversation
	// (does not need workdir), before apiKey (does not need a key), before mustReadPrompt (does not read stdin).
	// The three sessionDir lines share logic with the main path below; inlined to avoid restructuring the main flow.
	if *f.replay != "" {
		if *f.saveSession || *f.session != "" || *f.resultOnly {
			fmt.Fprintln(os.Stderr, "miniagent: -replay is mutually exclusive with -save-session/-session/-result-only")
			os.Exit(1)
		}
		sessionDir := defaultSessionDir
		if resolved.Session.Dir != "" {
			sessionDir = resolved.Session.Dir
		}
		runReplay(os.Stdout, sessionDir, *f.replay, int64(maxSessionBytesOf(resolved)))
		return
	}

	apiKey := resolveFinalKey(resolved.Provider.Key)

	validateConversation(resolved, f)
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "miniagent: API key missing (provider.key / $MINIAGENT_API_KEY)")
		os.Exit(1)
	}
	warnInsecureURL(resolved.Provider.ChatURL)

	sessionDir := defaultSessionDir
	if resolved.Session.Dir != "" {
		sessionDir = resolved.Session.Dir
	}
	workdir := effectiveWorkdir(f)
	modelSpec := resolved.Provider.Name + "/" + resolved.ModelID
	sessPath, meta, history := resolveSessionForRun(*f.saveSession, *f.session, sessionDir, modelSpec, resolved.Provider.Name, workdir, int64(maxSessionBytesOf(resolved)))
	if *f.saveSession {
		// New session: session metadata is emitted as the first NDJSON event on stdout (mirrors the first line of the jsonl) so consumers can programmatically capture the id to resume.
		// The mutual-exclusion check guarantees -result-only never triggers this, so it does not pollute the pure-text stdout of subagents.
		// ⚠️ id is emitted before Run, and jsonl is only persisted after Run succeeds; if Run fails / stdin is empty, the id captured by the consumer does not exist on disk — the consumer must verify the exit code.
		if err := event.EmitSession(os.Stdout, meta); err != nil {
			fmt.Fprintf(os.Stderr, "miniagent: emit session: %v\n", err)
			os.Exit(1)
		}
	}
	// System prompt: config defaults.system_prompt with the built-in defaultSystemPrompt fallback, then subagent guidance (see assembleSystemPrompt).
	resolved.System = assembleSystemPrompt(resolved.System, resolved.SubagentGuidance, absConfigPath(*f.configPath), resolved.Mode, workdir, resolved.RulesFile)

	// Apply runtime config overrides (precedence: config>builtin).
	limits := miniagent.Limits{
		MaxReadFileBytes:       maxReadFileBytesOf(resolved),
		MaxShellOutputChars:    maxShellOutputCharsOf(resolved),
		ShellStreamWindowBytes: into(resolved.RunConfig.ShellStreamWindowBytes, 0),
		MaxGrepMatches:         into(resolved.RunConfig.GrepMaxMatches, 0),
		MaxSessionBytes:        maxSessionBytesOf(resolved),
		ContextTrimToolChars:   into(resolved.RunConfig.ContextTrimToolChars, 0),
	}

	// Main provider chat/stream + summary compChat (when crossing providers); os.Exit on missing key or invalid endpoint.
	llm, compChat := buildRuntimeClients(resolved, apiKey, logger)

	tools := buildTools(workdir, shellTimeoutOf(resolved), fileOpTimeoutOf(resolved), writeTimeoutOf(resolved), resolved.Mode, into(resolved.RunConfig.MaxFileResultChars, 0), limits, intoBool(resolved.RunConfig.ConfineAuto, false), intoBool(resolved.RunConfig.ConfineEvalSymlinks, false), intoBool(resolved.RunConfig.ShellConfineCd, false), resolved.RunConfig.ShellAllowlist)
	baseCfg := loopCfg(resolved, f, history, tools)
	warnNoBudgetFuse(resolved, f, logger)
	// §P1-A: tool output persist directory — explicit config wins; otherwise, when -save-session/-session is active, derive from the session directory
	// as <sessionDir>/<id>.tool-output/ (disabled when there is no session and no config set).
	if resolved.RunConfig.ToolOutputDir != nil && *resolved.RunConfig.ToolOutputDir != "" {
		baseCfg.ToolOutputDir = *resolved.RunConfig.ToolOutputDir
	} else if sessPath != "" {
		baseCfg.ToolOutputDir = filepath.Join(filepath.Dir(sessPath), strings.TrimSuffix(filepath.Base(sessPath), ".jsonl")+".tool-output")
	}
	if resolved.Run.ToolOutputRetention != nil {
		baseCfg.ToolOutputRetention = *resolved.Run.ToolOutputRetention
	}
	// Compaction as a plugin: obtain before/after via NewCompaction; the three default policies (OnLLMError/OnBudget/ShapeToolResult) are attached via assembleHooks.
	compBefore, compAfter := compaction.NewCompaction(compactionOptions(resolved, meta, llm, compChat, baseCfg.System, tools, logger))
	hooks := assembleHooks(compBefore, compAfter, *f.resultOnly, intoBool(resolved.Run.ConfirmDestructive, *f.confirmDestructive), baseCfg, tools, limits, logger)
	if *f.metricsStep {
		hooks.OnStep = metrics.NewStepEmitter(os.Stderr).Emit
	}

	prompt := mustReadPrompt(ctx, os.Stdin)
	// runCtx carries the -max-duration timeout (if any); constructed after stdin read — mustReadPrompt uses the signal ctx and is unconstrained.
	// If runCtx were constructed earlier, a slow stdin would consume the max-duration budget causing Run to get an already-expired ctx (DeadlineExceeded exit 1).
	runCtx := ctx
	if d := maxDurationOf(resolved); d > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(runCtx, d)
		defer cancel()
	}
	result, err := miniagent.Run(runCtx, llm, baseCfg, string(prompt), hooks, logger)

	// saveSession recovers the already-executed part of this turn for resume: Run via defer guarantees result.Messages/NewMessages are returned on
	// error/cancel paths as well, and tool_call↔tool_result pairing is completed by fillPlaceholderTail — hence it persists
	// not only on the success path but also on error/cancel paths, eliminating the "tool already executed, jsonl not appended" orphan inconsistency
	// (e.g. tool-output residual while jsonl stops at the previous turn). SIGINT/SIGTERM are ignored during save: avoid truncating the session file or leaving temp files behind.
	// Returns saveErr for the caller to decide the exit code: failure on the success path still exits 1 (original semantics); error/cancel paths only warn without changing the code.
	saveSession := func() (saveErr error) {
		if sessPath == "" || len(result.NewMessages) == 0 {
			return nil
		}
		signal.Ignore(syscall.SIGINT, syscall.SIGTERM)
		// Accumulate the cross-turn cumulative LLM request count into the session metadata first line.
		// Always use RewriteMessages: the first meta line (LLMRequests) needs updating, append-only cannot change the first line.
		// The session file has a MaxSessionBytes upper bound and saveSession is a single call off the hot path, so the full rewrite cost is negligible.
		meta.LLMRequests += result.LLMRequests
		saveErr = session.RewriteMessages(sessPath, meta, result.Messages, int64(limits.MaxSessionBytes))
		signal.Reset(syscall.SIGINT, syscall.SIGTERM)
		return saveErr
	}

	if err != nil {
		// Signal cancellation (SIGINT/SIGTERM) takes code 130 to exit cleanly, does not emit error (review P3 SIGINT exit code).
		// Cancellation also persists: pairing is complete, recovering the executed part for the next continuation.
		if errors.Is(err, context.Canceled) {
			if se := saveSession(); se != nil {
				fmt.Fprintf(os.Stderr, "miniagent: save session: %v\n", se)
			}
			os.Exit(130)
		}
		emitRunError(err, *f.resultOnly, logger)
		if se := saveSession(); se != nil {
			fmt.Fprintf(os.Stderr, "miniagent: save session: %v\n", se)
		}
		os.Exit(1)
	}
	emitRunResult(result, resolved.ModelID, *f.resultOnly, logger)
	if se := saveSession(); se != nil {
		fmt.Fprintf(os.Stderr, "miniagent: save session: %v\n", se)
		os.Exit(1)
	}
}

// buildRuntimeClients constructs the main provider's LLM and (when compaction crosses providers) the
// summarization Doer. compChat is left nil when compaction uses the same provider (compaction falls back to
// llm, which satisfies miniagent.Doer). os.Exit on missing key or invalid endpoint.
func buildRuntimeClients(resolved *config.Resolved, apiKey string, logger *slog.Logger) (miniagent.LLM, miniagent.Doer) {
	llm := buildLLM(apiKey, resolved.Provider, logger, httpTimeoutOf(resolved))
	if resolved.CompactionProvider.Name != resolved.Provider.Name {
		key := resolveFinalKey(resolved.CompactionProvider.Key)
		if key == "" {
			fmt.Fprintf(os.Stderr, "miniagent: compaction provider API key missing (provider.key / $MINIAGENT_API_KEY)\n")
			os.Exit(1)
		}
		warnProviderInsecureURLs(resolved.CompactionProvider)
		return llm, buildDoer(key, resolved.CompactionProvider, logger, httpTimeoutOf(resolved))
	}
	return llm, nil
}

// assembleHooks assembles LoopHooks: event output (buildHooks) + compaction before/after + three default policy attachments
// (OnLLMError history-tightening retry / OnBudget estimation+circuit-break / ShapeToolResult truncation+persist). The core Run has zero policies; this function assembles them to restore full capability.
func assembleHooks(
	compBefore func(context.Context, miniagent.StepInput) (miniagent.StepOutput, error),
	compAfter func(context.Context, int, miniagent.Response) error,
	resultOnly bool, confirmDestructive bool, baseCfg miniagent.LoopConfig, tools []miniagent.Tool, limits miniagent.Limits, logger *slog.Logger,
) miniagent.LoopHooks {
	hooks := buildHooks(resultOnly)
	hooks.BeforeLLM = compBefore
	hooks.AfterLLM = compAfter
	hooks.OnLLMError = policy.NewDefaultOnLLMError(logger, limits.ContextTrimToolChars)
	hooks.OnBudget = policy.NewDefaultOnBudget(baseCfg.MaxTotalTokens, logger)
	hooks.ShapeToolResult = policy.NewDefaultShapeToolResult(tools, baseCfg.ToolOutputDir, baseCfg.ToolOutputRetention, baseCfg.MaxToolResultChars, logger)
	// S-2 (opt-in): wrap OnToolUse with a destructive-tool confirmation gate. buildHooks returns empty hooks (no
	// OnToolUse) for -result-only/subagent mode, so this wraps AFTER buildHooks in both paths — when enabled the gate
	// is active even for subagents (deny-by-default, since they have no TTY), closing the otherwise-uncovered
	// autonomous path; when disabled ConfirmOnToolUse is the identity, leaving behavior unchanged.
	hooks.OnToolUse = policy.ConfirmOnToolUse(hooks.OnToolUse, policy.ConfirmCfg{
		Enabled:     confirmDestructive,
		AutoApprove: os.Getenv("MINIAGENT_AUTO_APPROVE") == "1",
	})
	return hooks
}

// loopCfg overrides flag defaults per resolved (cli>config) to construct LoopConfig (loop body + policy-carrier fields;
// compaction policies are attached via NewCompaction, other policies via NewDefault* hook factories, core Run has zero policies).
// resolved.System must already be non-empty (the production path guarantees this via assembleSystemPrompt at main.go:147);
// loopCfg does not re-default it — a missing System here is a caller bug, surfaced as an empty prompt rather than silently papered over.
func loopCfg(resolved *config.Resolved, f *cliFlags, history []miniagent.Message, tools []miniagent.Tool) miniagent.LoopConfig {
	return miniagent.LoopConfig{
		Model:              resolved.ModelID,
		System:             resolved.System,
		SummaryRequest:     resolved.SummaryRequest,
		MaxTokens:          into(resolved.MaxTokens, 0),
		Tools:              tools,
		History:            history,
		MaxIterations:      into(resolved.Run.MaxIterations, *f.maxIterations),
		MaxTotalTokens:     into(resolved.RunConfig.MaxTotalTokens, 0),
		Stream:             intoBool(resolved.Run.Stream, *f.stream),
		ThinkingLevel:      resolved.Thinking,
		Thinking:           resolved.Provider.Thinking,
		MaxToolResultChars: into(resolved.RunConfig.MaxToolResultChars, 0),
		MaxParallelTools:   into(resolved.RunConfig.MaxParallelTools, 0),
	}
}

// warnNoBudgetFuse (C3): run.max_tokens_total defaults to nil → OnBudget skips the cumulative-spend check entirely,
// so a long Run can burn unbounded tokens until repeated context-window compaction. The default-config single turn is
// already bounded by soft fuses (max_iterations=20, CompactionAuto default-on, optional max-duration); the real footgun
// is cross-turn -session resume accumulation and single Runs with iterations raised above 20. Warn (do NOT auto-pick a
// budget — that would decide for the user) only when the fuse is unset AND the run is long-session-prone.
func warnNoBudgetFuse(resolved *config.Resolved, f *cliFlags, logger *slog.Logger) {
	iter := into(resolved.Run.MaxIterations, *f.maxIterations)
	if !shouldWarnBudgetFuse(resolved.RunConfig.MaxTotalTokens != nil, *f.session, iter) {
		return
	}
	logger.Warn("run.max_tokens_total is not set: no cumulative token budget fuse",
		"resume", *f.session != "", "max_iterations", iter,
		"hint", "set run.max_tokens_total to cap cumulative spend (especially across -session resumes)")
}

// shouldWarnBudgetFuse reports whether the C3 no-fuse warning should fire: the cumulative budget fuse is unset AND the
// run is long-session-prone — a cross-turn -session resume (the real cumulative-spend footgun) or a single Run with
// iterations raised above the default 20. Default single turns are left alone (soft fuses already bound them). Pure
// predicate for testability; 20 mirrors the maxIterations default.
func shouldWarnBudgetFuse(maxTotalTokensSet bool, session string, maxIterations int) bool {
	if maxTotalTokensSet {
		return false
	}
	return session != "" || maxIterations > 20
}

// compactionOptions assembles the resolved compaction policies into CompactionOptions. chat is the summarization client
// (use compChat if non-nil, otherwise fall back to main chat).
// compactionOptions assembles the resolved compaction policies into CompactionOptions. mainDoer is the
// summarization client fallback (used when compDoer is nil, i.e. compaction shares the main provider).
func compactionOptions(resolved *config.Resolved, meta session.SessionMeta, mainDoer, compDoer miniagent.Doer, system string, tools []miniagent.Tool, logger *slog.Logger) compaction.CompactionOptions {
	compClient := compDoer
	if compClient == nil {
		compClient = mainDoer
	}
	return compaction.CompactionOptions{
		Chat:                     compClient,
		ContextWindow:            into(resolved.ContextWindow, 0),
		Model:                    resolved.ModelID,
		CompactionModel:          resolved.CompactionModelID,
		System:                   system,
		Tools:                    tools,
		KeepRecent:               into(resolved.RunConfig.ContextKeepRecent, 0),
		KeepReasoning:            into(resolved.RunConfig.ContextKeepReasoning, 0),
		KeepToolArgs:             into(resolved.RunConfig.ContextKeepToolArgs, 0),
		KeepReasoningChars:       into(resolved.RunConfig.ContextKeepReasoningChars, 0),
		SummarizerPrompt:         resolved.SummarizerPrompt,
		SummaryCreateInstruction: resolved.SummaryCreateInstruction,
		SummaryUpdateInstruction: resolved.SummaryUpdateInstruction,
		SummaryTemplate:          resolved.SummaryTemplate,
		SummaryMaxChars:          into(resolved.RunConfig.SummaryMaxChars, 0),
		SummaryMaxTokens:         into(resolved.RunConfig.SummaryMaxTokens, 0),
		PreserveRecentTokens:     into(resolved.RunConfig.PreserveRecentTokens, 0),
		UseRealUsage:             intoBool(resolved.RunConfig.ContextUseRealUsage, true),
		Auto:                     resolved.CompactionAuto,
		Reserved:                 resolved.CompactionReserved,
		SessionID:                meta.ID,
		Logger:                   logger,
	}
}

func intoBool(ov *bool, def bool) bool {
	if ov != nil {
		return *ov
	}
	return def
}

// into parses *int overrides: if ov is non-nil use *ov, otherwise use def. Shared by loopCfg and main's buildTools.
func into(ov *int, def int) int {
	if ov != nil {
		return *ov
	}
	return def
}
