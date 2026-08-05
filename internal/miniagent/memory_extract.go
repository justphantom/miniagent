package miniagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// memoryExtractMaxTokens 限制抽取请求的输出 token：仅产 ≤K 条短 JSON，无需长输出。
const memoryExtractMaxTokens = 768

func getMemoryExtractMaxTokens() int { return memoryExtractMaxTokens }

// defaultMemoryExtractPrompt 是抽取记忆的默认 system prompt（%d=最大条数，%s=已有记忆）。
// 对话内容不进 system，而是作为 user message 传入（见 ExtractMemory）——某些端点（如 agnes）
// 要求 messages 含 user role，仅 system 会被 400 "No user query found in messages" 拒绝。
const defaultMemoryExtractPrompt = `你是对话记忆抽取器。从用户消息提供的对话中抽取不超过 %d 条值得长期记住的项目事实，严格只输出一个 JSON 数组，每个元素形如 {"type":"note","topic":"可选主题","content":"事实"}，不要输出任何解释或多余文字。

规则：
1. 只抽稳定长期事实：架构决策、约定规范、目录/接口结构、已踩坑与规避方法、用户偏好。不抽临时过程、单次命令输出、闲聊或易变状态。
2. 与「已有记忆」语义重复的，不要输出。
3. 严禁输出密钥、token、口令、凭证或个人隐私；含敏感信息的整条丢弃。
4. 没有值得记的事实时，输出 []。

已有记忆：
%s`

// ExtractMemory 调 llm 从 transcript 抽取至多 maxRecords 条记忆。
// system 放角色/规则/已有记忆锚点（prompt 占位符：%d=条数、%s=已有记忆）；
// transcript 经 renderTranscript 渲染后作为单条 user message 传入——某些端点（如 agnes）
// 要求 messages 含 user role，仅 system 会被 400 拒绝。
// existing 用于去重锚点；secrets 中的字面量若出现在某条 content 内则该条被丢弃。
// 返回待追加记录与该次调用 Usage。模型输出解析失败时返回 (nil, usage, nil)——丢弃但不视为错误。
func ExtractMemory(ctx context.Context, llm *ChatClient, model, prompt string, maxRecords int, existing []MemoryRecord, secrets []string, transcript []Message) ([]MemoryRecord, Usage, error) {
	if llm == nil {
		return nil, Usage{}, errors.New("ExtractMemory: llm 为 nil")
	}
	if maxRecords <= 0 || len(transcript) == 0 {
		return nil, Usage{}, nil
	}
	if strings.TrimSpace(prompt) == "" {
		prompt = defaultMemoryExtractPrompt
	}
	sys := fmt.Sprintf(prompt, maxRecords, formatExistingForDedup(existing))
	resp, err := llm.Do(ctx, Request{
		Model:     model,
		System:    sys,
		Messages:  []Message{{Role: roleUser, Content: renderTranscript(transcript)}},
		MaxTokens: getMemoryExtractMaxTokens(),
	})
	if err != nil {
		return nil, resp.Usage, err
	}
	raw := stripCodeFence(strings.TrimSpace(resp.Text))
	var recs []memoryRecord
	// 解析失败：丢弃本次抽取，但不向上报错（best-effort，不阻断会话退出）。
	_ = json.Unmarshal([]byte(raw), &recs)
	recs = filterMemoryRecords(recs, maxRecords, existing, secrets)
	return recs, resp.Usage, nil
}

// renderTranscript 把对话渲染为紧凑文本供抽取 user message 使用（每条内容截断，整体有上限）。
func renderTranscript(msgs []Message) string {
	const perMsgCap = 1500
	const totalCap = 20000
	var sb strings.Builder
	for _, m := range msgs {
		var contentB strings.Builder
		contentB.WriteString(strings.TrimSpace(m.Content))
		for _, tc := range m.ToolCalls {
			contentB.WriteString("\n[tool_call ")
			contentB.WriteString(tc.Name)
			contentB.WriteString("] ")
			contentB.WriteString(tc.Args)
		}
		if m.Kind == KindSummary {
			contentB.WriteString("[既往对话摘要] ")
		}
		fmt.Fprintf(&sb, "[%s] %s\n", m.Role, truncate(contentB.String(), perMsgCap, "…"))
		if sb.Len() > totalCap {
			sb.WriteString("…[对话已截断]")
			break
		}
	}
	return strings.TrimRight(sb.String(), "\n")
}

// formatExistingForDedup 把已有记忆渲染为去重锚点（取最近 20 条，避免 prompt 过长）。
func formatExistingForDedup(existing []memoryRecord) string {
	if len(existing) == 0 {
		return "（无）"
	}
	start := 0
	if len(existing) > 20 {
		start = len(existing) - 20
	}
	var sb strings.Builder
	for _, r := range existing[start:] {
		if r.Topic != "" {
			fmt.Fprintf(&sb, "- %s: %s\n", r.Topic, r.Content)
		} else {
			fmt.Fprintf(&sb, "- %s\n", r.Content)
		}
	}
	return strings.TrimRight(sb.String(), "\n")
}

// filterMemoryRecords 过滤/去重/限流抽取结果：去空白、剔密钥、本地精确去重、补默认 type、截到 maxRecords。
func filterMemoryRecords(in []memoryRecord, maxRecords int, existing []memoryRecord, secrets []string) []memoryRecord {
	seen := make(map[string]bool, len(existing))
	for _, r := range existing {
		seen[normalizeContent(r.Content)] = true
	}
	var out []memoryRecord
	for _, r := range in {
		c := strings.TrimSpace(r.Content)
		if c == "" {
			continue
		}
		if containsSecret(c, secrets) {
			continue
		}
		key := normalizeContent(c)
		if seen[key] {
			continue
		}
		seen[key] = true
		r.Content = c
		if r.Type == "" {
			r.Type = "note"
		}
		out = append(out, r)
		if len(out) >= maxRecords {
			break
		}
	}
	return out
}

func containsSecret(s string, secrets []string) bool {
	for _, sc := range secrets {
		if sc != "" && strings.Contains(s, sc) {
			return true
		}
	}
	return false
}

func normalizeContent(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(s))), " ")
}

// stripCodeFence 去除模型可能包裹的 ```json ... ``` 代码块围栏。
func stripCodeFence(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		if idx := strings.IndexByte(s, '\n'); idx >= 0 {
			s = s[idx+1:]
		}
		s = strings.TrimSuffix(strings.TrimSpace(s), "```")
		s = strings.TrimSpace(s)
	}
	return s
}
