// split.go：中段切分与有损压缩。

package compaction

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/justphantom/miniagent/internal/miniagent"
	"github.com/justphantom/miniagent/internal/miniagent/session"
	"github.com/justphantom/miniagent/internal/text"
)

// applyCompactionBarrier 定位最新一条 Kind=="summary" 消息，返回它及之后的消息；之前的
// 旧历史（含更老 summary）不进 context，仍留 session 文件。无 summary 原样返回。
func applyCompactionBarrier(msgs []miniagent.Message) []miniagent.Message {
	for i := range slices.Backward(msgs) {
		if msgs[i].Kind == miniagent.KindSummary {
			return msgs[i:]
		}
	}
	return msgs
}

// selectTailByTokens 按 token 预算从最近轮累加选 tail（移植 opencode select，§P1-E）。
// maxTurns=keepRecent（轮数上界）；tokenBudget=preserveRecentTokens(...)。流程：从最近轮向前累加
// estimateRoundTokens(轮)（边际，不含 system/schema），整轮装下并入 tail；首个装不下的边界轮调
// splitRoundByTokens 找安全切点（切出后缀并入 tail），切不动（tool-call 轮）转 shrinkRoundToolContents
// 压 tool content 贴合剩余预算并入 tail，仍不行则整轮进 middle；边界轮之前的全部进 middle。
// tokenBudget<=0 退化为「最近 maxTurns 轮」纯轮数模式（向后兼容）。返回 tail 与 middle 均为扁平 []miniagent.Message（原顺序）。
func selectTailByTokens(rounds [][]miniagent.Message, maxTurns, tokenBudget int) (tail, middle []miniagent.Message) {
	n := len(rounds)
	if n == 0 {
		return nil, nil
	}
	if tokenBudget <= 0 {
		cnt := min(maxTurns, n)
		return flatten(rounds[n-cnt:]), flatten(rounds[:n-cnt])
	}
	total := 0
	// tailStart 初值 0：若全部轮都装下（n<=maxTurns 且未触 token 上界，循环正常结束）则 tail=全部、middle=空。
	// 循环内 break 时覆写为 i+1（tail=rounds[i+1..]）。
	tailStart := 0
	boundary := -1 // token 边界轮索引（需 split/shrink 决策）；-1=无（全部装下或仅 maxTurns 截断）
	for i := n - 1; i >= 0; i-- {
		if n-i > maxTurns {
			tailStart = i + 1 // maxTurns 截断：tail=rounds[i+1..]，older 进 middle
			break
		}
		size := estimateRoundTokens(rounds[i])
		// 最近轮（i==n-1）即使单独超 tokenBudget 也强制并入 tail：最近上下文不可丢，把它压进 middle
		// 会被摘要掉，致模型丢失精确近期上下文。故仅 i<n-1 时才在超预算处取边界轮。
		if i < n-1 && total+size > tokenBudget {
			boundary = i
			tailStart = i + 1
			break
		}
		total += size
	}
	tail = flatten(rounds[tailStart:n])
	middle = flatten(rounds[:tailStart])
	if boundary >= 0 {
		// 边界轮尝试 split/shrink 并入 tail 前端；成功则该轮（压缩后）不进 middle。
		remaining := tokenBudget - total
		if fitted, ok := splitOrShrinkToRound(rounds[boundary], remaining); ok {
			tail = append(append([]miniagent.Message{}, fitted...), tail...)
			middle = flatten(rounds[:boundary]) // boundary 轮已并入 tail，从 middle 移除
		}
		// split/shrink 失败 → 边界轮整轮留 middle（rounds[:tailStart]=rounds[:boundary+1] 已含），符合预期。
	}
	// 不变量：tail 至少含最近 1 轮。兜底覆盖两类曾致空 tail 的退化路径——最近轮单独超 tokenBudget
	// 且 boundary split/shrink 失败（被上面 i<n-1 守卫挡掉，此处双保险），或 maxTurns<=0 在 i==n-1
	// 命中 maxTurns 截断。最近轮进 middle 被摘要永远是语义错误，宁可 tail 略超预算。
	if len(tail) == 0 && n > 0 {
		tail = flatten(rounds[n-1:])
		middle = flatten(rounds[:n-1])
	}
	return tail, middle
}

