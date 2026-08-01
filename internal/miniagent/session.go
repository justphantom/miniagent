package miniagent

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const maxSessionBytes = 4 << 20

const (
	sessionTypeSession = "session"
	sessionTypeMessage = "message"
	// KindSummary 标记 summary 消息：结构化识别（applyCompactionBarrier 用），替代脆弱的
	// 内容前缀嗅探（审查 v3 #2）。role=user 合法可持久化。
	KindSummary = "summary"
)

// SessionMeta 是 jsonl 首行 metadata（type=session），便于会话列举与多 provider 溯源。
type SessionMeta struct {
	Type     string `json:"type"`
	ID       string `json:"id"`
	Model    string `json:"model"`
	Workdir  string `json:"workdir"`
	Provider string `json:"provider"`
	Created  string `json:"created"`
}

// sessionLine 是 message 行的写入包装：嵌入 Message 提升 role/content/kind 等字段，
// 并补 type=message 判别（读侧按 type 分流到 SessionMeta 或 Message）。
type sessionLine struct {
	Type string `json:"type"`
	Message
}

// ResolveSessionPath 解析 -session 入参：含路径分隔符或「.」或绝对路径 → 视为路径（双语义，
// 向后兼容老的 .json/.jsonl 路径）；纯 id → {dir}/{id}.jsonl。dir 空且是 id 时报错。
func ResolveSessionPath(arg, dir string) (string, error) {
	if arg == "" {
		return "", errors.New("session 参数为空")
	}
	if filepath.IsAbs(arg) || strings.ContainsAny(arg, "/."+string(filepath.Separator)) {
		return arg, nil
	}
	if dir == "" {
		return "", fmt.Errorf("session %q 是 id 但未配置 session.dir（或传完整路径）", arg)
	}
	return filepath.Join(dir, arg+".jsonl"), nil
}

// LoadSession 读取 jsonl：首行 session metadata（若无则零值 meta），其余为 message 行。
// 文件不存在返回 (零 meta, nil, nil) 等同新会话。损坏（非法 JSON 行、role 未知、tool 消息
// 缺 tool_call_id、配对断裂、超大小上限）返回 error，调用方应报错退出而非静默丢历史。
func LoadSession(path string) (SessionMeta, []Message, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return SessionMeta{}, nil, nil
	}
	if err != nil {
		return SessionMeta{}, nil, err
	}
	defer func() { _ = f.Close() }()
	// 单次 open + LimitReader：消除 Stat/ReadFile 间 TOCTOU，并硬封顶读取量防撑爆内存。
	data, err := io.ReadAll(io.LimitReader(f, maxSessionBytes+1))
	if err != nil {
		return SessionMeta{}, nil, err
	}
	if int64(len(data)) > maxSessionBytes {
		return SessionMeta{}, nil, fmt.Errorf("session 文件 %q 超过大小上限 %d 字节", path, maxSessionBytes)
	}
	var meta SessionMeta
	var msgs []Message
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	var corruptLine int // 挂起的非法 JSON 行号（1-based），0=无
	var corruptErr error
	for i := 0; sc.Scan(); i++ {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var probe struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(line, &probe); err != nil {
			// 非法 JSON 行：append-only 下崩溃仅污染最后写入的行。先挂起而非立即报错，
			// 待确认是否尾行（见循环后 corruptLine 处置）。若此前已有挂起行 → 中间损坏。
			if corruptLine != 0 {
				return SessionMeta{}, nil, fmt.Errorf("session 文件 %q 第 %d 行非法 JSON：%w", path, corruptLine, corruptErr)
			}
			corruptLine = i + 1
			corruptErr = err
			continue
		}
		// 本行合法：若存在挂起的非法行，它不在文件末尾 → 中间损坏，严格报错。
		if corruptLine != 0 {
			return SessionMeta{}, nil, fmt.Errorf("session 文件 %q 第 %d 行非法 JSON（中间损坏）：%w", path, corruptLine, corruptErr)
		}
		if probe.Type == sessionTypeSession {
			if err := json.Unmarshal(line, &meta); err != nil {
				return SessionMeta{}, nil, fmt.Errorf("session 文件 %q metadata 行解析失败：%w", path, err)
			}
			continue
		}
		// message 行（type=message 或历史无 type）：反序列化进 Message，未知字段忽略。
		var m Message
		if err := json.Unmarshal(line, &m); err != nil {
			return SessionMeta{}, nil, fmt.Errorf("session 文件 %q 第 %d 行解析失败：%w", path, i+1, err)
		}
		if err := validateSessionMessage(m); err != nil {
			return SessionMeta{}, nil, fmt.Errorf("session 文件 %q 第 %d 条消息非法：%w", path, i+1, err)
		}
		msgs = append(msgs, m)
	}
	if err := sc.Err(); err != nil {
		return SessionMeta{}, nil, fmt.Errorf("session 文件 %q 读取失败：%w", path, err)
	}
	// 扫描结束：corruptLine 仍非 0 → 最后一行半写（append-only 崩溃残行），容忍丢弃，
	// 返回此前合法的历史。validateToolPairing 仍严格执行：若残行致配对断裂则报清晰错误。
	if err := validateToolPairing(msgs); err != nil {
		return SessionMeta{}, nil, fmt.Errorf("session 文件 %q：%w", path, err)
	}
	return meta, msgs, nil
}

