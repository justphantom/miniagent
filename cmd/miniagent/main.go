// Command miniagent runs an agent turn (or interactive loop) from stdin and
// emits NDJSON events to stdout.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/justphantom/miniagent/internal/miniagent"
)

var version = "dev"

type cliFlags struct {
	model         *string
	keyFile       *string
	system        *string
	maxTokens     *int
	workdir       *string
	session       *string
	logLevel      *string
	showVer       *bool
	maxIterations *int
	stream        *bool
	interactive   *bool
	listModels    *bool
	configPath    *string
	mode          *string
	thinking      *string
	resultOnly    *bool
}

func parseFlags() *cliFlags {
	f := &cliFlags{}
	f.configPath = flag.String("config", "", "配置文件路径（默认查 ./miniagent.json；不存在则生成最小模板）")
	f.mode = flag.String("mode", "", "权限模式 default|auto（default 时 workdir 必填）；默认 default")
	f.thinking = flag.String("thinking", "", "思考级别 off|minimal|low|medium|high|xhigh|max（默认 off）")
	f.resultOnly = flag.Bool("result-only", false, "仅输出 result.text（subagent fork 用）；与 -stream 互斥")
	f.model = flag.String("model", "", "LLM model（config 模式 provider/id）")
	f.keyFile = flag.String("key-file", "", "从文件读 API key（首尾空白截断）；优先于 provider.key/$MINIAGENT_API_KEY")
	f.system = flag.String("system", defaultSystemPrompt, "system prompt")
	f.maxTokens = flag.Int("max-tokens", 4096, "max output tokens per LLM call")
	f.workdir = flag.String("workdir", "", "working directory (default 模式写工具边界 + shell cwd)")
	f.session = flag.String("session", "", "session id 或路径（id 在 session.dir 解析为 .jsonl）")
	f.logLevel = flag.String("log-level", "info", "log level: debug|info|warn|error")
	f.maxIterations = flag.Int("max-iterations", 0, "单轮 LLM 调用上限（0=默认 20）")
	f.stream = flag.Bool("stream", false, "流式输出（SSE）；默认非流式")
	f.interactive = flag.Bool("interactive", false, "交互模式：循环读取 prompt（每行一个）")
	f.listModels = flag.Bool("list-models", false, "列出端点可用模型 id 后退出")
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

	if *f.listModels {
		// list-models 不要求 -model（本就为发现模型），故不走 Resolve。
		p, err := providerForListModels(cfg, f)
		if err != nil {
			fmt.Fprintf(os.Stderr, "miniagent: %v\n", err)
			os.Exit(1)
		}
		chat, _ := buildLLM(resolveFinalKey(p.Key, *f.keyFile), p, logger)
		ids, err := miniagent.ListAvailableModels(context.Background(), chat, p)
		if err != nil {
			fmt.Fprintf(os.Stderr, "miniagent: list models: %v\n", err)
			os.Exit(1)
		}
		for _, id := range ids {
			fmt.Println(id)
		}
		return
	}

	resolved, err := miniagent.Resolve(cfg, collectOverrides(f))
	if err != nil {
		fmt.Fprintf(os.Stderr, "miniagent: %v\n", err)
		os.Exit(1)
	}
	apiKey := resolveFinalKey(resolved.Provider.Key, *f.keyFile)

	validateConversation(resolved, f)
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "miniagent: API key 缺失（provider.key / -key-file / $MINIAGENT_API_KEY）")
		os.Exit(1)
	}
	warnInsecureURL(resolved.Provider.ChatURL)

	sessionDir := defaultSessionDir
	if resolved.Session.Dir != "" {
		sessionDir = resolved.Session.Dir
	}
	workdir := effectiveWorkdir(resolved, f)
	modelSpec := resolved.Provider.Name + "/" + resolved.ModelID
	sessPath, meta, history := resolveSessionForRun(*f.session, sessionDir, modelSpec, workdir)
	// P0/P5：发现 .miniagent/ 项目规则与记忆，合并进 system prompt（persona>rules>defaults）。
	pr := loadProjectRules(workdir)
	resolved.System = mergeSystemPrompt(resolved.System, pr.persona, pr.rules, pr.memory, pr.hasAny())
	resolved.System = injectSubagentGuidance(resolved.System, absConfigPath(*f.configPath), meta.ID, resolved.Mode)

	chat, stream := buildLLM(apiKey, resolved.Provider, logger)
	tools := buildTools(workdir, shellTimeoutOf(resolved), resolved.Mode, into(resolved.Run.MaxFileResultChars, 0), pr.scripts)
	reader := bufio.NewReader(os.Stdin)
	hooks := buildHooks(*f.resultOnly)
	baseCfg := loopCfg(resolved, f, history, tools)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if d := maxDurationOf(resolved); d > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}

	if *f.interactive {
		os.Exit(runInteractive(ctx, chat, stream, baseCfg, sessPath, meta, hooks, logger, reader))
	}

	prompt := mustReadPrompt(reader)
	result, err := miniagent.Run(ctx, chat, stream, baseCfg, string(prompt), hooks, logger)
	if err != nil {
		// 信号取消（SIGINT/SIGTERM）走码 130 干净退出，不 emit error（审查 P3 SIGINT 退出码）。
		if errors.Is(err, context.Canceled) {
			os.Exit(130)
		}
		emitRunError(err, *f.resultOnly, logger)
		os.Exit(1)
	}
	emitRunResult(result, resolved.ModelID, *f.resultOnly, logger)
	if sessPath != "" {
		// Compacted 时 rewrite 全量 transcript 丢弃被屏障中段（审查 P2 session 文件永不压缩）；
		// 否则 append-only 追加 NewMessages。
		var saveErr error
		if result.Compacted {
			saveErr = miniagent.RewriteMessages(sessPath, meta, result.Messages)
		} else {
			saveErr = miniagent.AppendMessages(sessPath, meta, result.NewMessages)
		}
		if saveErr != nil {
			fmt.Fprintf(os.Stderr, "miniagent: save session: %v\n", saveErr)
			os.Exit(1)
		}
	}
}

