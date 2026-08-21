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
	"syscall"

	"github.com/justphantom/miniagent/config"
)

// version is injected at build time via make build using -ldflags "-X main.version=$(git describe --tags)";
// falls back to "dev" when not injected (plain go build).
var version = "dev"

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
	thinking           *string
	resultOnly         *bool
	confirmDestructive *bool
	metricsStep        *bool
	replay             *string
	serve              *bool
}

func parseFlags() *cliFlags {
	f := &cliFlags{}
	f.configPath = flag.String("config", "", "path to config file (default looks up ~/.miniagent/miniagent.json; errors if not found)")
	f.logLevel = flag.String("log-level", "info", "log level: debug|info|warn|error")
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
	f.serve = flag.Bool("serve", false, "start the WebUI HTTP server (config web.listen, default 127.0.0.1:8787; auth via web.key/$MINIAGENT_WEB_KEY)")
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

	if *f.serve {
		runServe(ctx, cfg, absConfigPath(*f.configPath), logger)
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

	validateConversation(resolved, f)

	prompt := mustReadPrompt(ctx, os.Stdin)
	eng := &turnEngine{
		cfg:           cfg,
		cfgPath:       absConfigPath(*f.configPath),
		logger:        logger,
		buildClients:  buildRuntimeClients,
		protectSignal: true,
	}
	spec := turnSpec{
		prompt:      string(prompt),
		workdir:     effectiveWorkdir(f),
		sessionArg:  *f.session,
		saveNew:     *f.saveSession,
		resultOnly:  *f.resultOnly,
		maxIterDef:  *f.maxIterations,
		metricsStep: *f.metricsStep,
		overrides:   collectOverrides(f),
	}
	err = eng.runTurn(ctx, spec, os.Stdout)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			os.Exit(130)
		}
		// Mid-run failures already streamed an NDJSON error event (stdout contract); the stderr line
		// duplicates it for humans — pre-refactor behavior printed resolve/session/key errors here.
		fmt.Fprintf(os.Stderr, "miniagent: %v\n", err)
		os.Exit(1)
	}
}
