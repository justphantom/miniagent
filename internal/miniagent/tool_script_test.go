package miniagent

import (
	"context"
	"strings"
	"testing"
)

// ScriptTool 执行固定命令，工具名加 script_ 前缀。
func TestScriptTool_ExecutesAndNames(t *testing.T) {
	tl := ScriptTool("test", "运行测试", "echo hi", t.TempDir(), 0, ModeAuto, 0, 0)
	if tl.Name != "script_test" {
		t.Errorf("Name = %q, want script_test", tl.Name)
	}
	r := tl.Call(context.Background(), `{}`)
	if r.IsError {
		t.Fatalf("script exec: %s", r.Output)
	}
	if !strings.Contains(r.Output, "hi") {
		t.Errorf("output = %q, want contains hi", r.Output)
	}
}

// 可选 args 追加到命令末尾。
func TestScriptTool_AppendArgs(t *testing.T) {
	tl := ScriptTool("echo", "回显", "echo", t.TempDir(), 0, ModeAuto, 0, 0)
	r := tl.Call(context.Background(), `{"args":"a b"}`)
	if r.IsError {
		t.Fatalf("script exec: %s", r.Output)
	}
	if !strings.Contains(r.Output, "a b") {
		t.Errorf("output = %q, want appended args", r.Output)
	}
}

// default 模式继承 shell 安全策略：固定命令含 sudo 仍被拒。
func TestScriptTool_DefaultModeBlocksSudo(t *testing.T) {
	tl := ScriptTool("evil", "危险", "sudo rm -rf /", t.TempDir(), 0, ModeDefault, 0, 0)
	r := tl.Call(context.Background(), `{}`)
	if !r.IsError {
		t.Error("default mode script with sudo should be blocked")
	}
}
