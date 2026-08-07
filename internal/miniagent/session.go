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

// AppendMessages append-only 追加 msgs 到 jsonl（新建/空时先写 metadata 行）。写侧护栏：flock
// 跨进程锁防行边界交织非法 JSON（P2-13）；预序列化按 size+待写 超限拒绝，避免写入成功延后失败到
// LoadSession 致永久卡死（P1-4）；写前 ensureTrailingNewline 截断崩溃半写残行（H3-1）。withSessionLock
// 统一 O_NOFOLLOW + MkdirAll 0o700 + flock（P3）。opts：opts[0] 覆盖 maxSessionBytes 上限（<=0 或缺省回落常量）。
func AppendMessages(path string, meta SessionMeta, msgs []Message, opts ...int64) error {
	mb := int64(maxSessionBytes)
	if len(opts) > 0 && opts[0] > 0 {
		mb = opts[0]
	}
	if len(msgs) == 0 {
		return nil
	}
	// O_RDWR：写前需读末字节检测并截断崩溃半写残留的尾部不完整行（ensureTrailingNewline）。
	return withSessionLock(path, os.O_APPEND|os.O_CREATE|os.O_RDWR, func(f *os.File) error {
		info, err := f.Stat()
		if err != nil {
			return err
		}
		// 已超 mb 的文件不 heal：ensureTrailingNewline 慢路径 LimitReader(mb+1) 在 size>mb 时只读前
		// mb+1 字节、LastIndexByte 在不完整窗口错位，会截断丢弃合法行且无回滚（R4-1）。与 LoadSession
		// 一致——>mb 直接报错、零修改文件。
		if info.Size() > mb {
			return fmt.Errorf("session 文件 %q 已达 %d 字节超上限 %d（请压缩历史或新建会话）", path, info.Size(), mb)
		}
		// 截断崩溃半写残留的尾部不完整行：否则 O_APPEND 盲写把新消息拼到无换行结尾的残行上，
		// 使原本被 LoadSession 末行容忍的残行在后续保存反噬为中段损坏（永久丢会话）。
		size, err := ensureTrailingNewline(f, info.Size(), mb)
		if err != nil {
			return err
		}
		// 预序列化待写内容：既精确估算大小做写侧预判，又复用一次 marshal 避免重复劳动。
		var buf bytes.Buffer
		if size == 0 {
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
		if size+int64(buf.Len()) > mb {
			return fmt.Errorf("session 文件 %q 追加后将达 %d 字节，超上限 %d（请压缩历史或新建会话）", path, size+int64(buf.Len()), mb)
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

// ensureTrailingNewline 截断崩溃半写残留的尾部不完整行：O_APPEND 盲写会把新消息拼到无换行结尾的
// 残行上、破坏行边界。快路径只读末字节；仅当文件不以 '\n' 结尾（罕见恢复场景）才回扫到最后一个
// '\n' 并截断其后字节，返回截断后的文件大小供调用方判断是否需补写 metadata 头行。
func ensureTrailingNewline(f *os.File, size, mb int64) (int64, error) {
	if size == 0 {
		return 0, nil
	}
	var last [1]byte
	if _, err := f.ReadAt(last[:], size-1); err != nil {
		return size, err
	}
	if last[0] == '\n' {
		return size, nil
	}
	// 残行无换行结尾：读现有内容（上限 mb）定位最后一个 '\n' 并截断其后字节。
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return size, err
	}
	data, err := io.ReadAll(io.LimitReader(f, mb+1))
	if err != nil {
		return size, err
	}
	cutAt := int64(0)
	if idx := bytes.LastIndexByte(data, '\n'); idx >= 0 {
		cutAt = int64(idx) + 1
	}
	if err := f.Truncate(cutAt); err != nil {
		return size, err
	}
	return cutAt, nil
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
