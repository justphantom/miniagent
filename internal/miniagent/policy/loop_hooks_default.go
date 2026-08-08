package policy

import (
	miniagent "github.com/justphantom/miniagent/internal/miniagent"
	"context"
	"errors"
	"log/slog"
	"time"
	"unicode/utf8"
)

// 本文件承载 Run 核心原内置的三项策略的默认外挂实现，供 cmd 层组装 LoopHooks 时复用，
// 使「核心零策略」与「开箱即用的默认行为」兼得：核心循环不读策略、不做估算/预算/落盘/错误恢复；
// 调用方经这些工厂一行装配即可恢复原内置语义。

// NewDefaultOnLLMError 返回承载 miniagent.ErrContextLength 收紧重试的默认 OnLLMError 钩子。
// 仅对 miniagent.ErrContextLength 触发：trimHistoryForContext（清 reasoning + 压 tool content）收紧后 retry=true，
// 核心据此重试一次本次调用；其他 error 透传（retry=false），核心照常上抛。logger 仅用于收紧告警。
func NewDefaultOnLLMError(logger *slog.Logger, contextTrim int) func(context.Context, int, []miniagent.Message, error) ([]miniagent.Message, bool, error) {
	return func(_ context.Context, _ int, msgs []miniagent.Message, err error) ([]miniagent.Message, bool, error) {
		if !errors.Is(err, miniagent.ErrContextLength) {
			return nil, false, nil
		}
		if logger != nil {
			logger.Warn("context length exceeded; trimmed history; retrying step")
		}
		return trimHistoryForContext(msgs, contextTrim), true, nil
	}
}

// NewDefaultOnBudget 返回承载零 usage 本地估算 fallback + MaxTotalTokens 预算判定的默认 OnBudget 钩子。
// 核心已把真实 usage 累加进 total；本钩子在 resp.Usage 全零（流式端点常不 honor include_usage / 非流式缺失）
// 时补本地估算（EstimateTokens 计请求侧 toSend+system+tools，estimateResponseTokens 计响应侧），
// 再按 maxTotalTokens 判定熔断。maxTotalTokens<=0 不判定。
func NewDefaultOnBudget(maxTotalTokens int, logger *slog.Logger) func(context.Context, int, miniagent.BudgetInput, *miniagent.Usage) error {
	return func(_ context.Context, step int, in miniagent.BudgetInput, total *miniagent.Usage) error {
		// 零 usage fallback：逐侧估算缺失的 token（R4-7）。原 && 仅在全零时估，半零 usage（如只回 prompt_tokens）
		// 的缺失侧按 0 累计致预算系统性低估；现按侧独立估算。
		inZero := in.Resp.Usage.InputTokens == 0
		outZero := in.Resp.Usage.OutputTokens == 0
		if inZero || outZero {
			if logger != nil {
				logger.Warn("llm returned partial/zero usage; estimating missing side(s) locally", "step", step)
			}
			if inZero {
				total.InputTokens += EstimateTokens(in.ToSend, in.System, in.Tools)
			}
			if outZero {
				total.OutputTokens += estimateResponseTokens(in.Resp)
			}
		}
		if maxTotalTokens > 0 && total.InputTokens+total.OutputTokens > maxTotalTokens {
			return miniagent.ErrBudgetExceeded
		}
		return nil
	}
}

// NewDefaultShapeToolResult 返回承载原 defaultShapeResult 语义的默认 ShapeToolResult 钩子：
// trimForHistory 截断（miniagent.Tool.ResultLimit/SplitTruncate，回落 maxToolResultChars→MaxToolResultInHistory）
// + 可选落盘（dir 非空时超 limit 的全文写盘，Content 改 preview+路径提示）。dir 空=禁用落盘（仅截断）。
// 构造时建一次 store 并 cleanup 过期文件（贴合单次运行 CLI 形态）。tools 用于取各工具的 ResultLimit/SplitTruncate。
func NewDefaultShapeToolResult(tools []miniagent.Tool, dir string, retention time.Duration, maxToolResultChars int, logger *slog.Logger) func(string, string, int, miniagent.ToolResult) (string, error) {
	toolByName := make(map[string]miniagent.Tool, len(tools))
	for _, t := range tools {
		toolByName[t.Name] = t
	}
	var store *toolOutputStore
	if dir != "" {
		store = newToolOutputStore(dir, retention, logger)
		store.cleanup()
	}
	return func(name, callID string, step int, tres miniagent.ToolResult) (string, error) {
		limit := 0
		split := false
		if t, ok := toolByName[name]; ok {
			limit = t.ResultLimit
			split = t.SplitTruncate
		}
		// 工具未声明 ResultLimit 时回落 maxToolResultChars；trimForHistory 仍有 <=0→MaxToolResultInHistory
		// 的最终兜底，双保险。split 对 shell/grep 类工具走头尾分段截断。
		if limit <= 0 {
			limit = maxToolResultChars
		}
		preview := TrimForHistory(tres.Output, limit, split)
		content := preview
		if store != nil {
			// §P1-A：超 limit 的全文落盘，入历史 Content 改为 preview+路径提示。truncated 判定用
			// effective limit 的精确比较（trimForHistory 内部对 limit<=0 回落 MaxToolResultInHistory，
			// 此处复刻同一解析；截断 iff rune 数 > effective limit），避免 rune 长度差法在「输出刚好超 limit、
			// preview marker 反而更长」时的假阴性（丢全文不落盘）。
			effLimit := limit
			if effLimit <= 0 {
				effLimit = MaxToolResultInHistory
			}
			content = store.bound(step, callID, tres.Output, preview, utf8.RuneCountInString(tres.Output) > effLimit)
		}
		return content, nil
	}
}
