package miniagent

import (
	"context"
	"time"
)

// MaxToolResultInHistory is the built-in per-tool-result character cap — the final fallback when Tool.ResultLimit and
// LoopConfig.MaxToolResultChars are both <=0 (TrimForHistory applies it). Lives in core so the built-in tools (grep/glob/shell)
// can reference it without importing the policy package (kills the tools→policy edge for this constant).
const MaxToolResultInHistory = 4000

// Tool is one agent tool the LLM may call. Name/Description/Parameters
// are the three elements of the OpenAI function-calling schema; serialization is done by wire.go.
type Tool struct {
	Name        string
	Description string
	Parameters  map[string]any
	// ResultLimit is the character cap for this tool's result entering the history (<=0 uses the maxToolResultInHistory
	// default). Code-oriented tools like read/edit set a high limit to avoid losing accuracy to truncation. Not part of the tool schema serialization.
	ResultLimit int
	// SplitTruncate, when true, splits the tool result into "head N/4 + tail 3N/4" truncation (joined by an ellipsis marker),
	// instead of the default head-only. shell/grep/script-style tools often have key conclusions at the tail (compile/test error
	// summaries, hit-limit notices); pure head truncation would drop the diagnostic information the model most needs to see, forcing a
	// re-run (which burns more tokens). Code-oriented tools with line numbers like read/edit keep head-only (false): head truncation
	// matches the "paginate reading large files" semantics. Not part of the tool schema serialization (like ResultLimit, it is a
	// history-inclusion strategy, not an LLM-visible parameter).
	SplitTruncate bool
	// Call is the tool's actual execution function. Implementations must respect ctx: on ctx cancellation it must return promptly,
	// otherwise runToolsParallel's wg.Wait would hang and Run would not respond to SIGINT (Go cannot forcibly terminate a goroutine;
	// the core does not bake in a timeout — each tool implements its own, e.g. shell's shellTimeout). json:"-" prevents an accidental
	// json.Marshal(Tool) from reporting unsupported type.
	Call func(ctx context.Context, args string) ToolResult `json:"-"`
}

// ExitCodeNotSet marks that a shell command produced no valid exit code (timeout or launch failure), distinguishing it from
// a normal exit 0 (success) / N (command exit code), so consumers can recognize "the command didn't really finish running".
const ExitCodeNotSet = -1

type ToolResult struct {
	Output  string
	IsError bool
	// ExitCode is only meaningful for the shell tool; non-shell tools do not set it (zero value 0), and the event layer decides
	// whether to emit this field based on the tool name (see A4 tool_result event).
	ExitCode int
}

// OnToolUse is the pre-tool-execution callback: name is the tool name, input is the raw JSON arguments.
// Returning an error propagates up the call chain to Run — when the downstream is unwritable (the stdout pipe was closed early by
// the consumer) it immediately terminates the loop to avoid burning more tokens. Passing nil means no notification.
type OnToolUse func(name, input string) error

// DeltaKind identifies the kind of LLM streaming delta, carried by the OnDelta callback (once streaming mode is enabled).
type DeltaKind string

const (
	DeltaText      DeltaKind = "text"
	DeltaReasoning DeltaKind = "reasoning"
)

