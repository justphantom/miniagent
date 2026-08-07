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
)

// maxSessionBytes 是 session 文件默认大小上限：50MB 覆盖长会话，同时防止无限增长。
// 运行时经 Limits.MaxSessionBytes 覆盖（<=0 用此默认），由 LoadSession/AppendMessages/RewriteMessages 注入。
const maxSessionBytes = 50 << 20 // 50MB

const (
	sessionTypeSession = "session"
	sessionTypeMessage = "message"
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

// ValidateSessionID 白名单校验 id：仅允许拉丁字母、数字、连字符。禁路径分隔符/点/空格等，
// 使 id 只作文件名主体（.jsonl 扩展名由 ResolveSessionPath 补），杜绝路径穿越与扩展名注入。
func ValidateSessionID(id string) error {
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-':
		default:
			return fmt.Errorf("session id %q 含非法字符 %q（仅允许拉丁字母、数字、-）", id, r)
		}
	}
	return nil
}

// ResolveSessionPath 校验 id（白名单）后拼 {dir}/{id}.jsonl。仅解析路径，不判文件存在性——
// 新建（-save-session）与接续（-session）的存在性语义由调用方裁决（resolveSessionForRun）。
func ResolveSessionPath(arg, dir string) (string, error) {
	if arg == "" {
		return "", errors.New("session 参数为空")
	}
	if err := ValidateSessionID(arg); err != nil {
		return "", err
	}
	if dir == "" {
		return "", fmt.Errorf("session %q 有效但未配置 session.dir", arg)
	}
	return filepath.Join(dir, arg+".jsonl"), nil
}

// LoadSession 读取 jsonl：首行 session metadata（若无则零值 meta），其余为 message 行。
// 文件不存在返回 (零 meta, nil, nil) 等同新会话。损坏（非法 JSON 行、role 未知、tool 消息
// 缺 tool_call_id、配对断裂、超大小上限）返回 error，调用方应报错退出而非静默丢历史。
// opts：opts[0] 覆盖 maxSessionBytes 上限（<=0 或缺省回落 maxSessionBytes 常量）。
func LoadSession(path string, opts ...int64) (SessionMeta, []Message, error) {
	mb := int64(maxSessionBytes)
	if len(opts) > 0 && opts[0] > 0 {
		mb = opts[0]
	}
	f, err := openNoFollow(path, os.O_RDONLY, 0)
	if errors.Is(err, os.ErrNotExist) {
		return SessionMeta{}, nil, nil
	}
	if err != nil {
		return SessionMeta{}, nil, err
	}
	defer func() { _ = f.Close() }()
	// 单次 open + LimitReader：消除 Stat/ReadFile 间 TOCTOU，并硬封顶读取量防撑爆内存。
	data, err := io.ReadAll(io.LimitReader(f, mb+1))
	if err != nil {
		return SessionMeta{}, nil, err
	}
	if int64(len(data)) > mb {
		return SessionMeta{}, nil, fmt.Errorf("session 文件 %q 超过大小上限 %d 字节", path, mb)
	}
	var meta SessionMeta
	var msgs []Message
	sc := bufio.NewScanner(bytes.NewReader(data))
	// 单行上限对齐 mb：避免单条大消息触发 ErrTooLong 致整会话不可读、append-only 无法修复（P2-7）。
	sc.Buffer(make([]byte, 64*1024), int(mb+1))
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
		//nolint:musttag // Message 已有 json tag；sessionLine 嵌入 Message 是 session 文件格式约定（非 wire）
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
	if err := ValidateToolPairing(msgs); err != nil {
		return SessionMeta{}, nil, fmt.Errorf("session 文件 %q：%w", path, err)
	}
	return meta, msgs, nil
}

func validateSessionMessage(m Message) error {
	switch m.Role {
	case RoleUser, RoleAssistant:
		return nil
	case RoleTool:
		if m.ToolCallID == "" {
			return errors.New("tool 消息缺少 tool_call_id")
		}
		return nil
	default:
		return fmt.Errorf("未知 role %q", m.Role)
	}
}

// validateToolPairing 校验 assistant.tool_calls 与 tool 消息一一配对；断裂会被端点 400，提前拦截指明位置。
func ValidateToolPairing(msgs []Message) error {
	pending := map[string]bool{}
	for i, m := range msgs {
		switch m.Role {
		case RoleAssistant:
			for _, tc := range m.ToolCalls {
				if pending[tc.ID] {
					return fmt.Errorf("第 %d 条：tool_call id %q 重复", i+1, tc.ID)
				}
				pending[tc.ID] = true
			}
		case RoleTool:
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
// opts：opts[0] 覆盖 maxSessionBytes 上限（<=0 或缺省回落 maxSessionBytes 常量）。
func AppendMessages(path string, meta SessionMeta, msgs []Message, opts ...int64) error {
	mb := int64(maxSessionBytes)
	if len(opts) > 0 && opts[0] > 0 {
		mb = opts[0]
	}
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
			//nolint:musttag // sessionLine 嵌入 Message 是 session 文件格式约定（非 wire）
			b, err := json.Marshal(sessionLine{Type: sessionTypeMessage, Message: m})
			if err != nil {
				return err
			}
			buf.Write(b)
			buf.WriteByte('\n')
		}
		if info.Size()+int64(buf.Len()) > mb {
			return fmt.Errorf("session 文件 %q 追加后将达 %d 字节，超上限 %d（请压缩历史或新建会话）", path, info.Size()+int64(buf.Len()), mb)
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
// opts：opts[0] 覆盖 maxSessionBytes 上限（<=0 或缺省回落 maxSessionBytes 常量）。
func RewriteMessages(path string, meta SessionMeta, msgs []Message, opts ...int64) error {
	mb := int64(maxSessionBytes)
	if len(opts) > 0 && opts[0] > 0 {
		mb = opts[0]
	}
	var buf bytes.Buffer
	if meta.Type == "" {
		meta.Type = sessionTypeSession
	}
	b, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	buf.Write(b)
	buf.WriteByte('\n')
	for _, m := range msgs {
		//nolint:musttag // sessionLine 嵌入 Message 是 session 文件格式约定（非 wire）
		b, err := json.Marshal(sessionLine{Type: sessionTypeMessage, Message: m})
		if err != nil {
			return err
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	if int64(buf.Len()) > mb {
		return fmt.Errorf("session rewrite 后 %d 字节超上限 %d", buf.Len(), mb)
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
		// rename 失败（权限/磁盘满/文件系统异常）同样清理 tmp，与 write/sync 失败一致（注释承诺"write/rename 失败都清理"）。
		if err := os.Rename(tmpPath, path); err != nil {
			_ = os.Remove(tmpPath)
			return err
		}
		// fsync 父目录提交 rename 的目录元数据：防崩溃落在 rename 与目录元数据提交窗口内致 rewrite 丢失
		// （与已披露的 flock+rename 并发问题是不同维度——这是崩溃耐久性）。失败仅 best-effort（rename 已成功）。
		if d, derr := os.Open(dir); derr == nil {
			_ = d.Sync()
			_ = d.Close()
		}
		return nil
	})
}