// splitOrShrinkToRound 是边界轮的适配入口：shrinkRoundToolContents 压 tool content 贴合 remaining。
// 返回 (fitted, ok)；ok=false 表示边界轮无法并入 tail，应整轮进 middle。
// （原 splitRoundByTokens 已删：miniagent splitRounds 使文本轮为单消息、tool-call 轮为 [A(tc)+tools]，
// 单轮内无可安全切的非 tool 消息边界，该函数对生产轮恒返回 nil——YAGNI 删除，连带消除其成功路径丢 boundary 轮前缀的隐患。）
func splitOrShrinkToRound(round []miniagent.Message, remaining int) ([]miniagent.Message, bool) {
	if remaining <= 0 {
		return nil, false
	}
	shrunk := shrinkRoundToolContents(round, remaining)
	if estimateRoundTokens(shrunk) <= remaining {
		return shrunk, true
	}
	return nil, false
}

// shrinkRoundToolContents 是 miniagent flat 模型下 opencode splitTurn 的语义等价物（§P1-E，REFUTED 后的必需补偿）：
// tool-call 轮切不动时，把轮内 tool 结果 content 就地截短贴合 tokenBudget（深拷贝，不动入参，保配对不变）。
// 按 round 当前 estimateRoundTokens 与 tokenBudget 的比例缩放每条 tool content 字符数（复用 text.TruncateHeadTail 头1/4+尾3/4）。
// tokenBudget<=0 原样返回拷贝；无 tool 消息则压缩无意义但仍返回拷贝（由调用方判 fit）。
func shrinkRoundToolContents(round []miniagent.Message, tokenBudget int) []miniagent.Message {
	out := make([]miniagent.Message, len(round))
	copy(out, round)
	if tokenBudget <= 0 {
		return out
	}
	cur := estimateRoundTokens(out)
	if cur <= tokenBudget {
		return out
	}
	ratio := float64(tokenBudget) / float64(cur)
	if ratio > 1 {
		ratio = 1
	}
	for i, m := range out {
		if m.Role == miniagent.RoleTool && len(m.Content) > 0 {
			newLen := int(float64(len([]rune(m.Content))) * ratio)
			newLen = max(1, newLen)
			out[i].Content = text.TruncateHeadTail(m.Content, newLen, "…[tool 结果已压缩]")
		}
	}
	return out
}

