package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGo_RejectsDestructiveCommand(t *testing.T) {
	dir := t.TempDir()
	got := GoTool(dir, 0)
	cases := []string{"get github.com/x/y", "install golang.org/x/tools", "mod tidy", "mod download", "mod init", "fmt", "env -w"}
	for _, c := range cases {
		res := got.Call(context.Background(), `{"subcommand":"`+c+`"}`)
		if !res.IsError || !strings.Contains(res.Output, "not allowed") {
			t.Errorf("go %q should be rejected, got: %s", c, res.Output)
		}
	}
}

func TestGo_KnownCmdNotInAllowList(t *testing.T) {
	dir := t.TempDir()
	got := GoTool(dir, 0)
	res := got.Call(context.Background(), `{"subcommand":"foobar"}`)
	if !res.IsError || !strings.Contains(res.Output, "not allowed") {
		t.Errorf("unknown command should be rejected: %s", res.Output)
	}
}

func TestGo_MissingSubcommand(t *testing.T) {
	got := GoTool(t.TempDir(), 0)
	res := got.Call(context.Background(), `{}`)
	if !res.IsError || !strings.Contains(res.Output, "missing argument: subcommand") {
		t.Errorf("expected missing subcommand error: %s", res.Output)
	}
}

func TestGo_ModuleRootResolution(t *testing.T) {
	dir := t.TempDir()
	moduleDir := filepath.Join(dir, "module")
	os.MkdirAll(moduleDir, 0755)
	os.WriteFile(filepath.Join(moduleDir, "go.mod"), []byte("module example.com/test\n\ngo 1.21\n"), 0644)
	got := GoTool(moduleDir, 0)
	res := got.Call(context.Background(), `{"subcommand":"list","args":"-m"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
}

func TestGo_UnknownSubcommandRejected(t *testing.T) {
	dir := t.TempDir()
	got := GoTool(dir, 0)
	res := got.Call(context.Background(), `{"subcommand":"foobar"}`)
	if !res.IsError || !strings.Contains(res.Output, "not allowed in default mode") {
		t.Errorf("unknown command should be rejected: %s", res.Output)
	}
}
