package compaction

import "github.com/justphantom/miniagent/internal/miniagent"

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// testBudget 用 llm 构造 ContextBudget：Summarize 回调调 summarizeMiddle（maxChars=内置上限）。
// Model/CompactionModel/System/Tools 留零值（这些测试不关心 token 估算窗口，直接调 compactWithSummary）。
func testBudget(llm *miniagent.ChatClient) ContextBudget {
	return ContextBudget{
		Model: "m",
		Summarize: func(ctx context.Context, model, sys, prevSummary string, middle []miniagent.Message) (string, miniagent.Usage, error) {
			return summarizeMiddle(ctx, llm, model, sys, prevSummary, summaryMaxChars, middle)
		},
	}
}

// applyCompactionBarrier：有 summary → 返回最新 summary 及之后；无 → 原样。
func TestApplyCompactionBarrier(t *testing.T) {
	msgs := []miniagent.Message{
		{Role: miniagent.RoleUser, Content: "old1"},
		{Role: miniagent.RoleUser, Kind: miniagent.KindSummary, Content: "sum1"},
		{Role: miniagent.RoleUser, Content: "old2"},
		{Role: miniagent.RoleUser, Kind: miniagent.KindSummary, Content: "sum2"},
		{Role: miniagent.RoleUser, Content: "recent"},
	}
	out := applyCompactionBarrier(msgs)
	if len(out) != 2 || out[0].Content != "sum2" || out[1].Content != "recent" {
		t.Errorf("barrier should keep latest summary onward: %+v", out)
	}
	none := applyCompactionBarrier([]miniagent.Message{{Role: miniagent.RoleUser, Content: "x"}})
	if len(none) != 1 {
		t.Errorf("no summary → unchanged: %+v", none)
	}
}

// compactWithSummary：中段摘要为 miniagent.KindSummary，结构（最早 1 轮 + summary + 最近 N 轮）正确，
// 且经 insertSummaryIntoNewMsgs 写入 newMsgs（持久化）。
func TestCompactWithSummary_Success(t *testing.T) {
	tr := &fakeTransport{responses: []string{textResponse("压缩摘要")}}
	llm := &miniagent.ChatClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	var msgs []miniagent.Message
	for i := range 10 {
		msgs = append(msgs, miniagent.Message{Role: miniagent.RoleUser, Content: "q" + strconv.Itoa(i)})
	}
	var newMsgs []miniagent.Message
	out, summary, _, err := compactWithSummary(context.Background(), testBudget(llm), msgs, 3)
	if err != nil {
		t.Fatalf("compactWithSummary: %v", err)
	}
	if summary.Kind != miniagent.KindSummary {
		t.Fatal("expected summary.Kind == miniagent.KindSummary")
	}
	if err := miniagent.ValidateToolPairing(out); err != nil {
		t.Errorf("result pairing broken: %v", err)
	}
	// 最早 1 轮 + summary + 最近 3 轮
	if len(out) != 1+1+3 {
		t.Errorf("out len = %d, want 5", len(out))
	}
	if out[1].Kind != miniagent.KindSummary || !strings.Contains(out[1].Content, "压缩摘要") {
		t.Errorf("summary slot wrong: %+v", out[1])
	}
	insertSummaryIntoNewMsgs(&newMsgs, summary)
	if len(newMsgs) != 1 || newMsgs[0].Kind != miniagent.KindSummary {
		t.Errorf("summary not persisted to newMsgs: %+v", newMsgs)
	}
}

// 中段配对断裂（孤立 tool 消息）→ 不摘要，返回 error（调用方回落有损）。
func TestCompactWithSummary_PairingBreakErrors(t *testing.T) {
	tr := &fakeTransport{responses: []string{textResponse("x")}}
	llm := &miniagent.ChatClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	msgs := []miniagent.Message{
		{Role: miniagent.RoleUser, Content: "first"},
		{Role: miniagent.RoleTool, ToolCallID: "orphan", Content: "x"}, // 断裂
		{Role: miniagent.RoleUser, Content: "u2"},
		{Role: miniagent.RoleUser, Content: "u3"},
		{Role: miniagent.RoleUser, Content: "u4"},
	}
	if _, _, _, err := compactWithSummary(context.Background(), testBudget(llm), msgs, 1); err == nil {
		t.Fatal("expected pairing-break error")
	}
}