// loopCfg 按 resolved（cli>config）覆盖 flag 默认，构造 LoopConfig。System 空回落默认 prompt。
func loopCfg(resolved *miniagent.Resolved, f *cliFlags, history []miniagent.Message, tools []miniagent.Tool) miniagent.LoopConfig {
	system := resolved.System
	if system == "" {
		system = defaultSystemPrompt
	}
	compModel := resolved.Compaction.Model
	if compModel == "" {
		compModel = resolved.ModelID
	}
	return miniagent.LoopConfig{
		Model:              resolved.ModelID,
		System:             system,
		SummaryRequest:     resolved.SummaryRequest,
		SummarizerPrompt:   resolved.SummarizerPrompt,
		MaxTokens:          into(resolved.Run.MaxTokens, *f.maxTokens),
		Tools:              tools,
		History:            history,
		MaxIterations:      into(resolved.Run.MaxIterations, *f.maxIterations),
		MaxTotalTokens:     into(resolved.Run.MaxTotalTokens, 0),
		Stream:             intoBool(resolved.Run.Stream, *f.stream),
		ContextWindow:      into(resolved.Run.ContextWindow, 0),
		ThinkingLevel:      resolved.Thinking,
		Thinking:           resolved.Provider.Thinking,
		CompactionModel:    compModel,
		MaxToolResultChars: into(resolved.Run.MaxToolResultChars, 0),
		MaxFileResultChars: into(resolved.Run.MaxFileResultChars, 0),
		MaxParallelTools:   into(resolved.Run.MaxParallelTools, 0),
		ContextKeepRecent:  into(resolved.Run.ContextKeepRecent, 0),
		SummaryMaxChars:    into(resolved.Run.SummaryMaxChars, 0),
	}
}

func intoBool(ov *bool, def bool) bool {
	if ov != nil {
		return *ov
	}
	return def
}

// into 解析 *int 覆盖：ov 非 nil 用 *ov，否则用 def。loopCfg 与 main 的 buildTools 调用共用。
func into(ov *int, def int) int {
	if ov != nil {
		return *ov
	}
	return def
}
