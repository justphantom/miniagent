package compaction

import "github.com/justphantom/miniagent/internal/miniagent"

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// §P2 applyCompactingHook table-driven: nil noop / prompt override / context append / hook error abort.
func TestApplyCompactingHook(t *testing.T) {
	middle := []miniagent.Message{{Role: miniagent.RoleUser, Content: "m1"}}
	t.Run("nil_noop", func(t *testing.T) {
		p, m, err := applyCompactingHook(context.Background(), nil, "s", "model", "orig", middle)
		if err != nil || p != "orig" || len(m) != 1 {
			t.Errorf("nil hook should return as-is: prompt=%q m=%d err=%v", p, len(m), err)
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
			return CompactingOutput{}, nil // empty Prompt
		}
		p, _, err := applyCompactingHook(context.Background(), hook, "s", "model", "orig", middle)
		if err != nil || p != "orig" {
			t.Errorf("empty Prompt should keep original: prompt=%q err=%v", p, err)
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
			t.Errorf("context injection should keep original middle first: %+v", m)
		}
		if m[1].Role != miniagent.RoleUser || !strings.Contains(m[1].Content, "ctx-a") || !strings.Contains(m[1].Content, "ctx-b") {
			t.Errorf("a user message containing context should be appended at the end: %+v", m[1])
		}
		if len(m[1].ToolCalls) != 0 {
			t.Errorf("injected message should not carry tool_calls (keep pairing): %+v", m[1].ToolCalls)
		}
	})
	t.Run("hook_error_aborts", func(t *testing.T) {
		hook := func(_ context.Context, _ CompactingInput) (CompactingOutput, error) {
			return CompactingOutput{}, errors.New("boom")
		}
		_, _, err := applyCompactingHook(context.Background(), hook, "s", "model", "orig", middle)
		if err == nil || !strings.Contains(err.Error(), "boom") {
			t.Errorf("impl A: hook error should propagate and abort: err=%v", err)
		}
	})
}

// §P2 compactWithSummary: hook receives middle and compModel; returning Prompt overrides only this Summarize call's input.
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
		Summarize: func(_ context.Context, _, sys, _ string, _ []miniagent.Message) (string, miniagent.Usage, error) {
			gotSys = sys
			return "summary", miniagent.Usage{}, nil
		},
	}
	msgs := []miniagent.Message{
		{Role: miniagent.RoleUser, Content: "h0"}, {Role: miniagent.RoleUser, Content: "h1"}, {Role: miniagent.RoleUser, Content: "h2"},
		{Role: miniagent.RoleUser, Content: "h3"}, {Role: miniagent.RoleUser, Content: "h4"}, {Role: miniagent.RoleUser, Content: "h5"},
		{Role: miniagent.RoleUser, Content: "cur"},
	}
	if _, _, _, err := compactWithSummary(context.Background(), budget, msgs, 2); err != nil {
		t.Fatalf("compactWithSummary: %v", err)
	}
	if gotIn.Model != "comp" {
		t.Errorf("hook Model = %q, want comp", gotIn.Model)
	}
	if len(gotIn.Middle) == 0 {
		t.Error("hook should receive non-empty middle")
	}
	if gotSys != "CUSTOM" {
		t.Errorf("Summarize sys should be overridden by hook to CUSTOM: %q", gotSys)
	}
	if budget.SummarizerPrompt != "ORIG_PROMPT" {
		t.Errorf("budget.SummarizerPrompt should not be persisted modified: %q", budget.SummarizerPrompt)
	}
}

// §P2 compactWithSummary: hook error -> propagate and abort (impl A), Summarize is not called.
func TestCompactWithSummary_HookErrorAborts(t *testing.T) {
	summarized := false
	budget := ContextBudget{
		Model: "m",
		Compacting: func(_ context.Context, _ CompactingInput) (CompactingOutput, error) {
			return CompactingOutput{}, errors.New("hook boom")
		},
		Summarize: func(_ context.Context, _, _, _ string, _ []miniagent.Message) (string, miniagent.Usage, error) {
			summarized = true
			return "x", miniagent.Usage{}, nil
		},
	}
	msgs := []miniagent.Message{
		{Role: miniagent.RoleUser, Content: "h0"}, {Role: miniagent.RoleUser, Content: "h1"}, {Role: miniagent.RoleUser, Content: "h2"},
		{Role: miniagent.RoleUser, Content: "h3"}, {Role: miniagent.RoleUser, Content: "h4"}, {Role: miniagent.RoleUser, Content: "cur"},
	}
	_, _, _, err := compactWithSummary(context.Background(), budget, msgs, 2)
	if err == nil {
		t.Fatal("hook error should propagate and abort compaction")
	}
	if summarized {
		t.Error("Summarize should not be called after hook error")
	}
}

// §P2 FitHistory: noop fit (ContextWindow<=0) does not trigger hook; triggers once on overflow.
func TestFitHistory_HookFiring(t *testing.T) {
	t.Run("noop_no_fire", func(t *testing.T) {
		calls := 0
		budget := ContextBudget{
			Compacting: func(_ context.Context, _ CompactingInput) (CompactingOutput, error) {
				calls++
				return CompactingOutput{}, nil
			},
			Summarize: func(_ context.Context, _, _, _ string, _ []miniagent.Message) (string, miniagent.Usage, error) {
				return "x", miniagent.Usage{}, nil
			},
		}
		FitHistory(context.Background(), []miniagent.Message{{Role: miniagent.RoleUser, Content: "q"}}, budget, nil)
		if calls != 0 {
			t.Errorf("noop fit should not trigger hook: calls=%d", calls)
		}
	})
	t.Run("overflow_fires_once", func(t *testing.T) {
		calls := 0
		big := strings.Repeat("x", 1000)
		var msgs []miniagent.Message
		for range 30 {
			msgs = append(msgs, miniagent.Message{Role: miniagent.RoleUser, Content: big})
		}
		budget := ContextBudget{
			ContextWindow: 4000,
			KeepRecent:    3,
			Compacting: func(_ context.Context, _ CompactingInput) (CompactingOutput, error) {
				calls++
				return CompactingOutput{}, nil
			},
			Summarize: func(_ context.Context, _, _, _ string, _ []miniagent.Message) (string, miniagent.Usage, error) {
				return "summary", miniagent.Usage{}, nil
			},
		}
		FitHistory(context.Background(), msgs, budget, nil)
		if calls != 1 {
			t.Errorf("overflow should trigger hook exactly once: calls=%d", calls)
		}
	})
}