// compactWithSummary 保留最早 1 轮 + 最近 keepRecent 轮，中段摘要为单条 miniagent.KindSummary 消息。
// 返回 (out, summary, usage, err)：out 含新 summary；summary 是该消息（.Kind=="" 表示无中段/失败）；
// 中段配对断裂或摘要失败返回 error（调用方 FitHistory 回落 compactHistory）。无中段可摘返回
// (msgs, miniagent.Message{}, miniagent.Usage{}, nil)。不再接收 newMsgs——持久化插入由 Run 完成。
//
// 跨轮继承（P2-1 + §P0-A UPDATE）：上轮 LoadSession 带入的旧 summary 经 applyCompactionBarrier
// 落在 msgs 头，splitRounds 使其单独成 rounds[0]。
//   - 默认路径（SummarizerPrompt==""）：用 stripSummaryPrefix 抽出旧摘要文本作 previousSummary
//     经 Summarize 回调下传（UPDATE 模式），head 置 nil、旧摘要不再并入 middle——省一半 token
//     （旧摘要不再作为 history 重读重写）、显式 preserve 指令降低丢细节概率。这是 99% 用户路径。
//   - override 路径（SummarizerPrompt!=""）：维持旧行为，旧 summary 并入 middle 开头让 LLM 重读
//     重写（previousSummary 传空），已设自定义 prompt 的用户零回归。
//
// 下轮 barrier 命中新 summary 后旧 summary 被丢弃；首轮非 summary（正常 user 轮）维持原行为。
func compactWithSummary(ctx context.Context, budget ContextBudget, msgs []miniagent.Message, keepRecent int) (out []miniagent.Message, summary miniagent.Message, usage miniagent.Usage, err error) {
	// FitHistory 是导出函数，直接调用方可能漏设 Summarize；nil 时无法摘要，返回 error 让 FitHistory 回落有损压缩
	// （NewCompaction 内部总设 Summarize，生产路径不触发此分支）。
	if budget.Summarize == nil {
		return msgs, miniagent.Message{}, miniagent.Usage{}, errors.New("ContextBudget.Summarize 未配置，无法摘要")
	}
	rounds := splitRounds(msgs)
	if len(rounds) <= 1+keepRecent {
		return msgs, miniagent.Message{}, miniagent.Usage{}, nil // 无中段可摘
	}
	head := rounds[0]
	// §B：tail 预算改为联合预算（jointTailBudget）——从 CW×4/5 扣除 summary+head+请求开销后剩多少给 tail，
	// 使不可压缩的 summary 优先、tail 主动让步，减少中等 CW 压缩后超窗 trim/终止。tokenBudget<=0 回落纯轮数
	// 兼容（向后兼容老会话/无窗口）。§P1-E 边界轮细切（split/shrink）语义不变。
	tokenBudget := jointTailBudget(budget, head)
	tail, middleCore := selectTailByTokens(rounds[1:], keepRecent, tokenBudget)
	prevSummary := ""
	if len(head) == 1 && head[0].Kind == miniagent.KindSummary {
		if budget.SummarizerPrompt == "" {
			// 默认路径：抽旧摘要作 UPDATE 锚点，不再并入 middle 重读重写。
			prevSummary = stripSummaryPrefix(head[0].Content)
		} else {
			// override 路径：维持旧行为，旧 summary 并入 middle 让 LLM 重读重写。
			middleCore = append([]miniagent.Message{head[0]}, middleCore...)
		}
		head = nil
	}
	middle := middleCore
	if len(middle) == 0 {
		return msgs, miniagent.Message{}, miniagent.Usage{}, nil
	}
	// 中段必须自洽配对：否则替换进 summary 会留下孤立的 tool_call/tool，续跑被端点 400。
	if err := session.ValidateToolPairing(middle); err != nil {
		return msgs, miniagent.Message{}, miniagent.Usage{}, fmt.Errorf("中段配对断裂，无法安全摘要：%w", err)
	}
	// 摘要前对 middle 全压 strip（keepN=0）：middle 全是待有损摘要的非近期对象，清冗余 reasoning / 折叠被取代的
	// read / 压 write-edit args——省摘要 input token（实测 ~56%）+ 防 middle+summaryMaxTokens 超摘要模型 CW。
	// applyContextStrips 只改字段不删消息、不动 ToolCallID，配对不变（上面 ValidateToolPairing 已通过）。
	// logger=nil → dbg=false 直接 strip 零开销。UPDATE 旧 summary 走 prevSummary 不进 middle；override 并入的旧
	// summary 是纯 user 消息，strip 不影响。
	middle = applyContextStrips(ctx, middle, 0, 0, 0, nil, budget.System, budget.Tools)
	compModel := budget.CompactionModel
	if compModel == "" {
		compModel = budget.Model
	}
	// §P2：摘要 LLM 调用前触发 compaction hook（注入 context / 一次性替换 summarizerPrompt）。
	// 必须排在 session.ValidateToolPairing(middle) 通过之后：context 注入仅追加无 tool_calls 的 user 消息，不破坏配对。
	effPrompt, effMiddle, herr := applyCompactingHook(ctx, budget.Compacting, budget.SessionID, compModel, budget.SummarizerPrompt, middle)
	if herr != nil {
		return msgs, miniagent.Message{}, miniagent.Usage{}, herr // 实现A：hook 抛错上抛中止压缩
	}
	sumText, sumUsage, err := budget.Summarize(ctx, compModel, effPrompt, prevSummary, effMiddle)
	if err != nil {
		return msgs, miniagent.Message{}, miniagent.Usage{}, err
	}
	// §P0-B：summaryMsg 显式打新戳 text.NowMs()（不经 appendMsg）——防陈旧的关键触发点：新 Ts 使其前
	// assistant 的真实 usage 在下一轮 estimateTokensFromUsage 中失效（lastApplicableUsageIndex 的
	// latestSummaryTs 抬高），强制回落本地估算重算压缩后的小体积历史，避免陈旧大 usage 立即二次压缩。
	summaryMsg := miniagent.Message{Role: miniagent.RoleUser, Kind: miniagent.KindSummary, Content: summaryPrefix + sumText, Ts: text.NowMs()}
	out = append([]miniagent.Message{}, head...)
	out = append(out, summaryMsg)
	out = append(out, tail...)
	return out, summaryMsg, sumUsage, nil
}
