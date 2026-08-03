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

// maxSessionBytes 是 session 文件默认大小上限：50MB 覆盖长会话，同时防止无限增长。
// 可通过 SetMaxSessionBytes 覆盖。n<=0 用默认。
const maxSessionBytes = 50 << 20 // 50MB

// maxSessionBytesOverride 允许测试/配置覆盖内置上限；nil 用常量默认。
var maxSessionBytesOverride int

// SetMaxSessionBytes 覆盖 session 文件大小上限；测试用，正常流程由 Resolve 调用。
func SetMaxSessionBytes(n int) {
	if n > 0 {
		maxSessionBytesOverride = n
	}
}

func sessionBytes() int64 {
	if maxSessionBytesOverride > 0 {
		return int64(maxSessionBytesOverride)
	}
	return int64(maxSessionBytes)
}

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

// ResolveSessionPath 解析 -session：含路径分隔符/「.」/绝对路径→视为路径（双语义兼容老 .json/.jsonl）；纯 id → {dir}/{id}.jsonl。dir 空且是 id 报错。
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
	f, err := openNoFollow(path, os.O_RDONLY, 0)
	if errors.Is(err, os.ErrNotExist) {
		return SessionMeta{}, nil, nil
	}
	if err != nil {
		return SessionMeta{}, nil, err
	}
	defer func() { _ = f.Close() }()
	// 单次 open + LimitReader：消除 Stat/ReadFile 间 TOCTOU，并硬封顶读取量防撑爆内存。
	data, err := io.ReadAll(io.LimitReader(f, sessionBytes()+1))
	if err != nil {
		return SessionMeta{}, nil, err
	}
	if int64(len(data)) > sessionBytes() {
		return SessionMeta{}, nil, fmt.Errorf("session 文件 %q 超过大小上限 %d 字节", path, sessionBytes())
	}
	var meta SessionMeta
	var msgs []Message
	sc := bufio.NewScanner(bytes.NewReader(data))
	// 单行上限对齐 sessionBytes()：避免单条大消息触发 ErrTooLong 致整会话不可读、append-only 无法修复（P2-7）。
	sc.Buffer(make([]byte, 64*1024), int(sessionBytes()+1))
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
			// 非法 JSON 行：append-only 崩溃仅污染末行，先挂起待确认是否尾行；此前已有挂起行则中间损坏。
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

// validateToolPairing 校验 assistant.tool_calls 与 tool 消息一一配对；断裂会被端点 400，提前拦截指明位置。
func validateToolPairing(msgs []Message) error {
	pending := map[string]bool{}
	for i, m := range msgs {
		switch m.Role {
		case roleAssistant:
			for _, tc := range m.ToolCalls {
				if pending[tc.ID] {
					return fmt.Errorf("第 %d 条：tool_call id %q 重复", i+1, tc.ID)
				}
				pending[tc.ID] = true
			}
		case roleTool:
			if !pending[m.ToolCallID] {
				return fmt.Errorf("第 %d 条：tool 消息的 tool_call_id %q 没有对应的 assistant tool_call", i+1, m.ToolCallID)
			}
			delete(pending, m.ToolCallID)
		}
	}
	if len(pending) > 0 {
		return fmt.Errorf("%d 个 assistant tool_call 缺少对应 tool 结果", len(pending))
	}
	return nil
}

// AppendMessages append-only 追加 msgs 到 jsonl（新建/空时先写 metadata 行）。写侧护栏：flock
// 跨进程锁防行边界交织非法 JSON（P2-13）；预序列化按 info.Size()+待写 超限拒绝，避免写入成功
// 延后失败到 LoadSession 致永久卡死（P1-4）。withSessionLock 统一 O_NOFOLLOW + MkdirAll 0o700 + flock（P3）。
func AppendMessages(path string, meta SessionMeta, msgs []Message) error {
	if len(msgs) == 0 {
		return nil
	}
	return withSessionLock(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, func(f *os.File) error {
		info, err := f.Stat()
		if err != nil {
			return err
		}
		// 预序列化待写内容：既精确估算大小做写侧预判，又复用一次 marshal 避免重复劳动。
		var buf bytes.Buffer
		if info.Size() == 0 {
			if meta.Type == "" {
				meta.Type = sessionTypeSession
			}
			b, err := json.Marshal(meta)
			if err != nil {
				return err
			}
			buf.Write(b)
			buf.WriteByte('\n')
		}
		for _, m := range msgs {
			b, err := json.Marshal(sessionLine{Type: sessionTypeMessage, Message: m})
			if err != nil {
				return err
			}
			buf.Write(b)
			buf.WriteByte('\n')
		}
		if info.Size()+int64(buf.Len()) > sessionBytes() {
			return fmt.Errorf("session 文件 %q 追加后将达 %d 字节，超上限 %d（请压缩历史或新建会话）", path, info.Size()+int64(buf.Len()), sessionBytes())
		}
		w := bufio.NewWriter(f)
		if _, err := w.Write(buf.Bytes()); err != nil {
			return err
		}
		if err := w.Flush(); err != nil {
			return err
		}
		// Sync 落盘缩小「已写残行 + 未落盘」崩溃窗口（配合 LoadSession 尾行容忍）。
		return f.Sync()
	})
}

// RewriteMessages 全量重写 session 文件（写临时文件 → os.Rename 原子替换）。仅 Run 成功且
// result.Compacted 时调用：append-only 落盘的 newMsgs 含被屏障的旧 summary 与被压中段，长会话
// 需 rewrite 真正丢弃（审查 P2 session 文件永不压缩）。msgs 是全量 transcript；锁与临时文件策略
// 见 withSessionLock；write/rename 失败都清理临时文件。rename 后下轮 LoadSession 读精简文件。
func RewriteMessages(path string, meta SessionMeta, msgs []Message) error {
	var buf bytes.Buffer
	if meta.Type == "" {
		meta.Type = sessionTypeSession
	}
	mb, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	buf.Write(mb)
	buf.WriteByte('\n')
	for _, m := range msgs {
		b, err := json.Marshal(sessionLine{Type: sessionTypeMessage, Message: m})
		if err != nil {
			return err
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	if int64(buf.Len()) > sessionBytes() {
		return fmt.Errorf("session rewrite 后 %d 字节超上限 %d", buf.Len(), sessionBytes())
	}
	dir := filepath.Dir(path)
	return withSessionLock(path, os.O_WRONLY|os.O_CREATE, func(*os.File) error {
		// 临时文件与 path 同目录（保证 rename 同文件系统原子）；os.CreateTemp 默认 0o600。
		tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
		if err != nil {
			return err
		}
		tmpPath := tmp.Name()
		_, writeErr := tmp.Write(buf.Bytes())
		if writeErr == nil {
			writeErr = tmp.Sync()
		}
		_ = tmp.Close()
		if writeErr != nil {
			_ = os.Remove(tmpPath)
			return writeErr
		}
		// rename 原子替换 path：withSessionLock 持的 fd 此刻指向 unlinked 旧 inode，defer unlock/close 仍正确（fd 关闭释放锁）；下轮拿新 inode 的锁。
		return os.Rename(tmpPath, path)
	})
}
