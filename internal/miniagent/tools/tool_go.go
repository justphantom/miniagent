package tools

import (
	"context"
	"encoding/json"
	"fmt"
	miniagent "github.com/justphantom/miniagent/internal/miniagent"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type goArgs struct {
	Subcommand string `json:"subcommand"`
	Args       string `json:"args,omitempty"`
}

var allowedGoSubcommands = map[string]bool{
	"build": true, "test": true, "vet": true, "run": true,
	"doc": true, "list": true, "info": true, "bug": true,
	"version": true, "clean": true,
}

func GoTool(workspaceRoot string, timeout time.Duration) miniagent.Tool {
	if timeout <= 0 {
		timeout = fileOpTimeout
	}
	return miniagent.Tool{
		Name:        "go",
		Description: "Constrained go operations for building/testing. Blocks write operations like get/install/mod tidy.",
		Parameters: object(map[string]any{
			"subcommand": map[string]any{"type": "string", "description": "Go subcommand"},
			"args":       map[string]any{"type": "string", "description": "Additional arguments"},
		}, "subcommand"),
		Call: func(ctx context.Context, args string) miniagent.ToolResult {
			return runWithTimeout(ctx, timeout, "go", func(rctx context.Context) miniagent.ToolResult {
				return runGo(rctx, workspaceRoot, args)
			})
		},
	}
}

func runGo(ctx context.Context, workspaceRoot, args string) miniagent.ToolResult {
	var a goArgs
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("argument parsing failed: %v", err)}
	}
	if strings.TrimSpace(a.Subcommand) == "" {
		return miniagent.ToolResult{IsError: true, Output: "missing argument: subcommand"}
	}
	if !allowedGoSubcommands[a.Subcommand] {
		return miniagent.ToolResult{
			IsError: true,
			Output: fmt.Sprintf("go %q is not allowed in default mode; use one of: build, test, vet, run, doc, list, info, bug, version, clean", a.Subcommand),
		}
	}
	modDir := resolveModuleRoot(workspaceRoot)
	cmdArgs := []string{a.Subcommand}
	if a.Args != "" {
		cmdArgs = append(cmdArgs, strings.Fields(a.Args)...)
	}
	cmd := exec.CommandContext(ctx, "go", cmdArgs...)
	cmd.Dir = modDir
	cmd.Env = scrubEnv(os.Environ())
	body, err := runLimitedOutput(ctx, cmd, maxShellOutputChars)
	if err != nil {
		return miniagent.ToolResult{IsError: true, Output: fmt.Sprintf("go %s failed: %v\n%s", a.Subcommand, err, body)}
	}
	if body == "" {
		body = "(no output)\n"
	}
	return miniagent.ToolResult{Output: body}
}

func resolveModuleRoot(startDir string) string {
	dir := startDir
	if dir == "" {
		var err error
		dir, err = os.Getwd()
		if err != nil {
			return "."
		}
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "."
}
EOF