func validateSessionMessage(m Message) error {
	switch m.Role {
	case roleUser, roleAssistant:
		return nil
	case roleTool:
		if m.ToolCallID == "" {
			return errors.New("tool 消息缺少 tool_call_id")
		}
		return nil
	default:
		return fmt.Errorf("未知 role %q", m.Role)
	}
}

// validateToolPairing 校验 assistant.tool_calls 与 tool 消息的一一配对。配对断裂会被
// OpenAI 兼容端点 400，且报错指向 LLM 端而非 session 文件，这里提前拦截并指明位置。
func validateToolPairing(msgs []Message) error {
	pending := map[string]bool{}
	for i, m := range msgs {
		switch m.Role {
		case roleAssistant:
			for _, tc := range m.ToolCalls {
				if pending[tc.ID] {
					return fmt.Errorf("第 %d 条：tool_call id %q 重复", i, tc.ID)
				}
				pending[tc.ID] = true
			}
		case roleTool:
			if !pending[m.ToolCallID] {
				return fmt.Errorf("第 %d 条：tool 消息的 tool_call_id %q 没有对应的 assistant tool_call", i, m.ToolCallID)
			}
			delete(pending, m.ToolCallID)
		}
	}
	if len(pending) > 0 {
		return fmt.Errorf("%d 个 assistant tool_call 缺少对应 tool 结果", len(pending))
	}
	return nil
}

// AppendMessages append-only 追加 msgs 到 jsonl：文件新建/空时先写 metadata 行，再写每条
// message 行，Flush 后 f.Sync 落盘缩小崩溃残行窗口（LoadSession 兜底容忍尾行半写）。
// 失败轮由调用方决定是否落盘（main 仅成功轮调用）。权限 0o600（对话属敏感数据）。
func AppendMessages(path string, meta SessionMeta, msgs []Message) error {
	if len(msgs) == 0 {
		return nil
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("创建 session 目录：%w", err)
		}
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	if info.Size() == 0 {
		if meta.Type == "" {
			meta.Type = sessionTypeSession
		}
		b, err := json.Marshal(meta)
		if err != nil {
			return err
		}
		if _, err := w.Write(b); err != nil {
			return err
		}
		if err := w.WriteByte('\n'); err != nil {
			return err
		}
	}
	for _, m := range msgs {
		b, err := json.Marshal(sessionLine{Type: sessionTypeMessage, Message: m})
		if err != nil {
			return err
		}
		if _, err := w.Write(b); err != nil {
			return err
		}
		if err := w.WriteByte('\n'); err != nil {
			return err
		}
	}
	if err := w.Flush(); err != nil {
		return err
	}
	// Sync 落盘：把已 Flush 到内核缓冲的数据 fsync 到磁盘，缩小「已写残行 + 未落盘」
	// 的崩溃窗口（配合 LoadSession 的尾行容忍，见该函数 corruptLine 逻辑）。
	return f.Sync()
}

// MigrateSession 把 v2 的 JSON 数组 session（[]Message）转为 jsonl：写到 {dstDir}/{base}.jsonl，
// metadata 仅含 id（base 名）+ type，无 summary kind（纯历史）。返回写入路径。
func MigrateSession(srcPath, dstDir string) (string, error) {
	msgs, err := loadLegacySession(srcPath)
	if err != nil {
		return "", err
	}
	base := strings.TrimSuffix(filepath.Base(srcPath), filepath.Ext(srcPath))
	dst := filepath.Join(dstDir, base+".jsonl")
	meta := SessionMeta{Type: sessionTypeSession, ID: base}
	if err := AppendMessages(dst, meta, msgs); err != nil {
		return "", err
	}
	return dst, nil
}

// loadLegacySession 读 v2 JSON 数组 session（向后兼容迁移用）。
func loadLegacySession(path string) ([]Message, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读 legacy session %q: %w", path, err)
	}
	var msgs []Message
	if err := json.Unmarshal(data, &msgs); err != nil {
		return nil, fmt.Errorf("legacy session %q 不是合法 JSON 数组：%w", path, err)
	}
	for i, m := range msgs {
		if err := validateSessionMessage(m); err != nil {
			return nil, fmt.Errorf("legacy session %q 第 %d 条消息非法：%w", path, i, err)
		}
	}
	if err := validateToolPairing(msgs); err != nil {
		return nil, fmt.Errorf("legacy session %q：%w", path, err)
	}
	return msgs, nil
}
