package main

import (
	"strings"
	"testing"
)

// defaultSystemPrompt 须覆盖代码向工作流的关键约束，防回归误删。
func TestDefaultSystemPrompt_CoversWorkflow(t *testing.T) {
	for _, want := range []string{"read", "grep", "shell", "edit", "replace_all", "验证", "测试"} {
		if !strings.Contains(defaultSystemPrompt, want) {
			t.Errorf("default prompt missing %q", want)
		}
	}
}
