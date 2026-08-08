package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/justphantom/miniagent/internal/miniagent"
	"github.com/justphantom/miniagent/internal/miniagent/session"
)

// defaultSessionDir 是未配 session.dir 时的回落目录（Phase C 由 config 覆盖）。
const defaultSessionDir = ".miniagent/sessions"

// resolveSessionForRun 按三态裁决会话（互斥由 validateConversation 保证不会同时真）：
//   - saveNew=true：新建会话，generateSessionID 生成 id，构造 meta（id 的 stdout NDJSON 输出由 main 的 EmitSession 负责），history=nil。
//   - sessionArg!=""：接续，校验 id 后 LoadSession；文件不存在（meta.Type==""）报错防 typo 建垃圾会话。
//   - 两者皆空：无状态，返回空 path（main 据此跳过落盘）。
func resolveSessionForRun(saveNew bool, sessionArg, sessionDir, modelSpec, provider, workdir string, maxSessionBytes int64) (string, session.SessionMeta, []miniagent.Message) {
	if !saveNew && sessionArg == "" {
		return "", session.SessionMeta{}, nil
	}
	id := sessionArg
	if saveNew {
		id = generateSessionID()
	}
	sessPath, err := session.ResolveSessionPath(id, sessionDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "miniagent: session: %v\n", err)
		os.Exit(1)
	}
	meta, history, err := session.LoadSession(sessPath, maxSessionBytes)
	if err != nil {
		fmt.Fprintf(os.Stderr, "miniagent: load session: %v\n", err)
		os.Exit(1)
	}
	if meta.Type == "" {
		if saveNew {
			// 新会话：构造 metadata（Type 由 AppendMessages 补 session）。Provider 独立于
			// modelSpec（"provider/id"）单列，供会话列举/多 provider 溯源免解析字符串。
			meta = session.SessionMeta{
				ID:       id,
				Model:    modelSpec,
				Provider: provider,
				Workdir:  absWorkdir(workdir),
				Created:  time.Now().Format(time.RFC3339),
			}
		} else {
			// 接续但文件不存在 → 报错（防 typo 创建垃圾会话；新建请用 -save-session）。
			fmt.Fprintf(os.Stderr, "miniagent: session %q 不存在（新建请用 -save-session）\n", id)
			os.Exit(1)
		}
	} else {
		warnSessionMismatch(meta, modelSpec, workdir)
	}
	return sessPath, meta, history
}

// generateSessionID 生成新会话 id：时间戳 + 随机 hex，仅含拉丁字母/数字/-（满足 ValidateSessionID）。
// 随机段 8 字节（64 bit）：同秒并发新建的碰撞阈值抬到 2^32 量级，覆盖 CI 矩阵/批量 fork 场景。
// crypto/rand 失败极罕见，回落时间戳+pid（仍合法；pid 区分同秒不同进程，避免裸时间戳必碰撞）。
func generateSessionID() string {
	ts := time.Now().Format("20060102-150405")
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return ts + "-" + strconv.Itoa(os.Getpid())
	}
	return ts + "-" + hex.EncodeToString(b[:])
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

func warnSessionMismatch(meta session.SessionMeta, modelSpec, workdir string) {
	if modelSpec != "" && meta.Model != "" && meta.Model != modelSpec {
		fmt.Fprintf(os.Stderr, "miniagent: warning: session model %q 与本次 %q 不一致\n", meta.Model, modelSpec)
	}
	if aw := absWorkdir(workdir); aw != "" && meta.Workdir != "" && meta.Workdir != aw {
		fmt.Fprintf(os.Stderr, "miniagent: warning: session workdir %q 与本次 %q 不一致\n", meta.Workdir, aw)
	}
}