// LoopHooks is the set of hooks Run uses to call back the consumer at various points in the loop; all fields may be nil
// (no notification; minimal mode). This is the open seam of the agent core: BeforeLLM/AfterLLM (context view/observation),
// OnBudget (usage accounting + budget judgment), OnLLMError (failure recovery), ShapeToolResult (tool result shaping) let all
// context/usage/shaping strategies be implemented as plugins, while the core loop itself does no context management, estimation,
// budget judgment, or error recovery — it only does tool registration / context assembly / call LLM / execute tools / exit loop.
// Default hook implementations are in the NewDefault* factories (the cmd layer assembles them to reuse the original built-in behavior).
type LoopHooks struct {
	// BeforeLLM triggers before each LLM call in a step: it can rewrite the message view sent to the LLM, commit persistence
	// deltas, accumulate usage, and flag compaction. nil=pass-through (minimal mode: the core does no context management and sends
	// the transcript as-is). This is the sole seam for context management / compaction / memory / RAG injection.
	BeforeLLM func(ctx context.Context, in StepInput) (StepOutput, error)
	// AfterLLM triggers after each step's LLM response, for observing usage, detecting silent overflow, and accounting. nil=no notification.
	AfterLLM func(ctx context.Context, step int, resp Response) error
	// OnBudget triggers after each step's LLM response: the core has already accumulated the real usage into total, and this hook
	// is responsible for the local estimation fallback when usage is zero (accumulating into total) and the budget judgment. Returning
	// an error (typically ErrBudgetExceeded) → the core stops the loop (the error propagates directly, taking the circuit-break exit code).
	// nil=the core neither estimates nor judges (minimal mode: only accumulates real usage, no circuit-break). The budget becomes a
	// swappable strategy: the caller plugs in custom budget/circuit-break logic via this hook; the core bakes in no specific budget strategy.
	// The default implementation is in NewDefaultOnBudget (carrying the original EstimateTokens fallback + MaxTotalTokens judgment).
	OnBudget func(ctx context.Context, step int, in BudgetInput, total *Usage) error
	// OnLLMError triggers after a single step's LLM call fails, for plugging in error recovery (typically: on ErrContextLength, tighten
	// history and retry once). Returning recoveredMsgs non-nil → the core replaces the running transcript with it and retries this call;
	// retry=false or a nil hook → the core does no recovery and the error propagates directly to terminate the loop; a non-nil returned err
	// likewise propagates. Retry happens only once (the core does not recurse). This is the sole seam for the LLM failure path
	// (BeforeLLM/AfterLLM are both on the success path). The default implementation is in NewDefaultOnLLMError.
	OnLLMError func(ctx context.Context, step int, msgs []Message, callErr error) (recoveredMsgs []Message, retry bool, retErr error)
	// OnToolUse notifies before tool execution; a returned error propagates up the chain to Run to terminate the loop (when the
	// downstream pipe is closed). Returning the sentinel ErrToolDenied (defined in errors.go) only denies that tool, without terminating the loop.
	OnToolUse func(name, input string) error
	// OnToolResult notifies after tool execution, passing through the ToolResult (including ExitCode / IsError). Multiple tool_calls within
	// the same step execute in parallel; this callback notifies serially in tool_call order after all complete — it is not "notify as each tool
	// completes", so real-time responsiveness is bounded by the slowest tool.
	OnToolResult func(name, callID string, r ToolResult) error
	// ShapeToolResult triggers after tool execution, before the result enters history, returning the content of that tool message.
	// Returning an empty string = the core passes through the original Output (zero shaping); returning a non-empty string = the core
	// uses it to override content. A returned error propagates up the chain to terminate the loop (when the downstream pipe is closed), and
	// the core fills placeholder tool messages for the remaining calls via the same path as OnToolResult to preserve pairing. Only content is
	// changed; role/tool_call_id cannot be changed — the pairing invariant is guaranteed by the core. nil=the core passes through the original
	// text (minimal mode); the default shaping (trimForHistory truncation + optional persist-to-disk) is carried by NewDefaultShapeToolResult
	// and mounted during cmd-layer assembly; the core bakes in no specific shaping strategy.
	ShapeToolResult func(name, callID string, step int, r ToolResult) (string, error)
	// OnDelta is the LLM streaming delta; not triggered in non-streaming mode. A returned error aborts the stream — it propagates via
	// callLLMOnce up to Run to terminate the loop (not ErrThinkingUnsupported, so no downgrade is triggered). Use this to terminate early
	// when the downstream pipe is closed, avoiding burning more tokens.
	OnDelta func(step int, kind DeltaKind, text string) error
	// OnStep is the per-step observability seam: it fires once at the top of each loop iteration (BEFORE BeforeLLM/LLM/tools), so it
	// covers every exit path (success/error/summary/maxIter — it precedes the branching). nil=no notification (minimal mode, zero
	// overhead). Use it to export long-session metrics (transcript growth, token-spend slope, compaction count) — the snapshot is built
	// from state already in hand, no extra scans. It is observe-only (no error return): the default emitter is best-effort and never
	// terminates the loop. The default consumer is metrics.NewStepEmitter (NDJSON to a writer), wired via the cmd layer.
	OnStep func(ctx context.Context, snap StepSnapshot)
}

