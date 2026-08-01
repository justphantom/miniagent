package miniagent

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
)

const (
	// summaryMaxChars 是摘要的字符上限（初值，按实测调，设计 §15.4）。
	summaryMaxChars = 2000
	// summaryMaxTokens 限制摘要请求的输出 token 数：端点默认上限远超 summaryMaxChars
	// 对应的 token 量，不限会先生成超长输出再被字符截断，白烧 token（审查 P3-1）。
	summaryMaxTokens = 1024
)

// summarizerSystem 是压缩专用 system prompt：要求把历史压成一段受限摘要，保留关键事实。
const summarizerSystem = "你是会话压缩器。把以下对话历史压缩为一段不超过 %d 字符的中文摘要，保留关键事实、决策、文件改动与未决问题，不要复述全部对话，不要输出多余解释。"

// applyCompactionBarrier 定位最新一条 Kind=="summary" 消息，返回它及之后的消息；之前的
// 旧历史（含更老 summary）不进 context，仍留 session 文件。无 summary 原样返回。
// 归属 compaction.go（审查 v3 #9）；Run 入口统一调用（阶段 1，审查 v3 #3）。
func applyCompactionBarrier(msgs []Message) []Message {
	for i := range slices.Backward(msgs) {
		if msgs[i].Kind == KindSummary {
			return msgs[i:]
		}
	}
	return msgs
}

// compactIfOverWindow 是 Run 阶段 2：超 window 80% 时摘要中段，失败/无中段回落有损压缩，
// 仍超裁到最近轮，再超报错（审查 v2 #8 + v3 #4）。返回 (summarized, summaryUsage, error)：
// summarized=true 表示成功摘要压缩（Run 据此设 result.Compacted）；summaryUsage 是摘要调用的
// token 用量（Run 累加进 total，MaxTotalTokens 预算含摘要调用——审查 P2 摘要 token 不入预算）；
// 返回 error 时 Run 应终止避免循环烧请求。
func compactIfOverWindow(ctx context.Context, llm *HTTPClient, cfg LoopConfig, msgs *[]Message, newMsgs *[]Message, logger *slog.Logger) (bool, Usage, error) {
	if cfg.ContextWindow <= 0 || estimateTokens(*msgs, cfg.System, cfg.Tools) <= cfg.ContextWindow*4/5 {
		return false, Usage{}, nil
	}
	compModel := cfg.CompactionModel
	if compModel == "" {
		compModel = cfg.Model
	}
	summarized, sumUsage, serr := compactWithSummary(ctx, llm, compModel, msgs, contextKeepRecent, newMsgs)
	if serr != nil {
		if logger != nil {
			logger.Warn("summarize 失败，回落有损压缩", "error", serr)
		}
	} else if !summarized && logger != nil {
		logger.Warn("无中段可摘要，有损压缩")
	}
	if !summarized {
		*msgs = compactHistory(*msgs, contextKeepRecent)
	}
	if estimateTokens(*msgs, cfg.System, cfg.Tools) > cfg.ContextWindow*4/5 {
		*msgs = trimRecentRounds(*msgs, contextKeepRecent)
		if logger != nil {
			logger.Warn("仍超 window，裁到最近轮", "msgs", len(*msgs))
		}
	}
	if estimateTokens(*msgs, cfg.System, cfg.Tools) > cfg.ContextWindow*4/5 {
		return summarized, sumUsage, fmt.Errorf("history 超 context window（约 %d tokens）即使有损裁剪后仍超——终止以避免循环烧请求", estimateTokens(*msgs, cfg.System, cfg.Tools))
	}
	return summarized, sumUsage, nil
}

// summarizeMiddle 调 LLM 把中段 msgs 压成一段摘要文本（不带 tools）。返回经 summaryMaxChars
// 截断的摘要 + 该次调用的 Usage（供上游累加入预算）。复用 HTTPClient.Do；调用方据 error 回落
// 有损压缩（审查 v2 #6）。
func summarizeMiddle(ctx context.Context, llm *HTTPClient, model string, msgs []Message) (string, Usage, error) {
	if len(msgs) == 0 {
		return "", Usage{}, errors.New("无中段可摘要")
	}
	resp, err := llm.Do(ctx, Request{
		Model:     model,
		System:    fmt.Sprintf(summarizerSystem, summaryMaxChars),
		Messages:  msgs,
		MaxTokens: summaryMaxTokens,
	})
	if err != nil {
		return "", Usage{}, err
	}
	return truncate(strings.TrimSpace(resp.Text), summaryMaxChars, "…[摘要已截断]"), resp.Usage, nil
}

// compactWithSummary 保留最早 1 轮 + 最近 keepRecent 轮，中段摘要为单条 KindSummary 消息
// （既进 context 又经 newMsgs 落盘）。中段按完整轮切，切完 validateToolPairing 断言；断裂
// 或 LLM 失败返回 error（调用方回落 compactHistory）。无中段可摘返回 (false, Usage{}, nil)。
// 第二返回值是摘要调用的 Usage（供上游累加入预算）。
func compactWithSummary(ctx context.Context, llm *HTTPClient, model string, msgs *[]Message, keepRecent int, newMsgs *[]Message) (bool, Usage, error) {
	rounds := splitRounds(*msgs)
	if len(rounds) <= 1+keepRecent {
		return false, Usage{}, nil // 无中段可摘
	}
	middle := flatten(rounds[1 : len(rounds)-keepRecent])
	if len(middle) == 0 {
		return false, Usage{}, nil
	}
	// 中段必须自洽配对：否则替换进 summary 会留下孤立的 tool_call/tool，续跑被端点 400。
	if err := validateToolPairing(middle); err != nil {
		return false, Usage{}, fmt.Errorf("中段配对断裂，无法安全摘要：%w", err)
	}
	summary, sumUsage, err := summarizeMiddle(ctx, llm, model, middle)
	if err != nil {
		return false, Usage{}, err
	}
	summaryMsg := Message{Role: roleUser, Kind: KindSummary, Content: "[既往对话摘要]\n" + summary}
	out := append([]Message{}, rounds[0]...)
	out = append(out, summaryMsg)
	out = append(out, flatten(rounds[len(rounds)-keepRecent:])...)
	*msgs = out
	// summary 须排在 user_prompt 之前落盘：applyCompactionBarrier 屏障掉最新 summary 之前的
	// 旧历史，若 summary 落在 user_prompt 之后，下一轮 barrier 会把本轮 user_prompt 一并屏障，
	// 使压缩「保留最近 N 轮」的承诺跨轮失效（审查 P1-1）。此时 newMsgs 已含本轮 user_prompt
	// （loop.go Run 入口先加），故 summary 前插而非尾 append。
	//
	// 单轮多次压缩时，上一次写进 newMsgs 的旧 summary 其对应中段已被进一步压进本次新 summary
	// （msgs 重组后旧 summary 落在新 middle 内被覆盖）。若不剔除直接前插，newMsgs 变成
	// [summary_new, summary_old, ...]，applyCompactionBarrier 反向找最后 summary 会命中旧的
	// summary_old，把最新的 summary_new 屏障在外（审查 P2 单轮多次压缩反转）。先剔旧 summary
	// 再前插，保时间序与「barrier 命中最新」不变量。
	filtered := make([]Message, 0, len(*newMsgs)+1)
	for _, m := range *newMsgs {
		if m.Kind != KindSummary {
			filtered = append(filtered, m)
		}
	}
	*newMsgs = append([]Message{summaryMsg}, filtered...)
	return true, sumUsage, nil
}
