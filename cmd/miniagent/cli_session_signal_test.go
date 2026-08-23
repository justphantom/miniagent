package main

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// P3 SIGINT exit code: SIGINT takes code 130 (128+SIGINT POSIX), does not emit an error event (clean exit).
// Non-streaming Do returns context.Canceled after ctx cancel, and main os.Exit(130) accordingly.
func TestCLI_SIGINTExits130(t *testing.T) {
	var hitOnce sync.Once
	serverHit := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitOnce.Do(func() { close(serverHit) }) // the subprocess has entered Run and sent the HTTP request
		time.Sleep(5 * time.Second)             // slow response so SIGINT arrives before the response
	}))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], configArgs(t, srv.URL, "-workdir", t.TempDir())...)
	cmd.Stdin = strings.NewReader("prompt")
	env := []string{entrypointEnv}
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "MINIAGENT_API_KEY=") || strings.HasPrefix(kv, "MINIAGENT_TEST_ENTRYPOINTS=") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "MINIAGENT_API_KEY=sk-test")
	cmd.Env = env
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// Wait until srv receives the request (the subprocess has entered Run's HTTP block) before sending SIGINT: a fixed sleep is unreliable under -race
	// load — the subprocess may not have registered its signal handler yet, and SIGINT gets killed by the default disposition (exit -1).
	select {
	case <-serverHit:
	case <-ctx.Done():
		t.Fatalf("subprocess did not hit srv within the timeout: %s", out.String())
	}
	_ = cmd.Process.Signal(syscall.SIGINT)
	err := cmd.Wait()
	var ee *exec.ExitError
	code := 0
	if errors.As(err, &ee) {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("cmd.Wait err=%v out=%s", err, out.String())
	}
	if code != 130 {
		t.Errorf("SIGINT should exit 130, got %d (out=%s)", code, out.String())
	}
	// SIGINT should not emit an error NDJSON event (clean exit, distinct from a real failure).
	if strings.Contains(out.String(), `"type":"error"`) {
		t.Errorf("SIGINT should not emit an error event: %s", out.String())
	}
}