// StepInput is BeforeLLM's input: the current running transcript (read-only intent) + step + request-level System/Tools.
// BeforeLLM uses this to decide the message view sent to the LLM this round (may compact, inject, or pass through as-is).
//
// Msgs shares the backing array with the running transcript (only the slice header is copied); the hook must be read-only — mutating
// Msgs[i] fields in place would corrupt Run's transcript. Rewrites should be returned via StepOutput.View/Persist and folded back
// into state by the core.
type StepInput struct {
	Step   int
	Msgs   []Message
	System string
	Tools  []Tool
}

// StepOutput is BeforeLLM's return value. View is required (the messages sent to the LLM); the rest is optional, as core side effects.
type StepOutput struct {
	// View is the messages sent to the LLM this round (required; on pass-through = input Msgs).
	View []Message
	// Commit=true causes the core to replace the running transcript with View (compaction scenario: the shrunk result becomes the new
	// transcript); false means the core keeps the original transcript and only sends View this round (memory/RAG injection scenario:
	// injection does not enter the transcript).
	Commit bool
	// Persist is additionally appended to this round's persistence delta (e.g. the summary message generated by compaction).
	// Persisted messages with a Kind replace older entries of the same Kind in newMsgs (e.g. multiple compactions keep only the latest summary).
	Persist []Message
	// ExtraUsage is accumulated into this round's Result.Usage (e.g. the tokens of the summary LLM call); takes effect when non-nil.
	ExtraUsage *Usage
	// Compacted=true flags that compaction occurred this round; the core sets Result.Compacted accordingly (the interaction layer rewrites the session based on this).
	Compacted bool
}

// StepSnapshot is the per-step observability sample carried by the OnStep hook: a point-in-time view of loop state at the
// iteration top, BEFORE that step's LLM/tool work. Every field is drawn from state already in hand (no extra scans, no STW
// memstats by default), so the seam is cheap to fire each step. nil OnStep = zero overhead (minimal mode).
type StepSnapshot struct {
	Step          int  // the step about to run (1-based)
	TranscriptLen int  // len(msgs): running transcript length (history + this turn's additions so far)
	InputTokens   int  // total.InputTokens accumulated so far this turn
	OutputTokens  int  // total.OutputTokens accumulated so far this turn
	Compacted     bool // whether compaction has fired at least once this turn
	LLMRequests   int  // LLM requests sent so far this turn (downgrade/retry/summary inclusive)
	NewMessages   int  // len(newMsgs): this turn's persisted additions so far
}

// BudgetInput is OnBudget's request-side context: the message view sent to the LLM this round + request-level System/Tools + response.
// Lets the hook do the local estimation fallback when usage is zero (EstimateTokens computes toSend/system/tools; estimateResponseTokens computes resp).
type BudgetInput struct {
	ToSend []Message
	System string
	Tools  []Tool
	Resp   Response
}

type Result struct {
	Text  string
	Usage Usage
	// Steps is the number of LLM calls whose usage has been accounted for this round (usage is accumulated in recordStepUsage: unrecorded
	// failure paths count step-1, recorded ones count step; the extra summary call counts step+1). Error/cancel paths share this semantics.
	// Observation-only, not a logic dependency.
	Steps int
	// LLMRequests is the number of requests actually sent to the LLM endpoint this round (including thinking downgrade retry,
	// OnLLMError tighten retry, and the summary step), excluding provider-layer transparent retries (503 backoff) and the standalone LLM
	// call of the compaction step. Unlike Steps: Steps only counts steps whose usage is accounted for, while LLMRequests counts actual
	// HTTP requests (downgrade/retry often makes the two unequal).
	LLMRequests int
	// Finish is the termination reason: FinishStop (the model produced final text) or
	// FinishMaxIterations (hit the iteration limit, Text is empty). Empty string when returning on error.
	Finish string
	// Messages is the full transcript as of return (History + new additions this round);
	// all return paths (including error, hit maxIterations) bring it back for session persistence.
	Messages []Message
	// NewMessages is the messages added by this Run (excluding History): main appends them append-only to the session jsonl,
	// avoiding rewriting the full set each time. Run guarantees via defer that error/cancel paths also bring back NewMessages, and
	// tool_call↔tool_result pairing is completed by handleToolCalls' fillPlaceholderTail — main also calls saveSession on error/cancel
	// paths to persist the partially executed work before the failure for resume (only the failed step's LLM produced no output and is
	// naturally excluded).
	NewMessages []Message
	// Compacted flags whether summary compaction was triggered this round (the BeforeLLM hook sets StepOutput.Compacted, and the core
	// back-fills from it). The interaction/entry layer decides whether to rewrite the session file based on this — the append-only persisted
	// newMsgs contains the barriered old summary and the compacted middle, and long sessions need opportunistic rewrite to truly discard
	// them (review P2 session files are never compacted).
	Compacted bool
	// ThinkingDowngraded flags whether callLLMWithDowngrade experienced a thinking downgrade this round. The interaction layer clears
	// baseCfg.ThinkingLevel based on this, avoiding re-sending the original value next round and hitting 400 again (review P2 thinking
	// hardening across rounds).
	ThinkingDowngraded bool
}

