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
	"github.com/justphantom/miniagent/internal/miniagent/event"
)

// version 由 make build 经 -ldflags "-X main.version=$(git describe --tags)" 注入；
// 不在源码写死字面量，避免发版后滞后。未注入时为空串。
var version string

type cliFlags struct {
	provider      *string
	model         *string
	system        *string
	maxTokens     *int
	workdir       *string
	session       *string
	saveSession   *bool
	logLevel      *string
	showVer       *bool
	maxIterations *int
	stream        *bool
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
	f.provider = flag.String("provider", "", "LLM provider 名（与 -model 成对覆盖 defaults 对；-list-models 时单独用于筛选）")
	f.model = flag.String("model", "", "LLM model id（与 -provider 成对覆盖 defaults 对）")
	f.system = flag.String("system", defaultSystemPrompt, "system prompt")
	f.maxTokens = flag.Int("max-tokens", 4096, "max output tokens per LLM call")
	f.workdir = flag.String("workdir", "", "working directory (default 模式写工具边界 + shell cwd)")
	f.session = flag.String("session", "", "接续已有会话的 id（在 session.dir 解析为 .jsonl；不存在则报错）")
	f.saveSession = flag.Bool("save-session", false, "新建会话并落盘（id 内部生成；与 -session 互斥）")
	f.logLevel = flag.String("log-level", "info", "log level: debug|info|warn|error")
	f.maxIterations = flag.Int("max-iterations", 0, "单轮 LLM 调用上限（0=默认 20）")
	f.stream = flag.Bool("stream", false, "流式输出（SSE）；默认非流式")
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
		// list-models 不要求 -provider/-model（本就为发现模型），故不走 Resolve；
		// 逐行输出 NDJSON {"type":"model","provider","model"}。
		runListModels(ctx, cfg, *f.provider, logger)
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
	sessPath, meta, history := resolveSessionForRun(*f.saveSession, *f.session, sessionDir, modelSpec, resolved.Provider.Name, workdir)
	if *f.saveSession {
		// 新建会话：session 元数据作为 stdout NDJSON 首条事件（与 jsonl 首行同构），供消费方程序化捕获接续 id。
		// 互斥保证 -result-only 下不会触发，不污染 subagent 的纯文本 stdout。
		if err := event.EmitSession(os.Stdout, meta); err != nil {
			fmt.Fprintf(os.Stderr, "miniagent: emit session: %v\n", err)
			os.Exit(1)
		}
	}
	// P0：发现 .miniagent/ 项目规则，合并进 system prompt（persona>rules>defaults）。
	pr := loadProjectRules(workdir)
	resolved.System = mergeSystemPrompt(resolved.System, pr.persona, pr.rules, pr.hasAny())
	resolved.System = injectSubagentGuidance(resolved.System, absConfigPath(*f.configPath), resolved.Mode)

	// 应用运行时配置覆盖（优先级：config>builtin）。
	miniagent.SetMaxReadFileBytes(maxReadFileBytesOf(resolved))
	miniagent.SetMaxShellOutputChars(maxShellOutputCharsOf(resolved))
	miniagent.SetShellStreamWindowBytes(into(resolved.Run.ShellStreamWindowBytes, 0))
	miniagent.SetMaxSessionBytes(maxSessionBytesOf(resolved))

	compaction.SetSummaryMaxTokens(into(resolved.Run.SummaryMaxTokens, 0))
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
	if resolved.CompactionProvider.Name != resolved.Provider.Name {
		compChat, _ = secondaryClient("compaction", resolved.CompactionProvider)
	}

	tools := buildTools(workdir, shellTimeoutOf(resolved), fileOpTimeoutOf(resolved), writeTimeoutOf(resolved), resolved.Mode, into(resolved.Run.MaxFileResultChars, 0), pr.scripts)
	baseCfg := loopCfg(resolved, f, history, tools)
	// §P1-A：工具输出落盘目录——config 显式优先；否则 -save-session/-session 激活时按 session 目录
	// 派生 <sessionDir>/<id>.tool-output/（无 session 且 config 未配则禁用）。
	if resolved.Run.ToolOutputDir != nil && *resolved.Run.ToolOutputDir != "" {
		baseCfg.ToolOutputDir = *resolved.Run.ToolOutputDir
	} else if sessPath != "" {
		baseCfg.ToolOutputDir = filepath.Join(filepath.Dir(sessPath), strings.TrimSuffix(filepath.Base(sessPath), ".jsonl")+".tool-output")
	}
	if resolved.Run.ToolOutputRetention != nil {
		baseCfg.ToolOutputRetention = *resolved.Run.ToolOutputRetention
	}
	// 压缩作为外挂：经 NewCompaction 取 before/after 挂到 hooks（核心 Run 本身不含压缩）。
	compBefore, compAfter := compaction.NewCompaction(compactionOptions(resolved, f, meta, chat, compChat, baseCfg.System, tools, logger))
	hooks := buildHooks(*f.resultOnly)
	hooks.BeforeLLM = compBefore
	hooks.AfterLLM = compAfter
	// 预算作为外挂：经 OnBudget 把 MaxTotalTokens 判定从核心搬出（核心 Run 仅留 OnBudget=nil 时的回落）。
	maxTotal := baseCfg.MaxTotalTokens
	hooks.OnBudget = func(_ int, total miniagent.Usage) error {
		if maxTotal > 0 && total.InputTokens+total.OutputTokens > maxTotal {
			return miniagent.ErrBudgetExceeded
		}
		return nil
	}

	// runCtx 含 -max-duration 超时（若有）；信号处理由 main 顶部的 NotifyContext 提供。
	runCtx := ctx
	if d := maxDurationOf(resolved); d > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(runCtx, d)
		defer cancel()
	}

	prompt := mustReadPrompt(os.Stdin)
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
}

