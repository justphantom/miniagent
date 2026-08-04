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

var version = "3.4.0"

type cliFlags struct {
	model         *string
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
	f.configPath = flag.String("config", "", "配置文件路径（默认查 ~/.miniagent/miniagent.json；不存在则报错）")
	f.mode = flag.String("mode", "", "权限模式 default|auto（default 时 workdir 必填）；默认 default")
	f.thinking = flag.String("thinking", "", "思考级别 off|minimal|low|medium|high|xhigh|max（默认 off）")
	f.resultOnly = flag.Bool("result-only", false, "仅输出 result.text（subagent fork 用）；与 -stream 互斥")
	f.model = flag.String("model", "", "LLM model（config 模式 provider/id）")
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

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if *f.listModels {
		// list-models 不要求 -model（本就为发现模型），故不走 Resolve。
		// 统一输出 "provider/model_id"（单 provider 也带前缀），-model 可筛选单个 provider。
		listHTTPTimeout, err := httpTimeoutFromConfig(cfg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "miniagent: config: %v\n", err)
			os.Exit(1)
		}

		providers := cfg.Providers
		if f.model != nil && *f.model != "" {
			p, _, err := miniagent.ParseModelSpec(*f.model, cfg)
			if err != nil {
				fmt.Fprintf(os.Stderr, "miniagent: %v\n", err)
				os.Exit(1)
			}
			providers = []miniagent.ProviderConfig{p}
		}
		warnProvidersInsecureURLs(providers)
		ids, err := listAllModels(ctx, providers, listHTTPTimeout, logger)
		for _, id := range ids {
			fmt.Println(id)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "miniagent: list models: %v\n", err)
			os.Exit(1)
		}
		return
	}

	resolved, err := miniagent.Resolve(cfg, collectOverrides(f))
	if err != nil {
		fmt.Fprintf(os.Stderr, "miniagent: %v\n", err)
		os.Exit(1)
	}
	apiKey := resolveFinalKey(resolved.Provider.Key)

	validateConversation(resolved, f)
	if apiKey == "" {
		fmt.Fprintln(os.Stderr, "miniagent: API key 缺失（provider.key / $MINIAGENT_API_KEY）")
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
	// memory_recent_n 必须在 loadProjectRules（内部 FormatMemorySnippet 已格式化注入片段）
	// 之前生效，否则 run.memory_recent_n 对当前进程的注入无效（启动快照已用旧 N）。
	miniagent.SetMemoryRecentN(into(resolved.Run.MemoryRecentN, 0))
	// P0/P5：发现 .miniagent/ 项目规则与记忆，合并进 system prompt（persona>rules>defaults）。
	pr := loadProjectRules(workdir)
	resolved.System = mergeSystemPrompt(resolved.System, pr.persona, pr.rules, pr.memory, pr.hasAny())
	resolved.System = injectSubagentGuidance(resolved.System, absConfigPath(*f.configPath), meta.ID, resolved.Mode)

	// 应用运行时配置覆盖（优先级：config>builtin）。
	miniagent.SetMaxReadFileBytes(maxReadFileBytesOf(resolved))
	miniagent.SetMaxShellOutputChars(maxShellOutputCharsOf(resolved))
	miniagent.SetMaxSessionBytes(maxSessionBytesOf(resolved))

	miniagent.SetSummaryMaxTokens(into(resolved.Run.SummaryMaxTokens, 0))
	miniagent.SetGrepMaxMatches(into(resolved.Run.GrepMaxMatches, 0))
	miniagent.SetContextTrimToolChars(into(resolved.Run.ContextTrimToolChars, 0))

	chat, stream := buildLLM(apiKey, resolved.Provider, logger, httpTimeoutOf(resolved))

	// secondaryClient 为与主 provider 不同的二级 provider 解析 key + 建非流式 client（仅 Do）。
	secondaryClient := func(label string, prov miniagent.ProviderConfig) (*miniagent.ChatClient, string) {
		key := resolveFinalKey(prov.Key)
		if key == "" {
			fmt.Fprintf(os.Stderr, "miniagent: %s provider API key 缺失（provider.key / $MINIAGENT_API_KEY）\n", label)
			os.Exit(1)
		}
		warnProviderInsecureURLs(prov)
		return buildChatClient(key, prov, logger, httpTimeoutOf(resolved)), key
	}

	// compaction client：与主 provider 相同则留 nil（loop 回落主 chat），否则新建。
	var compChat *miniagent.ChatClient
	compKey := ""
	if resolved.CompactionProvider.Name != resolved.Provider.Name {
		compChat, compKey = secondaryClient("compaction", resolved.CompactionProvider)
	}

	// memory client：按 provider 名去重复用——与主或 compaction 相同则复用对应 client，否则新建。
	var memChat *miniagent.ChatClient
	memKey := ""
	switch resolved.MemoryProvider.Name {
	case resolved.Provider.Name:
		memChat = chat
	case resolved.CompactionProvider.Name:
		memChat = compChat // compaction==main 时本 case 不会命中（已被上一 case 接走）
	default:
		memChat, memKey = secondaryClient("memory", resolved.MemoryProvider)
	}

	tools := buildTools(workdir, shellTimeoutOf(resolved), fileOpTimeoutOf(resolved), writeTimeoutOf(resolved), resolved.Mode, into(resolved.Run.MaxFileResultChars, 0), pr.scripts)
	reader := bufio.NewReader(os.Stdin)
	hooks := buildHooks(*f.resultOnly)
	baseCfg := loopCfg(resolved, f, history, tools)
	baseCfg.CompactionChat = compChat

	// 会话结束自动抽取项目记忆：用 memory 专属 client（memChat，按 provider 去重后可能 == chat/compChat）。
	// 默认 on（resolved.MemoryAutoUpdate）。secrets 收集所有用到的 key 字面量，用于剔除含 key 的记录。
	secrets := []string{apiKey}
	if compKey != "" {
		secrets = append(secrets, compKey)
	}
	if memKey != "" {
		secrets = append(secrets, memKey)
	}
	memExtractor := &memoryExtractor{
		enabled: resolved.MemoryAutoUpdate,
		workdir: workdir,
		model:   resolved.MemoryModelID,
		maxK:    resolved.MemoryMaxPerSession,
		prompt:  resolved.MemoryExtractPrompt,
		secrets: secrets,
		client:  memChat,
		logger:  logger,
	}

	// runCtx 供单次运行与交互循环：含 -max-duration 超时（若有）；信号处理由各自路径自行注册。
	runCtx := ctx
	if d := maxDurationOf(resolved); d > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(runCtx, d)
		defer cancel()
	}

	if *f.interactive {
		os.Exit(runInteractive(runCtx, chat, stream, baseCfg, sessPath, meta, hooks, logger, reader, memExtractor))
	}

	prompt := mustReadPrompt(reader)
	result, err := miniagent.Run(runCtx, chat, stream, baseCfg, string(prompt), hooks, logger)
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
		// 保存期间忽略 SIGINT/SIGTERM：避免截断 session 文件或残留临时文件。
		// 保存完成后进程即退出，signal.Reset 在之后恢复默认行为。
		signal.Ignore(syscall.SIGINT, syscall.SIGTERM)
		var saveErr error
		if result.Compacted {
			saveErr = miniagent.RewriteMessages(sessPath, meta, result.Messages)
		} else {
			saveErr = miniagent.AppendMessages(sessPath, meta, result.NewMessages)
		}
		signal.Reset(syscall.SIGINT, syscall.SIGTERM)
		if saveErr != nil {
			fmt.Fprintf(os.Stderr, "miniagent: save session: %v\n", saveErr)
			os.Exit(1)
		}
	}

	// 非交互：会话结束抽取记忆（best-effort，失败仅 warn，不影响退出码）。
	// 内部 use context.Background()，避免 -max-duration 到期导致抽取立刻失败。
	memExtractor.extract(result.Messages)
}