// 无中段可摘（轮数 ≤ 1+keepRecent）→ summary.Kind==""，不发 LLM 请求，msgs 原样。
func TestCompactWithSummary_NoMiddleNoop(t *testing.T) {
	tr := &fakeTransport{responses: []string{textResponse("should-not-call")}}
	llm := &miniagent.ChatClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	msgs := []miniagent.Message{{Role: miniagent.RoleUser, Content: "u1"}, {Role: miniagent.RoleUser, Content: "u2"}}
	out, summary, _, err := compactWithSummary(context.Background(), testBudget(llm), msgs, 6)
	if err != nil || summary.Kind == miniagent.KindSummary {
		t.Fatalf("expected (no-summary,nil), got (kind=%v,err=%v)", summary.Kind, err)
	}
	if len(out) != len(msgs) {
		t.Errorf("should be unchanged: out=%d", len(out))
	}
	if tr.calls != 0 {
		t.Errorf("should not call LLM without middle: calls=%d", tr.calls)
	}
}

// summarizeMiddle 的 LLM 错误上抛（不吞）。
func TestSummarizeMiddle_LLMError(t *testing.T) {
	tr := &fakeTransport{statuses: []int{http.StatusInternalServerError}}
	llm := &miniagent.ChatClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	if _, _, err := summarizeMiddle(context.Background(), llm, "m", "", "", summaryMaxChars, []miniagent.Message{{Role: miniagent.RoleUser, Content: "q"}}); err == nil {
		t.Error("expected LLM error to propagate")
	}
}

// P2 摘要 token 入预算：summarizeMiddle 回传 LLM usage，供上游累加进 MaxTotalTokens 预算。
func TestSummarizeMiddle_ReturnsUsage(t *testing.T) {
	body := `{"choices":[{"message":{"role":"assistant","content":"摘要"},"finish_reason":"stop"}],"usage":{"prompt_tokens":50,"completion_tokens":30}}`
	tr := &fakeTransport{responses: []string{body}}
	llm := &miniagent.ChatClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	_, usage, err := summarizeMiddle(context.Background(), llm, "m", "", "", summaryMaxChars, []miniagent.Message{{Role: miniagent.RoleUser, Content: "q"}})
	if err != nil {
		t.Fatalf("summarizeMiddle: %v", err)
	}
	if usage.InputTokens != 50 || usage.OutputTokens != 30 {
		t.Errorf("usage = %+v, want {50,30}", usage)
	}
}

// P3-1：摘要请求设置 MaxTokens=summaryMaxTokens。
func TestSummarizeMiddle_SetsMaxTokens(t *testing.T) {
	tr := &fakeTransport{responses: []string{textResponse("摘要")}}
	llm := &miniagent.ChatClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	if _, _, err := summarizeMiddle(context.Background(), llm, "m", "", "", summaryMaxChars, []miniagent.Message{{Role: miniagent.RoleUser, Content: "q"}}); err != nil {
		t.Fatalf("summarizeMiddle: %v", err)
	}
	if !strings.Contains(tr.lastBody, `"max_tokens":1024`) {
		t.Errorf("摘要请求未设置 max_tokens=1024: %s", tr.lastBody)
	}
}