// loopCfg 按 resolved（cli>config）覆盖 flag 默认，构造 LoopConfig（仅核心字段——压缩策略已移出，
// 经 NewCompaction 外挂）。System 空回落默认 prompt。
func loopCfg(resolved *miniagent.Resolved, f *cliFlags, history []miniagent.Message, tools []miniagent.Tool) miniagent.LoopConfig {
	system := resolved.System
	if system == "" {
		system = defaultSystemPrompt
	}
	return miniagent.LoopConfig{
		Model:              resolved.ModelID,
		System:             system,
		SummaryRequest:     resolved.SummaryRequest,
		MaxTokens:          into(resolved.Run.MaxTokens, *f.maxTokens),
		Tools:              tools,
		History:            history,
		MaxIterations:      into(resolved.Run.MaxIterations, *f.maxIterations),
		MaxTotalTokens:     into(resolved.Run.MaxTotalTokens, 0),
		Stream:             intoBool(resolved.Run.Stream, *f.stream),
		ThinkingLevel:      resolved.Thinking,
		Thinking:           resolved.Provider.Thinking,
		MaxToolResultChars: into(resolved.Run.MaxToolResultChars, 0),
		MaxParallelTools:   into(resolved.Run.MaxParallelTools, 0),
	}
}

// compactionOptions 把 resolved 的压缩策略装配成 CompactionOptions。chat 是摘要用 client
// （compChat 非空用之，否则回落主 chat）。
func compactionOptions(resolved *miniagent.Resolved, f *cliFlags, meta miniagent.SessionMeta, chat, compChat *miniagent.ChatClient, system string, tools []miniagent.Tool, logger *slog.Logger) compaction.CompactionOptions {
	compClient := compChat
	if compClient == nil {
		compClient = chat
	}
	return compaction.CompactionOptions{
		Chat:                 compClient,
		MaxTokens:            into(resolved.Run.MaxTokens, *f.maxTokens),
		ContextWindow:        into(resolved.Run.ContextWindow, 0),
		Model:                resolved.ModelID,
		CompactionModel:      resolved.CompactionModelID,
		System:               system,
		Tools:                tools,
		KeepRecent:           into(resolved.Run.ContextKeepRecent, 0),
		KeepReasoning:        into(resolved.Run.ContextKeepReasoning, 0),
		KeepToolArgs:         into(resolved.Run.ContextKeepToolArgs, 0),
		KeepReasoningChars:   into(resolved.Run.ContextKeepReasoningChars, 0),
		SummarizerPrompt:     resolved.SummarizerPrompt,
		SummaryMaxChars:      into(resolved.Run.SummaryMaxChars, 0),
		PreserveRecentTokens: into(resolved.Run.PreserveRecentTokens, 0),
		UseRealUsage:         intoBool(resolved.Run.ContextUseRealUsage, true),
		Auto:                 resolved.CompactionAuto,
		Reserved:             resolved.CompactionReserved,
		SessionID:            meta.ID,
		Logger:               logger,
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
