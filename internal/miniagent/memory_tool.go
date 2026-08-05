package miniagent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// readMemoryTool 渲染 memory.jsonl 为文本（每行 `[type] topic: content`），供 read 工具的
// 保留路径 "memory" 调用。绕过常规符号链接/二进制检查——这是项目自有结构化文件，但仍
// 须通过 checkMemoryPathSafe 防止 .miniagent/memory.jsonl 被软链指向 workdir 外。
func readMemoryTool(workdir string) ToolResult {
	if err := checkMemoryPathSafe(workdir); err != nil {
		return ToolResult{IsError: true, Output: fmt.Sprintf("memory 路径检查失败：%v", err)}
	}
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

// FormatMemorySnippet 把最近 memoryRecentN 条记忆格式化为 system prompt 注入片段。
// 优先 workdir/.miniagent/memory.jsonl；不存在则从 ~/.miniagent/memory.jsonl 读。
// 若 workdir 文件存在但读取失败，返回空串而不回退到 home，避免把个人记忆泄漏进项目。
func FormatMemorySnippet(workdir string) string {
	recs, err := readMemoryRecords(workdir)
	if err != nil {
		return ""
	}
	if len(recs) == 0 {
		return FormatMemorySnippetFromHome()
	}
	return formatMemoryRecs(recs)
}

// FormatMemorySnippetFromHome 从 ~/.miniagent/memory.jsonl 读记忆并格式化。
func FormatMemorySnippetFromHome() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	recs, err := readMemoryRecordsFromPath(filepath.Join(home, ".miniagent", memoryFile))
	if err != nil || len(recs) == 0 {
		return ""
	}
	return formatMemoryRecs(recs)
}

func formatMemoryRecs(recs []memoryRecord) string {
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

// MemoryRecord 是 memory.jsonl 记录的导出别名，供 cmd 层读写。
type MemoryRecord = memoryRecord

// ReadMemoryRecords 导出读取 workdir 记忆（文件不存在返回 nil 无错）。
func ReadMemoryRecords(workdir string) ([]MemoryRecord, error) {
	return readMemoryRecords(workdir)
}

// AppendMemoryRecord 导出追加一条记录（含 0600/O_NOFOLLOW/路径校验/轮转）。
func AppendMemoryRecord(workdir string, r MemoryRecord) error {
	return appendMemoryRecord(workdir, r)
}

// MessagesUseTools 报告 messages 中是否出现 tool 角色或 tool_calls，
// 用于判断会话是否"值得抽取记忆"（避免对无工具的简单问答白跑一次抽取）。
func MessagesUseTools(msgs []Message) bool {
	for _, m := range msgs {
		if m.Role == roleTool {
			return true
		}
		if len(m.ToolCalls) > 0 {
			return true
		}
	}
	return false
}

// MessagesUseWriteOrEdit 报告 messages 中是否出现 write 或 edit 工具调用。
// 仅 write/edit 真正修改项目文件，read/grep/glob/shell 是读-only 探索，不值得
// 为后者额外跑一次 LLM 抽取记忆。
func MessagesUseWriteOrEdit(msgs []Message) bool {
	for _, m := range msgs {
		for _, tc := range m.ToolCalls {
			if tc.Name == "write" || tc.Name == "edit" {
				return true
			}
		}
	}
	return false
}
