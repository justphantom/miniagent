package policy

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/justphantom/miniagent/miniagent"
)

func TestNeedsConfirm(t *testing.T) {
	words := DefaultDangerWords
	destructive := DefaultDestructiveTools
	cases := []struct {
		name, input string
		want        bool
	}{
		{"write", `{"path":"/x"}`, true},
		{"edit", `{"path":"/x"}`, true},
		{"read", `{"path":"/x"}`, false},
		{"grep", `{"pattern":"x"}`, false},
		{"shell", `{"command":"ls -la"}`, false},
		{"shell", `{"command":"rm -rf /tmp/x"}`, true},
		{"shell", `{"command":"sudo apt update"}`, true},
		{"shell", `{"command":"dd if=/dev/zero of=/dev/sda"}`, true},
	}
	for _, c := range cases {
		if got := needsConfirm(c.name, c.input, words, destructive); got != c.want {
			t.Errorf("needsConfirm(%q,%q) = %v, want %v", c.name, c.input, got, c.want)
		}
	}
}

// emitRecorder is an OnToolUse that records its call and optionally returns a preset error.
type emitRecorder struct {
	called bool
	err    error
}

func (e *emitRecorder) fn() miniagent.OnToolUse {
	return func(name, callID, input string) error {
		e.called = true
		return e.err
	}
}

// Disabled → identity: gate never runs, emit called, destructive passes through (current behavior preserved).
func TestConfirmOnToolUse_Disabled_IsIdentity(t *testing.T) {
	emit := &emitRecorder{}
	hook := ConfirmOnToolUse(emit.fn(), ConfirmCfg{Enabled: false})
	if err := hook("write", "tc1", `{}`); err != nil {
		t.Errorf("disabled should pass destructive through, got %v", err)
	}
	if !emit.called {
		t.Error("emit should still be called when disabled")
	}
}

// Enabled + destructive + AutoApprove → allowed (emit called first).
func TestConfirmOnToolUse_AutoApprove_Allows(t *testing.T) {
	emit := &emitRecorder{}
	hook := ConfirmOnToolUse(emit.fn(), ConfirmCfg{Enabled: true, AutoApprove: true})
	if err := hook("write", "tc1", `{}`); err != nil {
		t.Errorf("AutoApprove should allow destructive, got %v", err)
	}
	if !emit.called {
		t.Error("emit must run before the gate")
	}
}

// Enabled + destructive + non-TTY stdin (strings.Reader is not *os.File) + no AutoApprove → ErrToolDenied
// (deny-by-default); emit still called first (order matters).
func TestConfirmOnToolUse_NonInteractive_Denies(t *testing.T) {
	emit := &emitRecorder{}
	hook := ConfirmOnToolUse(emit.fn(), ConfirmCfg{Enabled: true, Stdin: strings.NewReader(""), Stdout: io.Discard})
	err := hook("write", "tc1", `{}`)
	if !errors.Is(err, miniagent.ErrToolDenied) {
		t.Errorf("non-interactive destructive should deny with ErrToolDenied, got %v", err)
	}
	if !emit.called {
		t.Error("emit must run before the gate (order matters)")
	}
}

// Enabled + nil emit (the -result-only/subagent path: buildHooks returns empty hooks) + destructive + non-TTY → still
// denied. This proves the subagent hole (the most autonomous, riskiest path) is covered, not left uncovered.
func TestConfirmOnToolUse_NilEmit_SubagentCovered(t *testing.T) {
	hook := ConfirmOnToolUse(nil, ConfirmCfg{Enabled: true, Stdin: strings.NewReader(""), Stdout: io.Discard})
	if err := hook("write", "tc1", `{}`); !errors.Is(err, miniagent.ErrToolDenied) {
		t.Errorf("nil-emit subagent path should still deny destructive in non-interactive mode, got %v", err)
	}
}

// Enabled + non-destructive → no gate.
func TestConfirmOnToolUse_NonDestructive_Unaffected(t *testing.T) {
	emit := &emitRecorder{}
	hook := ConfirmOnToolUse(emit.fn(), ConfirmCfg{Enabled: true, Stdin: strings.NewReader(""), Stdout: io.Discard})
	if err := hook("read", "tc1", `{}`); err != nil {
		t.Errorf("non-destructive should be ungated, got %v", err)
	}
}

// emit error propagates BEFORE the gate. A destructive tool is used deliberately to prove the gate did not run/deny.
func TestConfirmOnToolUse_EmitErrorPropagated(t *testing.T) {
	sentinel := errors.New("pipe closed")
	emit := &emitRecorder{err: sentinel}
	hook := ConfirmOnToolUse(emit.fn(), ConfirmCfg{Enabled: true, Stdin: strings.NewReader(""), Stdout: io.Discard})
	if err := hook("write", "tc1", `{}`); !errors.Is(err, sentinel) {
		t.Errorf("emit error should propagate before the gate, got %v", err)
	}
}

// readYes: y/yes (any case) → true; everything else / EOF → false.
func TestReadYes(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"y\n", true},
		{"yes\n", true},
		{"Y\n", true},
		{"  Yes  \n", true},
		{"n\n", false},
		{"no\n", false},
		{"\n", false},
		{"", false},
	}
	for _, c := range cases {
		var out strings.Builder
		if got := readYes(strings.NewReader(c.in), &out, "write", "{}"); got != c.want {
			t.Errorf("readYes(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
