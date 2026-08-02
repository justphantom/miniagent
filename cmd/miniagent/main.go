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
	"time"

	"github.com/justphantom/miniagent/internal/miniagent"
)

var version = "dev"

type cliFlags struct {
	model           *string
	keyFile         *string
	system          *string
	summaryRequest  *string
	summarizerPrompt *string
	maxTokens       *int
	maxDuration     *time.Duration
	workdir         *string
	session         *string
	logLevel        *string
	showVer         *bool
	maxIterations   *int
	shellTimeout    *time.Duration
	maxTokensTotal  *int
	stream          *bool
	contextWindow   *int
	interactive     *bool
	listModels      *bool
	migrateSession  *string
	// v3 新增
	configPath *string
	chatURL    *string
	modelsURL  *string
	mode       *string
	thinking   *string
	resultOnly *bool
}

func parseFlags() *cliFlags {
	f := &cliFlags{}
	f.configPath = flag.String("config", "", "配置文件路径（默认查 ./miniagent.json；不存在软失败退裸模式）")
	f.chatURL = flag.String("chat-url", "", "裸模式 chat completions 完整 URL（替代 -base-url）")
	f.modelsURL = flag.String("models-url", "", "裸模式 models 完整 URL（可选）")
	f.mode = flag.String("mode", "", "权限模式 default|auto（default 时 workdir 必填）；默认 default")
	f.thinking = flag.String("thinking", "", "思考级别 off|minimal|low|medium|high|xhigh|max（默认 off）")
	f.resultOnly = flag.Bool("result-only", false, "仅输出 result.text（subagent fork 用）；与 -stream 互斥")
	f.model = flag.String("model", "", "LLM model（config 模式 provider/id；裸模式裸 id）")
	f.keyFile = flag.String("key-file", "", "从文件读 API key（首尾空白截断）；优先于 provider.key/$MINIAGENT_API_KEY")
	f.system = flag.String("system", defaultSystemPrompt, "system prompt")
	f.summaryRequest = flag.String("summary-request", "", "迭代上限时注入的总结引导 prompt（空=回落内置默认）")
	f.summarizerPrompt = flag.String("summarizer-prompt", "", "摘要压缩专用 system prompt（空=回落内置默认）")
	f.maxTokens = flag.Int("max-tokens", 4096, "max output tokens per LLM call")
	f.maxDuration = flag.Duration("max-duration", 0, "overall wall-clock limit (0 = unlimited)")
	f.workdir = flag.String("workdir", "", "working directory (default 模式写工具边界 + shell cwd)")
	f.session = flag.String("session", "", "session id 或路径（id 在 session.dir 解析为 .jsonl）")
	f.logLevel = flag.String("log-level", "info", "log level: debug|info|warn|error")
	f.maxIterations = flag.Int("max-iterations", 0, "单轮 LLM 调用上限（0=默认 20）")
	f.shellTimeout = flag.Duration("shell-timeout", 0, "单条 shell 命令超时（0=默认 60s）")
	f.maxTokensTotal = flag.Int("max-tokens-total", 0, "单轮累计 token 上限（0=不限）")
	f.stream = flag.Bool("stream", false, "流式输出（SSE）；默认非流式")
	f.contextWindow = flag.Int("context-window", 0, "模型 context 上限（tokens）；>0 主动压缩历史")
	f.interactive = flag.Bool("interactive", false, "交互模式：循环读取 prompt（每行一个）")
	f.listModels = flag.Bool("list-models", false, "列出端点可用模型 id 后退出")
	f.migrateSession = flag.String("migrate-session", "", "把 v2 JSON 会话转为 jsonl 后退出（旧 .json 路径）")
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

	if *f.migrateSession != "" {
		runMigrate(*f.migrateSession, defaultSessionDir)
		return
	}

	cfg, err := loadConfigOrBare(*f.configPath)
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
		llm := buildLLM(resolveFinalKey(p.Key, *f.keyFile), p, logger)
		ids, err := miniagent.ListAvailableModels(context.Background(), llm, p)
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
	modelSpec := resolved.Provider.Name + "/" + resolved.ModelID
	sessPath, meta, history := resolveSessionForRun(*f.session, sessionDir, modelSpec, effectiveWorkdir(resolved, f))
	resolved.System = injectSubagentGuidance(resolved.System, absConfigPath(*f.configPath, cfg), meta.ID, resolved.Mode)

	workdir := effectiveWorkdir(resolved, f)
	llm := buildLLM(apiKey, resolved.Provider, logger)
	tools := buildTools(workdir, shellTimeoutOf(resolved, f), resolved.Mode)
	reader := bufio.NewReader(os.Stdin)
	hooks := buildHooks(*f.resultOnly)
	baseCfg := loopCfg(resolved, f, history, tools)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if d := maxDurationOf(resolved, f); d > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}

	if *f.interactive {
		os.Exit(runInteractive(ctx, llm, baseCfg, sessPath, meta, hooks, logger, reader))
	}

	prompt := mustReadPrompt(reader)
	result, err := miniagent.Run(ctx, llm, baseCfg, string(prompt), hooks, logger)
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
	into := func(ov *int, def int) int {
		if ov != nil {
			return *ov
		}
		return def
	}
	system := resolved.System
	if system == "" {
		system = defaultSystemPrompt
	}
	compModel := resolved.Compaction.Model
	if compModel == "" {
		compModel = resolved.ModelID
	}
	return miniagent.LoopConfig{
		Model:            resolved.ModelID,
		System:           system,
		SummaryRequest:   resolved.SummaryRequest,
		SummarizerPrompt: resolved.SummarizerPrompt,
		MaxTokens:        into(resolved.Run.MaxTokens, *f.maxTokens),
		Tools:            tools,
		History:          history,
		MaxIterations:    into(resolved.Run.MaxIterations, *f.maxIterations),
		MaxTotalTokens:   into(resolved.Run.MaxTotalTokens, *f.maxTokensTotal),
		Stream:           intoBool(resolved.Run.Stream, *f.stream),
		ContextWindow:    into(resolved.Run.ContextWindow, *f.contextWindow),
		ThinkingLevel:    resolved.Thinking,
		Thinking:         resolved.Provider.Thinking,
		CompactionModel:  compModel,
	}
}

func intoBool(ov *bool, def bool) bool {
	if ov != nil {
		return *ov
	}
	return def
}