// compactWithSummary 应把 budget.CompactionModel 透传给 Summarize 回调。
func TestCompactWithSummary_CompactionModelOverride(t *testing.T) {
	llm := &miniagent.ChatClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: &fakeTransport{responses: []string{textResponse("x")}}}}
	var gotModel string
	budget := ContextBudget{
		Model:           "main-model",
		CompactionModel: "compaction-model",
		Summarize: func(ctx context.Context, model, sys, prevSummary string, middle []miniagent.Message) (string, miniagent.Usage, error) {
			gotModel = model
			return summarizeMiddle(ctx, llm, model, sys, prevSummary, summaryMaxChars, middle)
		},
	}
	var msgs []miniagent.Message
	for i := range 10 {
		msgs = append(msgs, miniagent.Message{Role: miniagent.RoleUser, Content: "q" + strconv.Itoa(i)})
	}
	if _, _, _, err := compactWithSummary(context.Background(), budget, msgs, 3); err != nil {
		t.Fatalf("compactWithSummary: %v", err)
	}
	if gotModel != "compaction-model" {
		t.Errorf("Summarize model = %q, want compaction-model", gotModel)
	}
}

// §P0-A：buildSummarizerSystem 三模式。CREATE：含模板 6 段、不含 <previous-summary>。
func TestBuildSummarizerSystem_CreateMode(t *testing.T) {
	got := buildSummarizerSystem("", "", 5000)
	for _, want := range []string{"## 目标", "## 关键细节", "## 进展状态", "## 下一步", "## 相关文件"} {
		if !strings.Contains(got, want) {
			t.Errorf("CREATE 模式应含模板段 %q：\n%s", want, got)
		}
	}
	if strings.Contains(got, "<previous-summary>") {
		t.Errorf("CREATE 模式不应含 <previous-summary>：%s", got)
	}
}

// §P0-A：UPDATE：含 <previous-summary> 块包裹旧摘要 + update 指令 + 模板 6 段。
func TestBuildSummarizerSystem_UpdateMode(t *testing.T) {
	got := buildSummarizerSystem("", "旧锚点", 5000)
	for _, want := range []string{"<previous-summary>\n旧锚点\n</previous-summary>", "更新已有的锚定摘要", "## 目标", "## 相关文件"} {
		if !strings.Contains(got, want) {
			t.Errorf("UPDATE 模式应含 %q：\n%s", want, got)
		}
	}
}

// §P0-A：override：summarizerPrompt 非空 → 全量接管，Sprintf 注入 maxChars，不含模板/previous-summary。
func TestBuildSummarizerSystem_Override(t *testing.T) {
	got := buildSummarizerSystem("自定义%d", "旧", 5000)
	if got != "自定义5000" {
		t.Errorf("override = %q, want 自定义5000", got)
	}
	if strings.Contains(got, "<previous-summary>") || strings.Contains(got, "## 目标") {
		t.Errorf("override 不应含模板/previous-summary：%s", got)
	}
}

// §P0-A：stripSummaryPrefix 表驱动（前缀仅展示层，识别必须用 Kind==miniagent.KindSummary）。
func TestStripSummaryPrefix(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{summaryPrefix + "x", "x"},
		{"x", "x"},
		{"", ""},
		{summaryPrefix, ""},
	}
	for i, c := range cases {
		if got := stripSummaryPrefix(c.in); got != c.want {
			t.Errorf("case %d: stripSummaryPrefix(%q) = %q, want %q", i, c.in, got, c.want)
		}
	}
}