// loopCfg 按 resolved（cli>config）覆盖 flag 默认，构造 LoopConfig。System 空回落默认 prompt。
func loopCfg(resolved *miniagent.Resolved, f *cliFlags, history []miniagent.Message, tools []miniagent.Tool) miniagent.LoopConfig {
	system := resolved.System
	if system == "" {
		system = defaultSystemPrompt
	}
	compModel := resolved.CompactionModelID
	if compModel == "" {
		compModel = resolved.ModelID
	}
	return miniagent.LoopConfig{
		Model:                resolved.ModelID,
		System:               system,
		SummaryRequest:       resolved.SummaryRequest,
		SummarizerPrompt:     resolved.SummarizerPrompt,
		MaxTokens:            into(resolved.Run.MaxTokens, *f.maxTokens),
		Tools:                tools,
		History:              history,
		MaxIterations:        into(resolved.Run.MaxIterations, *f.maxIterations),
		MaxTotalTokens:       into(resolved.Run.MaxTotalTokens, 0),
		Stream:               intoBool(resolved.Run.Stream, *f.stream),
		ContextWindow:        into(resolved.Run.ContextWindow, 0),
		ThinkingLevel:        resolved.Thinking,
		Thinking:             resolved.Provider.Thinking,
		CompactionModel:      compModel,
		MaxToolResultChars:   into(resolved.Run.MaxToolResultChars, 0),
		MaxFileResultChars:   into(resolved.Run.MaxFileResultChars, 0),
		MaxParallelTools:     into(resolved.Run.MaxParallelTools, 0),
		ContextKeepRecent:    into(resolved.Run.ContextKeepRecent, 0),
		SummaryMaxChars:      into(resolved.Run.SummaryMaxChars, 0),
		ContextKeepReasoning: into(resolved.Run.ContextKeepReasoning, 0),
		ContextKeepToolArgs:  into(resolved.Run.ContextKeepToolArgs, 0),
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
