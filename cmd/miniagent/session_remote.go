package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/justphantom/miniagent/config"
	"github.com/justphantom/miniagent/miniagent"
	"github.com/justphantom/miniagent/miniagent/session"
)

// remoteClientOf 按 config 构造 minisession client；session.url 为空返回 nil（本地模式）。
// 读侧 handler（列表/回放/删除）与 runTurn 一致：每次请求新建，Client 只是薄封装
// （URL+key+http.Client），无连接态需要复用。
func remoteClientOf(cfg *config.Config) *session.Client {
	if cfg.Session.URL == "" {
		return nil
	}
	return session.NewClient(cfg.Session.URL, cfg.Session.Key)
}

// resolveSessionRemote 是 resolveSession 的远端镜像：session.url 指向 minisession 时，
// 加载/新建判定走 HTTP 而非本地文件。返回的 remote 供 saveSessionRemote 持久化使用。
//   - saveNew=true: 本地构造 meta（与本地路径一致：jsonl 仅在首 turn 成功后落盘），远端不预创建；
//   - sessionArg!="": LoadSession 404 → errSessionNotFound（与本地路径同哨兵，web 层同样映射 404）；
//   - both empty: 无状态，返回 nil client。
func resolveSessionRemote(ctx context.Context, remote *session.Client, saveNew bool, presetID, sessionArg, modelSpec, provider, workdir string) (session.SessionMeta, []miniagent.Message, error) {
	if !saveNew && sessionArg == "" {
		return session.SessionMeta{}, nil, nil
	}
	id := sessionArg
	if saveNew {
		id = presetID
		if id == "" {
			id = generateSessionID()
		}
	}
	meta, history, err := remote.LoadSession(ctx, id)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if saveNew {
				return session.SessionMeta{
					ID:       id,
					Model:    modelSpec,
					Provider: provider,
					Workdir:  absWorkdir(workdir),
					Created:  time.Now().Format(time.RFC3339),
				}, nil, nil
			}
			return session.SessionMeta{}, nil, fmt.Errorf("%w: session %q (use -save-session to create a new one)", errSessionNotFound, id)
		}
		return session.SessionMeta{}, nil, fmt.Errorf("load session: %w", err)
	}
	warnSessionMismatch(meta, modelSpec, workdir)
	return meta, history, nil
}

// saveSessionRemote 是 saveSession 的远端镜像：Rewrite 为主，404 时先 Create 再 Rewrite
// （远端 Rewrite 语义为替换已存在会话，新建必须显式创建；Create 后立即 Rewrite 幂等安全）。
// 与本地 saveSession 一致：meta.LLMRequests 由调用方累加后传入。
func saveSessionRemote(ctx context.Context, remote *session.Client, meta session.SessionMeta, msgs []miniagent.Message) error {
	if len(msgs) == 0 {
		return nil
	}
	err := remote.RewriteMessages(ctx, meta.ID, meta, msgs)
	if errors.Is(err, os.ErrNotExist) {
		if _, cerr := remote.CreateSession(ctx, meta); cerr != nil {
			return cerr
		}
		return remote.RewriteMessages(ctx, meta.ID, meta, msgs)
	}
	return err
}