// §P0-A：默认路径（SummarizerPrompt=""）检测到 head 是旧 miniagent.KindSummary 时，抽 prevSummary、
// 不并入 middle、head 置空。旧摘要文本经 previousSummary 下传（UPDATE 模式）。
func TestCompactWithSummary_UpdateModeExtractsPrevSummary(t *testing.T) {
	var gotPrev string
	var gotMiddle []miniagent.Message
	budget := ContextBudget{
		Model: "m",
		Summarize: func(_ context.Context, _, _ string, prevSummary string, middle []miniagent.Message) (string, miniagent.Usage, error) {
			gotPrev = prevSummary
			gotMiddle = middle
			return "新摘要", miniagent.Usage{}, nil
		},
	}
	msgs := []miniagent.Message{
		{Role: miniagent.RoleUser, Kind: miniagent.KindSummary, Content: summaryPrefix + "旧摘"},
		{Role: miniagent.RoleUser, Content: "real0"},
		{Role: miniagent.RoleUser, Content: "real1"},
		{Role: miniagent.RoleUser, Content: "real2"},
		{Role: miniagent.RoleUser, Content: "real3"},
		{Role: miniagent.RoleUser, Content: "real4"},
		{Role: miniagent.RoleUser, Content: "本轮"},
	}
	out, summary, _, err := compactWithSummary(context.Background(), budget, msgs, 3)
	if err != nil || summary.Kind != miniagent.KindSummary {
		t.Fatalf("compactWithSummary: kind=%v err=%v", summary.Kind, err)
	}
	if gotPrev != "旧摘" {
		t.Errorf("previousSummary = %q, want 旧摘", gotPrev)
	}
	for _, m := range gotMiddle {
		if m.Kind == miniagent.KindSummary {
			t.Errorf("默认路径 middle 不应含 miniagent.KindSummary（旧摘要应作 prevSummary 下传）：%+v", gotMiddle)
		}
	}
	// head 置空：out = summaryMsg + tail（3），首条为新 summary。
	if len(out) != 1+3 || out[0].Kind != miniagent.KindSummary {
		t.Errorf("out 应为 summary+tail（head 已置空）：%+v", out)
	}
}

// §P0-A：override 路径（SummarizerPrompt!=""）维持旧行为——旧 miniagent.KindSummary 并入 middle，
// previousSummary 传空。
func TestCompactWithSummary_OverrideMergesPrevSummaryIntoMiddle(t *testing.T) {
	var gotPrev string
	var gotMiddle []miniagent.Message
	budget := ContextBudget{
		Model:            "m",
		SummarizerPrompt: "自定义%d",
		Summarize: func(_ context.Context, _, _ string, prevSummary string, middle []miniagent.Message) (string, miniagent.Usage, error) {
			gotPrev = prevSummary
			gotMiddle = middle
			return "新摘要", miniagent.Usage{}, nil
		},
	}
	msgs := []miniagent.Message{
		{Role: miniagent.RoleUser, Kind: miniagent.KindSummary, Content: summaryPrefix + "旧摘"},
		{Role: miniagent.RoleUser, Content: "real0"},
		{Role: miniagent.RoleUser, Content: "real1"},
		{Role: miniagent.RoleUser, Content: "real2"},
		{Role: miniagent.RoleUser, Content: "real3"},
		{Role: miniagent.RoleUser, Content: "real4"},
		{Role: miniagent.RoleUser, Content: "本轮"},
	}
	if _, _, _, err := compactWithSummary(context.Background(), budget, msgs, 3); err != nil {
		t.Fatalf("compactWithSummary: %v", err)
	}
	if gotPrev != "" {
		t.Errorf("override 路径 previousSummary 应为空，got %q", gotPrev)
	}
	if len(gotMiddle) == 0 || gotMiddle[0].Kind != miniagent.KindSummary || !strings.Contains(gotMiddle[0].Content, "旧摘") {
		t.Errorf("override 路径 middle 首条应为旧 miniagent.KindSummary（旧行为）：%+v", gotMiddle)
	}
}

// §P0-A：summarizeMiddle UPDATE 模式把 previous-summary 写进请求 system。
func TestSummarizeMiddle_UpdateModeRequest(t *testing.T) {
	tr := &fakeTransport{responses: []string{textResponse("更新摘要")}}
	llm := &miniagent.ChatClient{APIKey: "sk", ChatURL: "http://localhost", HTTP: &http.Client{Transport: tr}}
	if _, _, err := summarizeMiddle(context.Background(), llm, "m", "", "旧锚点", summaryMaxChars, []miniagent.Message{{Role: miniagent.RoleUser, Content: "q"}}); err != nil {
		t.Fatalf("summarizeMiddle: %v", err)
	}
	// lastBody 是 JSON-marshaled 请求体，< > 被转义成 < >；断言用未转义的标签名 + 旧锚点文本。
	if !strings.Contains(tr.lastBody, "previous-summary") || !strings.Contains(tr.lastBody, "旧锚点") {
		t.Errorf("UPDATE 模式请求应含 previous-summary 块 + 旧锚点：%s", tr.lastBody)
	}
}