const (
	FinishStop          = "stop"
	FinishMaxIterations = "max_iterations"
)

// LoopConfig is Run's configuration. Fields fall into two categories: loop-body fields (Model/System/Tools/History/MaxIterations/
// MaxTokens/Stream/Thinking…) read directly by the core Run; strategy fields (MaxTotalTokens/MaxToolResultChars/
// ToolOutputDir/ToolOutputRetention) are not read by the core Run — they are just config carriers extracted by the caller and fed to
// the NewDefault* hook factories (see cmd/miniagent). All context/usage/shaping strategies are plugged in via LoopHooks; Run is a
// minimal, strategy-free ReAct core.
type LoopConfig struct {
	Model          string
	System         string
	SummaryRequest string // The summary request injected when hitting the iteration limit (an internal bootstrap message, not persisted); empty uses the built-in default.
	MaxTokens      int
	Tools          []Tool
	// History is the session history before this round, concatenated in order before the new user prompt. Run does not modify its contents.
	History []Message
	// MaxIterations overrides the per-round LLM call limit; <=0 uses the maxIterations default.
	MaxIterations int
	// MaxTotalTokens is the per-round cumulative token (input+output) limit; <=0 means unlimited. Strategy config carrier — not read by
	// the core Run; NewDefaultOnBudget uses it to judge circuit-break (exceeding returns ErrBudgetExceeded, going through the error event + exit code 1).
	MaxTotalTokens int
	// Stream, when true, makes callLLMOnce use streaming (DoStream); deltas are pushed out via LoopHooks.OnDelta;
	// default false (non-streaming Do).
	Stream bool
	// ThinkingLevel / Thinking are passed through to each callLLMOnce Request (thinking level + provider mapping).
	ThinkingLevel string
	Thinking      *ThinkingMapping
	// MaxToolResultChars is the default character cap for a tool result entering history (backs Tool.ResultLimit; <=0 uses the built-in default).
	// Strategy config carrier — not read by the core Run; read by NewDefaultShapeToolResult.
	MaxToolResultChars int
	// MaxParallelTools is the per-step parallel tool limit (<=0 uses the built-in default).
	MaxParallelTools int
	// ToolOutputDir, when non-empty, enables "tool output persist-to-disk + path read-back": tools exceeding the limit write the full
	// text to <ToolOutputDir>/tool_<step>_<callID>_<n>.txt, and the history Content is replaced with a preview + absolute path hint.
	// Empty=disabled (trimForHistory truncation only). Strategy config carrier — not read by the core Run; read by NewDefaultShapeToolResult.
	ToolOutputDir string
	// ToolOutputRetention is the retention duration for persisted files; NewDefaultShapeToolResult opportunistically cleans up earlier
	// files during construction. <=0 uses 7d. Strategy config carrier — not read by the core Run.
	ToolOutputRetention time.Duration
}

// Permission modes (review v3 requirement §4): default=thin soft constraint (write tools limited to workdir, shell rejects sudo/su),
// auto=unrestricted. default does not constitute a security boundary (shell can cd/absolute-path escape, write tools can escape via symlinks).
const (
	ModeDefault = "default"
	ModeAuto    = "auto"
)
