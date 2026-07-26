package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// fork-based 测试：通过 MINIAGENT_TEST_ENTRYPOINTS=1 让 test binary 重新进入
// main()，覆盖 os.Exit 路径（这些路径无法在进程内测试）。
// os.Args[0] 为 test binary 自身，用 exec.Command 重新启动它。

const entrypointEnv = "MINIAGENT_TEST_ENTRYPOINTS=1"

func TestMain(m *testing.M) {
	if os.Getenv("MINIAGENT_TEST_ENTRYPOINTS") == "1" {
		main()
		// main() 大概率已 os.Exit；若正常返回（成功完成对话路径），
		// 显式 exit 0 避免 go test 框架继续运行（行为依赖版本，是埋雷）。
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestBuildTools_AlwaysRegisters4(t *testing.T) {
	tools := buildTools(t.TempDir())
	if len(tools) != 4 {
		t.Fatalf("got %d tools, want 4", len(tools))
	}
	expect := map[string]bool{"read_file": true, "write_file": true, "edit_file": true, "shell": true}
	for _, tk := range tools {
		if !expect[tk.Name] {
			t.Errorf("unexpected tool %q", tk.Name)
		}
	}
}

func TestBuildTools_EmptyWorkdirStillRegisters(t *testing.T) {
	tools := buildTools("")
	if len(tools) != 4 {
		t.Fatalf("got %d tools, want 4", len(tools))
	}
}

// runMainBin fork 出 test binary 自身，注入 env + stdin + args，返回
// (exitCode, combinedOutput)。extraEnv 追加在默认 env 之后（覆盖同名 key）。
func runMainBin(t *testing.T, stdin string, args []string, extraEnv ...string) (int, string) {
	t.Helper()
	// 用测试自身 ctx 控制 fork 出来的 binary，避免卡死。
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], args...)
	cmd.Stdin = strings.NewReader(stdin)
	// 显式重建 env：剥离可能存在的宿主 MINIAGENT_API_KEY，保证用例独立。
	env := []string{entrypointEnv}
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "MINIAGENT_API_KEY=") ||
			strings.HasPrefix(kv, "MINIAGENT_TEST_ENTRYPOINTS=") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, extraEnv...)
	cmd.Env = env
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	code := 0
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("exec test binary: %v", err)
	}
	return code, out.String()
}

func TestCLI_VersionExitsZero(t *testing.T) {
	code, out := runMainBin(t, "", []string{"-version"})
	if code != 0 {
		t.Errorf("code = %d, want 0; out=%s", code, out)
	}
	if !strings.Contains(out, "miniagent ") {
		t.Errorf("missing version banner: %s", out)
	}
}

func TestCLI_MissingModelExits1(t *testing.T) {
	code, out := runMainBin(t, "prompt", nil)
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(out, "--model is required") {
		t.Errorf("missing error: %s", out)
	}
}

func TestCLI_MissingAPIKeyExits1(t *testing.T) {
	code, out := runMainBin(t, "prompt", []string{"-model", "x"})
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(out, "API_KEY is required") {
		t.Errorf("missing error: %s", out)
	}
}

func TestCLI_EmptyStdinExits1(t *testing.T) {
	// 提供 API_KEY 跳过前置校验，专测 stdin empty 路径。
	code, out := runMainBin(t, "", []string{"-model", "x"}, "MINIAGENT_API_KEY=sk-test")
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(out, "stdin is empty") {
		t.Errorf("missing error: %s", out)
	}
}
