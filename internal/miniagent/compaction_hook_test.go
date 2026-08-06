package miniagent

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// §P2 applyCompactingHook 表驱动：nil noop / prompt override / context append / hook error abort。
func TestApplyCompactingHook(t *testing.T) {
	middle := []Message{{Role: RoleUser, Content: "m1"}}
	t.Run("nil_noop", func(t *testing.T) {
		p, m, err := applyCompactingHook(context.Background(), nil, "s", "model", "orig", middle)
		if err != nil || p != "orig" || len(m) != 1 {
			t.Errorf("nil hook 应原样返回: prompt=%q m=%d err=%v", p, len(m), err)
		}
	})
	t.Run("prompt_override", func(t *testing.T) {
		hook := func(_ context.Context, _ CompactingInput) (CompactingOutput, error) {
			return CompactingOutput{Prompt: "CUSTOM"}, nil
		}
		p, m, err := applyCompactingHook(context.Background(), hook, "s", "model", "orig", middle)
		if err != nil || p != "CUSTOM" || len(m) != 1 {
			t.Errorf("prompt override: prompt=%q m=%d err=%v", p, len(m), err)
		}
	})
	t.Run("empty_prompt_keeps_orig", func(t *testing.T) {
		hook := func(_ context.Context, _ CompactingInput) (CompactingOutput, error) {
			return CompactingOutput{}, nil // 空 Prompt
		}
		p, _, err := applyCompactingHook(context.Background(), hook, "s", "model", "orig", middle)
		if err != nil || p != "orig" {
			t.Errorf("空 Prompt 应沿用原值: prompt=%q err=%v", p, err)
		}
	})
	t.Run("context_append", func(t *testing.T) {
		hook := func(_ context.Context, _ CompactingInput) (CompactingOutput, error) {
			return CompactingOutput{Context: []string{"ctx-a", "ctx-b"}}, nil
		}
		_, m, err := applyCompactingHook(context.Background(), hook, "s", "model", "orig", middle)
		if err != nil {
			t.Fatal(err)
		}
		if len(m) != 2 || m[0].Content != "m1" {
			t.Errorf("context 注入应保留原 middle 在前: %+v", m)
		}
		if m[1].Role != RoleUser || !strings.Contains(m[1].Content, "ctx-a") || !strings.Contains(m[1].Content, "ctx-b") {
			t.Errorf("末尾应追加一条含 context 的 user 消息: %+v", m[1])
		}
		if len(m[1].ToolCalls) != 0 {
			t.Errorf("注入消息不应带 tool_calls（保配对）: %+v", m[1].ToolCalls)
		}
	})
	t.Run("hook_error_aborts", func(t *testing.T) {
		hook := func(_ context.Context, _ CompactingInput) (CompactingOutput, error) {
			return CompactingOutput{}, errors.New("boom")
		}
		_, _, err := applyCompactingHook(context.Background(), hook, "s", "model", "orig", middle)
		if err == nil || !strings.Contains(err.Error(), "boom") {
			t.Errorf("实现A：hook 抛错应上抛中止: err=%v", err)
		}
	})
}

