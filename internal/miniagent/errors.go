package miniagent

import "errors"

// ErrBudgetExceeded 由默认 OnBudget 钩子（NewDefaultOnBudget）在累计 token 超过 MaxTotalTokens
// 时返回。核心 Run 不内置预算判定——由 OnBudget 钩子外挂；走 error 路径（CLI 退出码 1），
// 调用方可 errors.Is 判定熔断 vs 真故障。
var ErrBudgetExceeded = errors.New("miniagent: token budget exceeded")

// ErrContextLength 由 HTTPClient 在端点返回 context 超限的 400 时返回。默认 OnLLMError 钩子
// （NewDefaultOnLLMError）据此做一次历史收紧重试（见 trimHistoryForContext）；核心 Run 不内置
// 失败恢复——由 OnLLMError 钩子外挂。调用方亦可 errors.Is 判定。
var ErrContextLength = errors.New("miniagent: context length exceeded")

// ErrThinkingUnsupported 由 HTTPClient 在端点返回疑似 thinking 参数不被支持的 400 时返回。
// callLLMWithDowngrade 据此一次性去 thinking 字段重试（审查 v2 #7）；误判无害（重试仍失败则上抛）。
var ErrThinkingUnsupported = errors.New("miniagent: thinking parameter unsupported")

// ErrToolDenied 由 OnToolUse 返回表示拒绝执行该工具（如危险命令未获确认）。
// handleToolCalls 据此跳过该工具（回填拒绝结果）、不终止循环；其他 error 仍终止。
var ErrToolDenied = errors.New("miniagent: tool denied by caller")
