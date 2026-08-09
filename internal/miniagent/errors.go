package miniagent

import "errors"

// ErrBudgetExceeded is returned by the default OnBudget hook (NewDefaultOnBudget) when cumulative
// tokens exceed MaxTotalTokens. The core Run does not build in budget enforcement — that is attached
// via the OnBudget hook; it goes through the error path (CLI exit code 1), and callers may use errors.Is
// to distinguish a circuit-break trip from a real failure.
var ErrBudgetExceeded = errors.New("miniagent: token budget exceeded")

// ErrContextLength is returned by HTTPClient when the endpoint returns a 400 indicating the context
// length was exceeded. The default OnLLMError hook (NewDefaultOnLLMError) uses it to perform a single
// history-tightening retry (see trimHistoryForContext); the core Run does not build in failure recovery —
// that is attached via the OnLLMError hook. Callers may also use errors.Is to detect it.
var ErrContextLength = errors.New("miniagent: context length exceeded")

// ErrThinkingUnsupported is returned by HTTPClient when the endpoint returns a 400 that appears to
// indicate the thinking parameter is unsupported. callLLMWithDowngrade uses it to retry once with the
// thinking field dropped (review v2 #7); a misjudgment is harmless (if the retry still fails it propagates up).
var ErrThinkingUnsupported = errors.New("miniagent: thinking parameter unsupported")

// ErrToolDenied is returned by OnToolUse to indicate the tool was denied execution (e.g. a dangerous
// command that was not confirmed). handleToolCalls uses it to skip that tool (backfilling a denied result)
// without terminating the loop; other errors still terminate.
var ErrToolDenied = errors.New("miniagent: tool denied by caller")
