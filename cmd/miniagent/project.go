package main

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/justphantom/miniagent/internal/miniagent/config"
)

// projectRules 是 loadProjectRules 的返回：workdir/.miniagent/ 下的 persona/rules 文本。
type projectRules struct {
	persona string
	rules   string
}

// hasAny 报告是否加载到任意项目规则（决定是否在 system prompt 末尾告知 LLM）。
func (p projectRules) hasAny() bool {
	return p.persona != "" || p.rules != ""
}

// loadProjectRules 读 <workdir>/.miniagent/ 的项目规则（persona.md/rules.md）。
// workdir 为空或目录/文件不存在时返回零值（不报错）。仅项目级单层查找——全局 system
// 定制改用 config defaults.system_prompt（取消 ~/.miniagent/ 全局规则层）。
func loadProjectRules(workdir string) projectRules {
	var pr projectRules
	if workdir == "" {
		return pr
	}
	dir := filepath.Join(workdir, ".miniagent")
	if _, err := os.Stat(filepath.Join(dir, "persona.md")); err == nil {
		pr.persona = readTrimmedFile(filepath.Join(dir, "persona.md"))
	}
	if _, err := os.Stat(filepath.Join(dir, "rules.md")); err == nil {
		pr.rules = readTrimmedFile(filepath.Join(dir, "rules.md"))
	}
	return pr
}

// maxProjectFileBytes 是 persona.md/rules.md 等规则文件的字节上限。
const maxProjectFileBytes = 1 << 20 // 1 MiB

func readTrimmedFile(path string) string {
	b, err := config.ReadFileLimited(path, maxProjectFileBytes)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// mergeSystemPrompt 按优先级 persona.md > rules.md > defaults.system_prompt 合并 system prompt
// （persona 存在则取代 base 作身份基线，否则沿用 base=defaults.system_prompt 工作流约束），
// 追加 rules 段，并在加载了任意项目规则时显式告知 LLM（P0）。
func mergeSystemPrompt(base, persona, rules string, hasAny bool) string {
	if persona != "" {
		base = persona
	}
	parts := []string{base}
	if rules != "" {
		parts = append(parts, "## 项目规则\n"+rules)
	}
	if hasAny {
		parts = append(parts, "（已加载 .miniagent/ 项目规则）")
	}
	return strings.Join(parts, "\n\n")
}