// §P2 compactWithSummary：hook 收到 middle 与 compModel；返回 Prompt 仅本次覆盖 Summarize 入参。
func TestCompactWithSummary_HookReceivesAndOverrides(t *testing.T) {
	var gotIn CompactingInput
	var gotSys string
	budget := ContextBudget{
		Model:            "main",
		CompactionModel:  "comp",
		SummarizerPrompt: "ORIG_PROMPT",
		Compacting: func(_ context.Context, in CompactingInput) (CompactingOutput, error) {
			gotIn = in
			return CompactingOutput{Prompt: "CUSTOM"}, nil
		},
		Summarize: func(_ context.Context, _, sys, _ string, _ []Message) (string, Usage, error) {
			gotSys = sys
			return "summary", Usage{}, nil
		},
	}
	msgs := []Message{
		{Role: RoleUser, Content: "h0"}, {Role: RoleUser, Content: "h1"}, {Role: RoleUser, Content: "h2"},
		{Role: RoleUser, Content: "h3"}, {Role: RoleUser, Content: "h4"}, {Role: RoleUser, Content: "h5"},
		{Role: RoleUser, Content: "cur"},
	}
	if _, _, _, err := compactWithSummary(context.Background(), budget, msgs, 2); err != nil {
		t.Fatalf("compactWithSummary: %v", err)
	}
	if gotIn.Model != "comp" {
		t.Errorf("hook Model = %q, want comp", gotIn.Model)
	}
	if len(gotIn.Middle) == 0 {
		t.Error("hook 应收到非空 middle")
	}
	if gotSys != "CUSTOM" {
		t.Errorf("Summarize sys 应被 hook 覆盖为 CUSTOM: %q", gotSys)
	}
	if budget.SummarizerPrompt != "ORIG_PROMPT" {
		t.Errorf("budget.SummarizerPrompt 不应被持久修改: %q", budget.SummarizerPrompt)
	}
}

// §P2 compactWithSummary：hook 抛错 → 上抛中止（实现A），Summarize 不被调用。
func TestCompactWithSummary_HookErrorAborts(t *testing.T) {
	summarized := false
	budget := ContextBudget{
		Model: "m",
		Compacting: func(_ context.Context, _ CompactingInput) (CompactingOutput, error) {
			return CompactingOutput{}, errors.New("hook boom")
		},
		Summarize: func(_ context.Context, _, _, _ string, _ []Message) (string, Usage, error) {
			summarized = true
			return "x", Usage{}, nil
		},
	}
	msgs := []Message{
		{Role: RoleUser, Content: "h0"}, {Role: RoleUser, Content: "h1"}, {Role: RoleUser, Content: "h2"},
		{Role: RoleUser, Content: "h3"}, {Role: RoleUser, Content: "h4"}, {Role: RoleUser, Content: "cur"},
	}
	_, _, _, err := compactWithSummary(context.Background(), budget, msgs, 2)
	if err == nil {
		t.Fatal("hook 抛错应上抛中止压缩")
	}
	if summarized {
		t.Error("hook 抛错后 Summarize 不应被调用")
	}
}

// §P2 FitHistory：noop fit（ContextWindow<=0）不触发 hook；超窗时触发一次。
func TestFitHistory_HookFiring(t *testing.T) {
	t.Run("noop_no_fire", func(t *testing.T) {
		calls := 0
		budget := ContextBudget{
			Compacting: func(_ context.Context, _ CompactingInput) (CompactingOutput, error) {
				calls++
				return CompactingOutput{}, nil
			},
			Summarize: func(_ context.Context, _, _, _ string, _ []Message) (string, Usage, error) {
				return "x", Usage{}, nil
			},
		}
		FitHistory(context.Background(), []Message{{Role: RoleUser, Content: "q"}}, budget, nil)
		if calls != 0 {
			t.Errorf("noop fit 不应触发 hook: calls=%d", calls)
		}
	})
	t.Run("overflow_fires_once", func(t *testing.T) {
		calls := 0
		big := strings.Repeat("x", 1000)
		var msgs []Message
		for range 30 {
			msgs = append(msgs, Message{Role: RoleUser, Content: big})
		}
		budget := ContextBudget{
			ContextWindow: 4000,
			KeepRecent:    3,
			Compacting: func(_ context.Context, _ CompactingInput) (CompactingOutput, error) {
				calls++
				return CompactingOutput{}, nil
			},
			Summarize: func(_ context.Context, _, _, _ string, _ []Message) (string, Usage, error) {
				return "summary", Usage{}, nil
			},
		}
		FitHistory(context.Background(), msgs, budget, nil)
		if calls != 1 {
			t.Errorf("超窗应触发 hook 恰好一次: calls=%d", calls)
		}
	})
}
