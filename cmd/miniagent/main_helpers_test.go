package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fork-based tests: via MINIAGENT_TEST_ENTRYPOINTS=1 the test binary re-enters
// main(), covering os.Exit paths (these paths cannot be tested in-process).

const entrypointEnv = "MINIAGENT_TEST_ENTRYPOINTS=1"

func TestMain(m *testing.M) {
	if os.Getenv("MINIAGENT_TEST_ENTRYPOINTS") == "1" {
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// writeConfigFixture writes a temporary miniagent.json pointing at srvURL (mode=auto; -workdir is supplied explicitly per e2e caller — workdir is required in all modes),
// and returns its path. When runJSON is non-empty it is used verbatim as the "run" section (supporting config-only params like max_tokens_total/max_duration).
func writeConfigFixture(t *testing.T, srvURL, runJSON string) string {
	t.Helper()
	cfgPath := filepath.Join(t.TempDir(), "miniagent.json")
	runField := ""
	if runJSON != "" {
		runField = `,"run":` + runJSON
	}
	body := `{"providers":[{"name":"p","chat_url":"` + srvURL + `/v1/chat/completions","models_url":"` + srvURL + `/v1/models"}],"defaults":{"provider":"p","model":"m","mode":"auto"},"compaction":{"provider":"p","model":"m"}` + runField + `}`
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return cfgPath
}

// configArgs builds the common args for e2e: writes a temporary config (replacing the old bare-mode chatArgs), returns -config <path> + extra.
func configArgs(t *testing.T, srvURL string, extra ...string) []string {
	t.Helper()
	return append([]string{"-config", writeConfigFixture(t, srvURL, "")}, extra...)
}

// runMainBin forks the test binary itself, injecting env + stdin + args, returning
// (exitCode, combinedOutput). extraEnv is appended after the default env (overriding same-named keys).
func runMainBin(t *testing.T, stdin string, args []string, extraEnv ...string) (int, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], args...)
	cmd.Stdin = strings.NewReader(stdin)
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
