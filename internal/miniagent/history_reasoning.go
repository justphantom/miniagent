package miniagent

import (
	"slices"
)

// stripStaleReasoning 主动清空非最近 keepN 条 assistant 消息的 Reasoning（P1）。
// 思考模型单次 reasoning 常达正文数倍，每轮原样回灌是隐性 token 大户；而模型下一步决策几乎不需
// 回看更早的思考——结论已凝结在当时的 assistant 正文 / tool_calls 里。Reasoning 与 tool_calls/tool
// 配对无关（配对要求的是 tool_calls ↔ tool 一一对应），故清空不破坏配对、不丢任何可见事实。
//
// 与 trimHistoryForContext（被动、撞上限才清全部）互补：本函数主动、常态化、仅清「非最近 N 条」，
// 保留当前推理上下文。仅改 context 侧拷贝——入参 msgs/newMsgs/session 不被改动（持久化仍留原 reasoning）。
// keepN<0 视作 0；无非空 reasoning 可清、或全部在保留窗口内时原样返回（零拷贝）。
func stripStaleReasoning(msgs []Message, keepN int) []Message {
	if keepN < 0 {
		keepN = 0
	}
	nAssistant := 0
	hasReasoning := false
	for _, m := range msgs {
		if m.Role == roleAssistant {
			nAssistant++
			if m.Reasoning != "" {
				hasReasoning = true
			}
		}
	}
	if !hasReasoning || nAssistant <= keepN {
		return msgs // 无可清 / 全在保留窗口内
	}
	out := make([]Message, len(msgs))
	copy(out, msgs)
	// 从后往前保留最近 keepN 条 assistant 的 reasoning，更早的清空。
	kept := 0
	for i := range slices.Backward(out) {
		if out[i].Role != roleAssistant {
			continue
		}
		if kept < keepN {
			kept++
			continue
		}
		out[i].Reasoning = ""
	}
	return out
}

// truncateKeptReasoning（P7）对保留窗口内（最近 keepN 条 assistant）的超长 Reasoning 做头尾分段截断。
// 与 stripStaleReasoning 互补：后者清空「非保留」条（减条数），本函数裁「保留」条的单条体积——
// 思考模型单条 Reasoning 常达数千~数万 token，是单条最大的上下文项；stripStaleReasoning 的「全有/全无」
// 逻辑使保留的那条仍逐字全量回灌（wire.go 的 reasoning_content 字段），P1–P4 完全没减这部分体积。
//
// 仅当 Reasoning 超 threshold（rune）才截：复用 truncateHeadTail 的头 1/4 + 尾 3/4 比例——两端高价值
// （开头建立问题框架、收尾收敛到结论/动作），中段多为发散探索/试错/自我纠正，参考价值最低。threshold<=0
// 视作关闭原样返回；未超阈值、或保留窗口内无超长项时零拷贝原样返回。仅改 context 侧拷贝——Reasoning 是
// string（不可变），改 out[i].Reasoning 不污染调用方输入/newMsgs/session；不动正文/tool_calls/配对。
func truncateKeptReasoning(msgs []Message, keepN, threshold int) []Message {
	if threshold <= 0 || keepN <= 0 {
		return msgs
	}
	// 预扫：从后往前遍历保留窗口（最近 keepN 条 assistant），检查是否有超长 Reasoning。
	kept := 0
	hasOversized := false
	for i := range slices.Backward(msgs) {
		if msgs[i].Role != roleAssistant {
			continue
		}
		if kept >= keepN {
			break
		}
		kept++
		if len([]rune(msgs[i].Reasoning)) > threshold {
			hasOversized = true
			break
		}
	}
	if !hasOversized {
		return msgs
	}
	out := make([]Message, len(msgs))
	copy(out, msgs)
	kept = 0
	for i := range slices.Backward(out) {
		if out[i].Role != roleAssistant {
			continue
		}
		if kept >= keepN {
			break
		}
		kept++
		if len([]rune(out[i].Reasoning)) > threshold {
			out[i].Reasoning = truncateHeadTail(out[i].Reasoning, threshold, "…[推理中段已省略]")
		}
	}
	return out
}
