package miniagent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
)

const (
	memoryDir          = ".miniagent"
	memoryFile         = "memory.jsonl"
	memoryRecentN      = 10 // system prompt 注入的最近记忆条数；可通过 SetMemoryRecentN 覆盖
	memoryPathToken    = "memory"
	maxMemoryFileBytes = 1 << 20 // 1 MiB；超限后丢弃旧记录轮转
)

// memoryRecentNOverride 允许配置覆盖内置默认；nil 用常量默认。
// 用 atomic 保护并发 Set/Get，防 -race 检测告警。
var memoryRecentNOverride atomic.Int64

// SetMemoryRecentN 覆盖记忆注入条数；测试用，正常流程由 Resolve 调用。
func SetMemoryRecentN(n int) {
	if n > 0 {
		memoryRecentNOverride.Store(int64(n))
	}
}

func getMemoryRecentN() int {
	if v := memoryRecentNOverride.Load(); v > 0 {
		return int(v)
	}
	return memoryRecentN
}

// memoryRecord 是 .miniagent/memory.jsonl 的一条结构化记录（P5 项目级记忆）。
type memoryRecord struct {
	Type    string `json:"type"`
	Topic   string `json:"topic,omitempty"`
	Content string `json:"content"`
}

// memoryPath 返回 <workdir>/.miniagent/memory.jsonl。
func memoryPath(workdir string) string {
	return filepath.Join(workdir, memoryDir, memoryFile)
}

// checkMemoryPathSafe 校验 .miniagent/memory.jsonl 路径安全：父目录与文件自身
// 不得为符号链接，文件（若存在）须为普通文件；非空 workdir 时路径还须落在 workdir 子树内。
// 该检查在 IO 前执行，可缩小 TOCTOU 窗口，但非原子隔离（default 模式本就不是安全边界）。
func checkMemoryPathSafe(workdir string) error {
	path := memoryPath(workdir)
	dir := filepath.Dir(path)

	// 父目录存在时必须是真实目录（非软链）
	if info, err := os.Lstat(dir); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf(".miniagent 目录 %q 是符号链接", dir)
		}
		if !info.Mode().IsDir() {
			return fmt.Errorf(".miniagent 路径 %q 不是目录", dir)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("检查 .miniagent 目录 %q 失败：%w", dir, err)
	}

	// 目标文件存在时必须是普通文件（非软链/设备/FIFO）
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("memory 文件 %q 是符号链接", path)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("memory 文件 %q 不是普通文件", path)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("检查 memory 文件 %q 失败：%w", path, err)
	}

	// 非空 workdir 时路径须落在 workdir 子树内
	if workdir != "" {
		absPath, err := filepath.Abs(filepath.Clean(path))
		if err != nil {
			return fmt.Errorf("解析 memory 路径 %q 失败：%w", path, err)
		}
		absWorkdir, err := filepath.Abs(filepath.Clean(workdir))
		if err != nil {
			return fmt.Errorf("解析 workdir %q 失败：%w", workdir, err)
		}
		sep := string(filepath.Separator)
		if absPath != absWorkdir && !strings.HasPrefix(absPath+sep, absWorkdir+sep) {
			return fmt.Errorf("memory 路径 %q 越出 workdir %q", path, workdir)
		}
	}
	return nil
}

// readMemoryRecordsFromPath 从指定路径读 memory.jsonl（文件不存在返回 nil 无错；非法行跳过）。
// 使用 O_NOFOLLOW 打开，防止目标文件或父目录被替换为符号链接后读到/写到预期外位置。
func readMemoryRecordsFromPath(path string) ([]memoryRecord, error) {
	f, err := openNoFollow(path, os.O_RDONLY, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, int64(readFileBytes())))
	if err != nil {
		return nil, err
	}
	var recs []memoryRecord
	for line := range strings.SplitSeq(strings.TrimRight(string(data), "\n"), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var r memoryRecord
		if err := json.Unmarshal([]byte(line), &r); err != nil {
			continue // 容忍个别非法行，不阻塞读取
		}
		recs = append(recs, r)
	}
	return recs, nil
}

// readMemoryRecords 读 memory.jsonl（文件不存在返回 nil 无错；非法行跳过）。
func readMemoryRecords(workdir string) ([]memoryRecord, error) {
	return readMemoryRecordsFromPath(memoryPath(workdir))
}

// appendMemoryRecord 追加一条记录到 memory.jsonl（MkdirAll .miniagent，0o600）。
// 写入前后均校验路径安全，降低符号链接 TOCTOU 风险。
func appendMemoryRecord(workdir string, r memoryRecord) error {
	if err := checkMemoryPathSafe(workdir); err != nil {
		return err
	}
	path := memoryPath(workdir)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("创建 .miniagent 目录：%w", err)
	}
	// MkdirAll 后再次确认父目录未被替换为符号链接（缩小 TOCTOU 窗口）。
	if err := checkMemoryPathSafe(workdir); err != nil {
		return err
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	line := append(b, '\n')
	// 超限轮转：保留最近一半记录，避免无限增长。
	if info, err := os.Lstat(path); err == nil && info.Size()+int64(len(line)) > maxMemoryFileBytes {
		if rerr := rotateMemoryFile(path, line); rerr != nil {
			return fmt.Errorf("memory 文件轮转失败：%w", rerr)
		}
		return nil
	}
	f, err := openNoFollow(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write(line); err != nil {
		return err
	}
	return f.Sync()
}

// rotateMemoryFile 读取现有记录，保留最近一半并追加新行，原子重写文件。
func rotateMemoryFile(path string, newLine []byte) error {
	recs, err := readMemoryRecordsFromPath(path)
	if err != nil {
		return err
	}
	keep := len(recs) / 2
	keep = max(keep, 1)
	recs = recs[len(recs)-keep:]
	var buf bytes.Buffer
	for _, rec := range recs {
		b, err := json.Marshal(rec)
		if err != nil {
			return err
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	buf.Write(newLine)
	if int64(buf.Len()) > maxMemoryFileBytes {
		return fmt.Errorf("单条 memory 记录 %d 字节超过文件上限 %d", len(newLine), maxMemoryFileBytes)
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	// CreateTemp 默认 0600，已是 memory.jsonl 所需权限，无需再 Chmod；rename 保留该权限。
	if _, err := tmp.Write(buf.Bytes()); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return os.Rename(tmpPath, path)
}
