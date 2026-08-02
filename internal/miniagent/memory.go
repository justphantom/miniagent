package miniagent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	memoryDir     = ".miniagent"
	memoryFile    = "memory.jsonl"
	memoryRecentN = 10 // system prompt 注入的最近记忆条数；可通过 SetMemoryRecentN 覆盖
	// memoryPathToken 是 read/write 工具识别项目记忆的保留路径：path=="memory" 时
	// 路由到 <workdir>/.miniagent/memory.jsonl（read 渲染记录、write 追加记录），不走普通文件路径。
	memoryPathToken = "memory"
)

// memoryRecentNOverride 允许配置覆盖内置默认；nil 用常量默认。
var memoryRecentNOverride int

// SetMemoryRecentN 覆盖记忆注入条数；测试用，正常流程由 Resolve 调用。
func SetMemoryRecentN(n int) { if n > 0 { memoryRecentNOverride = n } }

func getMemoryRecentN() int {
	if memoryRecentNOverride > 0 {
		return memoryRecentNOverride
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

// readMemoryRecords 读 memory.jsonl（文件不存在返回 nil 无错；非法行跳过）。
func readMemoryRecords(workdir string) ([]memoryRecord, error) {
	data, err := os.ReadFile(memoryPath(workdir))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
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

// appendMemoryRecord 追加一条记录到 memory.jsonl（MkdirAll .miniagent，0o600）。
func appendMemoryRecord(workdir string, r memoryRecord) error {
	path := memoryPath(workdir)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("创建 .miniagent 目录：%w", err)
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Write(append(b, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

// readMemoryTool 渲染 memory.jsonl 为文本（每行 `[type] topic: content`），供 read 工具的
// 保留路径 "memory" 调用。绕过常规符号链接/二进制检查——这是项目自有结构化文件。
func readMemoryTool(workdir string) ToolResult {
	recs, err := readMemoryRecords(workdir)
	if err != nil {
		return ToolResult{IsError: true, Output: fmt.Sprintf("读取 memory 失败：%v", err)}
	}
	if len(recs) == 0 {
		return ToolResult{Output: "（暂无项目记忆。用 write 工具 path=memory 追加记录。）"}
	}
	var sb strings.Builder
	for _, r := range recs {
		if r.Topic != "" {
			fmt.Fprintf(&sb, "[%s] %s: %s\n", r.Type, r.Topic, r.Content)
		} else {
			fmt.Fprintf(&sb, "[%s] %s\n", r.Type, r.Content)
		}
	}
	return ToolResult{Output: strings.TrimRight(sb.String(), "\n")}
}

// writeMemoryTool 追加一条 {type:"note"} 记录（write 工具 path=memory 的特殊语义：追加而非覆盖）。
func writeMemoryTool(workdir, content string) ToolResult {
	if strings.TrimSpace(content) == "" {
		return ToolResult{IsError: true, Output: "memory 记录 content 不能为空"}
	}
	if err := appendMemoryRecord(workdir, memoryRecord{Type: "note", Content: content}); err != nil {
		return ToolResult{IsError: true, Output: fmt.Sprintf("写入 memory 失败：%v", err)}
	}
	return ToolResult{Output: "已追加 1 条记忆到 .miniagent/memory.jsonl"}
}

// FormatMemorySnippet 把最近 memoryRecentN 条记忆格式化为 system prompt 注入片段；无记忆返回空串。
func FormatMemorySnippet(workdir string) string {
	recs, err := readMemoryRecords(workdir)
	if err != nil || len(recs) == 0 {
		return ""
	}
	start := 0
	if len(recs) > getMemoryRecentN() {
		start = len(recs) - getMemoryRecentN()
	}
	recs = recs[start:]
	var sb strings.Builder
	sb.WriteString("## 近期项目记忆\n")
	for _, r := range recs {
		if r.Topic != "" {
			fmt.Fprintf(&sb, "- [%s] %s: %s\n", r.Type, r.Topic, r.Content)
		} else {
			fmt.Fprintf(&sb, "- [%s] %s\n", r.Type, r.Content)
		}
	}
	return strings.TrimRight(sb.String(), "\n")
}
