package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/justphantom/miniagent/internal/miniagent"
)

// defaultSessionDir 是未配 session.dir 时的回落目录（Phase C 由 config 覆盖）。
const defaultSessionDir = ".miniagent/sessions"

// resolveSessionForRun 解析 -session → (path, meta, history)。新会话构造 metadata；
// 已有会话保留 Created 并对 model/workdir 不一致 warn（§3.4）。sessionArg 空 → 不持久化。
func resolveSessionForRun(sessionArg, sessionDir, modelSpec, workdir string) (string, miniagent.SessionMeta, []miniagent.Message) {
	if sessionArg == "" {
		return "", miniagent.SessionMeta{}, nil
	}
	sessPath, err := miniagent.ResolveSessionPath(sessionArg, sessionDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "miniagent: session: %v\n", err)
		os.Exit(1)
	}
	meta, history, err := miniagent.LoadSession(sessPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "miniagent: load session: %v\n", err)
		os.Exit(1)
	}
	if meta.Type == "" {
		// 文件不存在 → 新会话：构造 metadata（Type 由 AppendMessages 补 session）。
		meta = miniagent.SessionMeta{
			ID:      sessionIDFromArg(sessionArg),
			Model:   modelSpec,
			Workdir: absWorkdir(workdir),
			Created: time.Now().Format(time.RFC3339),
		}
	} else {
		warnSessionMismatch(meta, modelSpec, workdir)
	}
	return sessPath, meta, history
}

// sessionIDFromArg：路径取 base 去扩展名；纯 id 原样。
func sessionIDFromArg(arg string) string {
	if filepath.IsAbs(arg) || strings.ContainsAny(arg, "/."+string(filepath.Separator)) {
		return strings.TrimSuffix(filepath.Base(arg), filepath.Ext(arg))
	}
	return arg
}

func absWorkdir(workdir string) string {
	if workdir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return ""
		}
		return wd
	}
	abs, err := filepath.Abs(workdir)
	if err != nil {
		return workdir
	}
	return abs
}

func warnSessionMismatch(meta miniagent.SessionMeta, modelSpec, workdir string) {
	if modelSpec != "" && meta.Model != "" && meta.Model != modelSpec {
		fmt.Fprintf(os.Stderr, "miniagent: warning: session model %q 与本次 %q 不一致\n", meta.Model, modelSpec)
	}
	if aw := absWorkdir(workdir); aw != "" && meta.Workdir != "" && meta.Workdir != aw {
		fmt.Fprintf(os.Stderr, "miniagent: warning: session workdir %q 与本次 %q 不一致\n", meta.Workdir, aw)
	}
}
