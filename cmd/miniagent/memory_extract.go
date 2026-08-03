package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/justphantom/miniagent/internal/miniagent"
)

// memoryExtractTimeout 是抽取调用的独立超时：不复用 runCtx（其 -max-duration 到期会让抽取
// 立刻失败），也不计入会话 token 预算（会话已结束）。
const memoryExtractTimeout = 30 * time.Second

// memoryExtractor 封装会话结束后的记忆抽取。extract 是 best-effort：任何失败仅 warn，
// 不影响会话退出码与已保存的 session。无 workdir / client / 未启用 / 无工具使用时跳过。
type memoryExtractor struct {
	enabled bool
	workdir string
	model   string
	maxK    int
	prompt  string
	secrets []string
	client  *miniagent.ChatClient // 抽取专用 client（可能 == 主/compaction client）
	logger  *slog.Logger
}

// extract 对 transcript 抽取并追加记忆。
// 内部使用 context.Background() 而非调用方 ctx：runCtx 可能在 -max-duration 到期后已
// DeadlineExceeded，复用会让抽取立刻失败；此处希望超时退出仍有机会抽取记忆。
func (m *memoryExtractor) extract(transcript []miniagent.Message) {
	ctx := context.Background()
	if m == nil || !m.enabled || m.workdir == "" || m.client == nil {
		return
	}
	if !miniagent.MessagesUseTools(transcript) {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, memoryExtractTimeout)
	defer cancel()

	existing, _ := miniagent.ReadMemoryRecords(m.workdir)
	recs, _, err := miniagent.ExtractMemory(ctx, m.client, m.model, m.prompt, m.maxK, existing, m.secrets, transcript)
	if err != nil {
		if m.logger != nil {
			m.logger.Warn("memory extract failed", "error", err)
		}
		return
	}
	for _, r := range recs {
		if err := miniagent.AppendMemoryRecord(m.workdir, r); err != nil {
			if m.logger != nil {
				m.logger.Warn("memory append failed", "error", err)
			}
		}
	}
}
