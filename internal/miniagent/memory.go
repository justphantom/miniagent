package miniagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const (
	memoryDir          = ".miniagent"
	memoryFile         = "memory.jsonl"
	memoryRecentN      = 10 // system prompt 注入的最近记忆条数；可通过 SetMemoryRecentN 覆盖
	memoryPathToken    = "memory"
	maxMemoryFileBytes = 1 << 20 // 1 MiB；超限后丢弃旧记录轮转
)

// memoryRecentNOverride 允许配置覆盖内置默认；nil 用常量默认。
var memoryRecentNOverride int

// SetMemoryRecentN 覆盖记忆注入条数；测试用，正常流程由 Resolve 调用。
func SetMemoryRecentN(n int) {
	if n > 0 {
		memoryRecentNOverride = n
	}
}

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

// memoryExtractMaxTokens 限制抽取请求的输出 token：仅产 ≤K 条短 JSON，无需长输出。
const memoryExtractMaxTokens = 768

func getMemoryExtractMaxTokens() int { return memoryExtractMaxTokens }

// defaultMemoryExtractPrompt 是抽取记忆的默认 system prompt（%d=最大条数，%s=已有记忆，%s=对话）。
const defaultMemoryExtractPrompt = `你是对话记忆抽取器。从下列对话中抽取不超过 %d 条值得长期记住的项目事实，严格只输出一个 JSON 数组，每个元素形如 {"type":"note","topic":"可选主题","content":"事实"}，不要输出任何解释或多余文字。

规则：
1. 只抽稳定长期事实：架构决策、约定规范、目录/接口结构、已踩坑与规避方法、用户偏好。不抽临时过程、单次命令输出、闲聊或易变状态。
2. 与「已有记忆」语义重复的，不要输出。
3. 严禁输出密钥、token、口令、凭证或个人隐私；含敏感信息的整条丢弃。
4. 没有值得记的事实时，输出 []。

已有记忆：
%s

对话：
%s`

// ExtractMemory 调 llm 从 transcript 抽取至多 maxRecords 条记忆。
// existing 用于去重锚点；secrets 中的字面量若出现在某条 content 内则该条被丢弃。
// 返回待追加记录与该次调用 Usage。模型输出解析失败时返回 (nil, usage, nil)——丢弃但不视为错误。
func ExtractMemory(ctx context.Context, llm *ChatClient, model, prompt string, maxRecords int, existing []MemoryRecord, secrets []string, transcript []Message) ([]MemoryRecord, Usage, error) {
	if llm == nil {
		return nil, Usage{}, errors.New("ExtractMemory: llm 为 nil")
	}
	if maxRecords <= 0 || len(transcript) == 0 {
		return nil, Usage{}, nil
	}
	if strings.TrimSpace(prompt) == "" {
		prompt = defaultMemoryExtractPrompt
	}
	sys := fmt.Sprintf(prompt, maxRecords, formatExistingForDedup(existing), renderTranscript(transcript))
	resp, err := llm.Do(ctx, Request{
		Model:     model,
		System:    sys,
		MaxTokens: getMemoryExtractMaxTokens(),
	})
	if err != nil {
		return nil, resp.Usage, err
	}
	raw := stripCodeFence(strings.TrimSpace(resp.Text))
	var recs []memoryRecord
	// 解析失败：丢弃本次抽取，但不向上报错（best-effort，不阻断会话退出）。
	_ = json.Unmarshal([]byte(raw), &recs)
	recs = filterMemoryRecords(recs, maxRecords, existing, secrets)
	return recs, resp.Usage, nil
}

// renderTranscript 把对话渲染为紧凑文本供抽取 prompt 使用（每条内容截断，整体有上限）。
func renderTranscript(msgs []Message) string {
	const perMsgCap = 1500
	const totalCap = 20000
	var sb strings.Builder
	for _, m := range msgs {
		var contentB strings.Builder
		contentB.WriteString(strings.TrimSpace(m.Content))
		for _, tc := range m.ToolCalls {
			contentB.WriteString("\n[tool_call ")
			contentB.WriteString(tc.Name)
			contentB.WriteString("] ")
			contentB.WriteString(tc.Args)
		}
		if m.Kind == KindSummary {
			contentB.WriteString("[既往对话摘要] ")
		}
		fmt.Fprintf(&sb, "[%s] %s\n", m.Role, truncate(contentB.String(), perMsgCap, "…"))
		if sb.Len() > totalCap {
			sb.WriteString("…[对话已截断]")
			break
		}
	}
	return strings.TrimRight(sb.String(), "\n")
}

// formatExistingForDedup 把已有记忆渲染为去重锚点（取最近 20 条，避免 prompt 过长）。
func formatExistingForDedup(existing []memoryRecord) string {
	if len(existing) == 0 {
		return "（无）"
	}
	start := 0
	if len(existing) > 20 {
		start = len(existing) - 20
	}
	var sb strings.Builder
	for _, r := range existing[start:] {
		if r.Topic != "" {
			fmt.Fprintf(&sb, "- %s: %s\n", r.Topic, r.Content)
		} else {
			fmt.Fprintf(&sb, "- %s\n", r.Content)
		}
	}
	return strings.TrimRight(sb.String(), "\n")
}

// filterMemoryRecords 过滤/去重/限流抽取结果：去空白、剔密钥、本地精确去重、补默认 type、截到 maxRecords。
func filterMemoryRecords(in []memoryRecord, maxRecords int, existing []memoryRecord, secrets []string) []memoryRecord {
	seen := make(map[string]bool, len(existing))
	for _, r := range existing {
		seen[normalizeContent(r.Content)] = true
	}
	var out []memoryRecord
	for _, r := range in {
		c := strings.TrimSpace(r.Content)
		if c == "" {
			continue
		}
		if containsSecret(c, secrets) {
			continue
		}
		key := normalizeContent(c)
		if seen[key] {
			continue
		}
		seen[key] = true
		r.Content = c
		if r.Type == "" {
			r.Type = "note"
		}
		out = append(out, r)
		if len(out) >= maxRecords {
			break
		}
	}
	return out
}

func containsSecret(s string, secrets []string) bool {
	for _, sc := range secrets {
		if sc != "" && strings.Contains(s, sc) {
			return true
		}
	}
	return false
}

func normalizeContent(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(s))), " ")
}

// stripCodeFence 去除模型可能包裹的 ```json ... ``` 代码块围栏。
func stripCodeFence(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		if idx := strings.IndexByte(s, '\n'); idx >= 0 {
			s = s[idx+1:]
		}
		s = strings.TrimSuffix(strings.TrimSpace(s), "```")
		s = strings.TrimSpace(s)
	}
	return s
}
