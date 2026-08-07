package miniagent

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// toolOutputRetentionDefault 是落盘工具输出的默认保留时长（取自 opencode RETENTION=7d）。
const toolOutputRetentionDefault = 7 * 24 * time.Hour

// toolOutputMaxBytes 是单个落盘文件的字节硬封顶（防 OOM；高于 shell 入口 100KB 上限，留余量）。
const toolOutputMaxBytes = 1 << 20 // 1 MiB

// toolOutputStore 是工具输出落盘存储，私有；由 Run 在入口构造一次、跨步复用。超 limit 的工具全文
// 写盘，历史 Content 改为「现有预览 + 绝对路径提示」，模型用已有 read(offset/limit)/grep 按需回读，
// 不再永久丢全文（§P1-A，修 trimForHistory 一次性硬截断的硬损口）。
type toolOutputStore struct {
	dir       string        // 落盘根目录；构造时已确保非空
	retention time.Duration // 过期清理阈值；<=0 用 toolOutputRetentionDefault
	counter   atomic.Int64  // 同 (step,callID) 碰撞消解
	logger    *slog.Logger
}

// newToolOutputStore 是 Run 入口构造。retention<=0 落 toolOutputRetentionDefault。dir 由调用方判空。
func newToolOutputStore(dir string, retention time.Duration, logger *slog.Logger) *toolOutputStore {
	if retention <= 0 {
		retention = toolOutputRetentionDefault
	}
	return &toolOutputStore{dir: dir, retention: retention, logger: logger}
}

// bound 是核心方法。truncated=false 原样返回 preview（无落盘）。truncated=true：把 output（按字节
// cap 到 toolOutputMaxBytes）写入 <dir>/tool_<step>_<sanitize(callID)>_<counter>.txt（O_CREATE|O_EXCL
// 0o600，仿 session.go 写侧护栏；失败 best-effort log warn 并返回 preview 不带 marker，等同当前硬截断），
// 成功返回 preview + 路径提示（模型用 read(offset/limit)/grep 回读）。
func (s *toolOutputStore) bound(step int, callID, output, preview string, truncated bool) string {
	if !truncated {
		return preview
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		s.warnf("tool-output mkdir failed: %v", err)
		return preview
	}
	name := fmt.Sprintf("tool_%d_%s_%d.txt", step, sanitizeFileSegment(callID), s.counter.Add(1))
	path := filepath.Join(s.dir, name)
	data := []byte(output)
	byteCapped := false
	if len(data) > toolOutputMaxBytes {
		// 超 1 MiB 硬封顶：回退到最近的 UTF-8 rune 边界再截，保证落盘文件始终是合法 UTF-8
		// （否则多字节 rune 被从中间切开，模型 read 回读得乱码，回灌 API 可能引发 400）。
		byteCapped = true
		end := toolOutputMaxBytes
		for end > 0 && !utf8.RuneStart(data[end]) {
			end--
		}
		data = data[:end]
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		s.warnf("tool-output create failed: %v", err)
		return preview
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		s.warnf("tool-output write failed: %v", err)
		return preview
	}
	// Sync 兑现下方 marker「完整输出已保存」的语义：崩溃后模型 read 回读得完整数据而非空/残文件。
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		s.warnf("tool-output sync failed: %v", err)
		return preview
	}
	if err := f.Close(); err != nil {
		// 对齐写失败语义：close 失败则删孤儿文件、返回 preview 不带 marker，避免模型据 marker 回读到残文件。
		_ = os.Remove(path)
		s.warnf("tool-output close failed: %v", err)
		return preview
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	// 落盘文件可能被 1 MiB 字节封顶截断（byteCapped）——此时不可声称「完整」，否则模型 read 回读
	// 得被截断的数据却以为是全文。
	marker := "…[完整输出已保存"
	if byteCapped {
		marker = "…[输出已保存（超 1 MiB 部分已截断）"
	}
	return preview + "\n\n" + marker + "：" + abs + "；用 read(offset/limit) 或 grep 回读，勿整文件 read]"
}

// cleanup 扫描 dir 下 tool_*.txt，对 mtime < now-retention 的 os.Remove（best-effort，失败仅 warn）。
// Run 启动时机会性调用一次，贴合单次运行 CLI 形态（不做后台定时器）。
func (s *toolOutputStore) cleanup() {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return // 目录不存在等：无落盘文件可清，静默。
	}
	cutoff := time.Now().Add(-s.retention)
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "tool_") || !strings.HasSuffix(e.Name(), ".txt") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			if err := os.Remove(filepath.Join(s.dir, e.Name())); err != nil && s.logger != nil {
				s.logger.Warn("tool-output cleanup remove failed", "file", e.Name(), "error", err)
			}
		}
	}
}

// sanitizeFileSegment 把 callID 压成文件名安全段：保留 [A-Za-z0-9_-]，其余替换为 '_'，
// 截断到 ≤32 字节（b.Len() 计字节，非 rune；多字节 rune 可使段长略超 32B，counter 后缀消解碰撞）。
// 防路径穿越（剔 / .. 等）。
func sanitizeFileSegment(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
		if b.Len() >= 32 {
			break
		}
	}
	return b.String()
}

func (s *toolOutputStore) warnf(format string, args ...any) {
	if s.logger != nil {
		s.logger.Warn(fmt.Sprintf(format, args...))
	}
}
