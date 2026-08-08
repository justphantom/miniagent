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
	"github.com/justphantom/miniagent/internal/miniagent/policy"
	"github.com/justphantom/miniagent/internal/miniagent/session"
	"github.com/justphantom/miniagent/internal/provider/openai"
)

// version 由 make build 经 -ldflags "-X main.version=$(git describe --tags)" 注入；
// 不在源码写死字面量，避免发版后滞后。未注入时为空串。
var version string

type cliFlags struct {
	provider      *string
	model         *string
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
	replay        *string
}

func parseFlags() *cliFlags {
	f := &cliFlags{}
	f.configPath = flag.String("config", "", "配置文件路径（默认查 ~/.miniagent/miniagent.json；不存在则报错）")
	f.mode = flag.String("mode", "", "权限模式 default|auto（default 时 workdir 必填）；默认 default")
	f.thinking = flag.String("thinking", "", "思考级别 off|minimal|low|medium|high|xhigh|max（默认 off）")
	f.resultOnly = flag.Bool("result-only", false, "仅输出 result.text（subagent fork 用）；与 -stream 互斥")
	f.provider = flag.String("provider", "", "LLM provider 名（与 -model 成对覆盖 defaults 对；-list-models 时单独用于筛选）")
	f.model = flag.String("model", "", "LLM model id（与 -provider 成对覆盖 defaults 对）")
	f.workdir = flag.String("workdir", "", "working directory (default 模式写工具边界 + shell cwd)")
	f.session = flag.String("session", "", "接续已有会话的 id（在 session.dir 解析为 .jsonl；不存在则报错）")
	f.saveSession = flag.Bool("save-session", false, "新建会话并落盘（id 内部生成；与 -session 互斥）")
	f.replay = flag.String("replay", "", "回放指定会话（读 session 文件重显过程，不调 LLM；与 -save-session/-session/-result-only 互斥）")
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

	// SIGINT 与 SIGTERM 都路由到 ctx 取消 → 退出码 130（128+SIGINT）作通用「信号中断」码，
	// 不区分 POSIX 的 SIGTERM=143：消费方只需识别「被信号打断」而非具体信号，多数工具合并处理。
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if *f.listModels {
		// list-models 不要求 -provider/-model（本就为发现模型），故不走 Resolve；
		// 逐行输出 NDJSON {"type":"model","provider","model"}。
		runListModels(ctx, cfg, *f.provider, logger)
		return
	}

	resolved, err := config.Resolve(cfg, collectOverrides(f))
	if err != nil {
		fmt.Fprintf(os.Stderr, "miniagent: %v\n", err)
		os.Exit(1)
	}
	// -replay：离线回放指定 session，不调 LLM/不需 key/不落盘/不读 stdin。短路位置经取舍：
	// 在 Resolve 后（需 resolved.Session.Dir 解析 session 目录）、在 validateConversation 前
	//（不需 workdir）、在 apiKey 前（不需 key）、在 mustReadPrompt 前（不读 stdin）。
	// sessionDir 三行与下方主路径同逻辑，内联以避免挪动主流程结构。
	if *f.replay != "" {
		if *f.saveSession || *f.session != "" || *f.resultOnly {
			fmt.Fprintln(os.Stderr, "miniagent: -replay 与 -save-session/-session/-result-only 互斥")
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
	sessPath, meta, history := resolveSessionForRun(*f.saveSession, *f.session, sessionDir, modelSpec, resolved.Provider.Name, workdir, int64(maxSessionBytesOf(resolved)))
	if *f.saveSession {
		// 新建会话：session 元数据作为 stdout NDJSON 首条事件（与 jsonl 首行同构），供消费方程序化捕获接续 id。
		// 互斥保证 -result-only 下不会触发，不污染 subagent 的纯文本 stdout。
		// ⚠️ id 在 Run 前 emit，jsonl 直到 Run 成功才落盘；Run 失败/空 stdin 时消费方捕获的 id 磁盘上不存在，须校验退出码。
		if err := event.EmitSession(os.Stdout, meta); err != nil {
			fmt.Fprintf(os.Stderr, "miniagent: emit session: %v\n", err)
			os.Exit(1)
		}
	}
	// P0：发现 .miniagent/ 项目规则，合并进 system prompt（persona>rules>defaults + 默认兜底，见 assembleSystemPrompt）。
	pr := loadProjectRules(workdir)
	resolved.System = assembleSystemPrompt(resolved.System, pr, resolved.SubagentGuidance, absConfigPath(*f.configPath), resolved.Mode)

	// 应用运行时配置覆盖（优先级：config>builtin）。
	limits := miniagent.Limits{
		MaxReadFileBytes:       maxReadFileBytesOf(resolved),
		MaxShellOutputChars:    maxShellOutputCharsOf(resolved),
		ShellStreamWindowBytes: into(resolved.RunConfig.ShellStreamWindowBytes, 0),
		MaxGrepMatches:         into(resolved.RunConfig.GrepMaxMatches, 0),
		MaxSessionBytes:        maxSessionBytesOf(resolved),
		ContextTrimToolChars:   into(resolved.RunConfig.ContextTrimToolChars, 0),
	}

	// 主 provider chat/stream +（跨 provider 时）摘要 compChat；key 缺失或端点非法时 os.Exit。
	chat, stream, compChat := buildRuntimeClients(resolved, apiKey, logger)

	tools := buildTools(workdir, shellTimeoutOf(resolved), fileOpTimeoutOf(resolved), writeTimeoutOf(resolved), resolved.Mode, into(resolved.RunConfig.MaxFileResultChars, 0), limits)
	baseCfg := loopCfg(resolved, f, history, tools)
	// §P1-A：工具输出落盘目录——config 显式优先；否则 -save-session/-session 激活时按 session 目录
	// 派生 <sessionDir>/<id>.tool-output/（无 session 且 config 未配则禁用）。
	if resolved.RunConfig.ToolOutputDir != nil && *resolved.RunConfig.ToolOutputDir != "" {
		baseCfg.ToolOutputDir = *resolved.RunConfig.ToolOutputDir
	} else if sessPath != "" {
		baseCfg.ToolOutputDir = filepath.Join(filepath.Dir(sessPath), strings.TrimSuffix(filepath.Base(sessPath), ".jsonl")+".tool-output")
	}
	if resolved.Run.ToolOutputRetention != nil {
		baseCfg.ToolOutputRetention = *resolved.Run.ToolOutputRetention
	}
	// 压缩作为外挂：经 NewCompaction 取 before/after；三项默认策略（OnLLMError/OnBudget/ShapeToolResult）经 assembleHooks 外挂。
	compBefore, compAfter := compaction.NewCompaction(compactionOptions(resolved, meta, chat, compChat, baseCfg.System, tools, logger))
	hooks := assembleHooks(compBefore, compAfter, *f.resultOnly, baseCfg, tools, limits, logger)

	prompt := mustReadPrompt(ctx, os.Stdin)
	// runCtx 含 -max-duration 超时（若有）；在 stdin 读取之后构造——mustReadPrompt 用 signal ctx 不受限，
	// 若 runCtx 在前则慢 stdin 会消耗 max-duration 预算致 Run 拿到已过期 ctx（DeadlineExceeded exit 1）。
	runCtx := ctx
	if d := maxDurationOf(resolved); d > 0 {
		var cancel context.CancelFunc
		runCtx, cancel = context.WithTimeout(runCtx, d)
		defer cancel()
	}
	llm := &openai.Provider{Chat: chat, Stream: stream}
	result, err := miniagent.Run(runCtx, llm, baseCfg, string(prompt), hooks, logger)

	// saveSession 救回本轮已执行的部分供 resume：Run 经 defer 保证 result.Messages/NewMessages 在
	// 出错/取消路径亦带回，且 tool_call↔tool_result 配对由 fillPlaceholderTail 补全完整——故不只
	// 成功路径落盘，出错/取消亦调用，消除「工具已执行、jsonl 未追加」的孤儿不一致（如 tool-output
	// 残留而 jsonl 停在上一轮）。保存期间忽略 SIGINT/SIGTERM：避免截断 session 文件或残留临时文件。
	// 返回 saveErr 由调用方裁决 exit code：成功路径失败仍 exit 1（原语义）；出错/取消路径仅 warn 不改码。
	saveSession := func() (saveErr error) {
		if sessPath == "" || len(result.NewMessages) == 0 {
			return nil
		}
		signal.Ignore(syscall.SIGINT, syscall.SIGTERM)
		if result.Compacted {
			saveErr = session.RewriteMessages(sessPath, meta, result.Messages, int64(limits.MaxSessionBytes))
		} else {
			saveErr = session.AppendMessages(sessPath, meta, result.NewMessages, int64(limits.MaxSessionBytes))
		}
		signal.Reset(syscall.SIGINT, syscall.SIGTERM)
		return saveErr
	}

	if err != nil {
		// 信号取消（SIGINT/SIGTERM）走码 130 干净退出，不 emit error（审查 P3 SIGINT 退出码）。
		// 取消亦落盘：配对完整，救回已执行部分供下次续聊。
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

// buildRuntimeClients 构造主 provider 的 chat/stream client 与（compaction 跨 provider 时）摘要用 compChat。
// compChat 与主 provider 同名时留 nil（loop 回落主 chat）。key 缺失或端点非法时 os.Exit（原 secondaryClient 闭包仅 compaction 一处用，内联）。
func buildRuntimeClients(resolved *config.Resolved, apiKey string, logger *slog.Logger) (chat *openai.ChatClient, stream *openai.StreamClient, compChat *openai.ChatClient) {
	chat, stream = buildLLM(apiKey, resolved.Provider, logger, httpTimeoutOf(resolved))
	if resolved.CompactionProvider.Name != resolved.Provider.Name {
		key := resolveFinalKey(resolved.CompactionProvider.Key)
		if key == "" {
			fmt.Fprintf(os.Stderr, "miniagent: compaction provider API key 缺失（provider.key / $MINIAGENT_API_KEY）\n")
			os.Exit(1)
		}
		warnProviderInsecureURLs(resolved.CompactionProvider)
		compChat = buildChatClient(key, resolved.CompactionProvider, logger, httpTimeoutOf(resolved))
	}
	return chat, stream, compChat
}

// assembleHooks 组装 LoopHooks：事件输出（buildHooks）+ 压缩 before/after + 三项默认策略外挂
// （OnLLMError 历史收紧重试 / OnBudget 估算+熔断 / ShapeToolResult 截断+落盘）。核心 Run 零策略，本函数装配即恢复完整能力。
func assembleHooks(
	compBefore func(context.Context, miniagent.StepInput) (miniagent.StepOutput, error),
	compAfter func(context.Context, int, miniagent.Response) error,
	resultOnly bool, baseCfg miniagent.LoopConfig, tools []miniagent.Tool, limits miniagent.Limits, logger *slog.Logger,
) miniagent.LoopHooks {
	hooks := buildHooks(resultOnly)
	hooks.BeforeLLM = compBefore
	hooks.AfterLLM = compAfter
	hooks.OnLLMError = policy.NewDefaultOnLLMError(logger, limits.ContextTrimToolChars)
	hooks.OnBudget = policy.NewDefaultOnBudget(baseCfg.MaxTotalTokens, logger)
	hooks.ShapeToolResult = policy.NewDefaultShapeToolResult(tools, baseCfg.ToolOutputDir, baseCfg.ToolOutputRetention, baseCfg.MaxToolResultChars, logger)
	return hooks
}

// loopCfg 按 resolved（cli>config）覆盖 flag 默认，构造 LoopConfig（循环本体 + 策略载体字段；
// 压缩策略经 NewCompaction 外挂，其余策略经 NewDefault* 钩子工厂外挂，核心 Run 零策略）。
// 生产路径下 resolved.System 经 assembleSystemPrompt（main.go:126）保证非空，下面的空串兜底仅对
// 直接构造 loopCfg 的测试有意义（防漏传 System 致空 prompt）。
func loopCfg(resolved *config.Resolved, f *cliFlags, history []miniagent.Message, tools []miniagent.Tool) miniagent.LoopConfig {
	system := resolved.System
	if system == "" {
		system = defaultSystemPrompt
	}
	return miniagent.LoopConfig{
		Model:              resolved.ModelID,
		System:             system,
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

// compactionOptions 把 resolved 的压缩策略装配成 CompactionOptions。chat 是摘要用 client
// （compChat 非空用之，否则回落主 chat）。
func compactionOptions(resolved *config.Resolved, meta session.SessionMeta, chat, compChat *openai.ChatClient, system string, tools []miniagent.Tool, logger *slog.Logger) compaction.CompactionOptions {
	compClient := compChat
	if compClient == nil {
		compClient = chat
	}
	return compaction.CompactionOptions{
		Chat:                     compClient,
		MaxTokens:                into(resolved.MaxTokens, 0),
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

// into 解析 *int 覆盖：ov 非 nil 用 *ov，否则用 def。loopCfg 与 main 的 buildTools 调用共用。
func into(ov *int, def int) int {
	if ov != nil {
		return *ov
	}
	return def
}
