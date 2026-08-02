package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"github.com/justphantom/miniagent/internal/miniagent"
)

// scriptDef 是 .miniagent/scripts.json 的一条脚本声明（P1：注册为 script_<name> 工具）。
type scriptDef struct {
	Name        string `json:"name"`
	Command     string `json:"command"`
	Description string `json:"description"`
}

// projectRules 是 loadProjectRules 的返回：persona/rules 文本、scripts 列表、memory 注入片段。
type projectRules struct {
	persona string
	rules   string
	scripts []scriptDef
	memory  string
}

// hasAny 报告是否加载到任意项目规则/脚本/记忆（决定是否在 system prompt 末尾告知 LLM）。
func (p projectRules) hasAny() bool {
	return p.persona != "" || p.rules != "" || len(p.scripts) > 0 || p.memory != ""
}

// loadProjectRules 读 <workdir>/.miniagent/{persona.md, rules.md, scripts.json, memory.jsonl}。
// 各文件可选（缺一不影响其余）。workdir 为空时返回零值（P0/P1/P5 仅在 workdir 下发现）。
func loadProjectRules(workdir string) projectRules {
	var pr projectRules
	if workdir == "" {
		return pr
	}
	dir := filepath.Join(workdir, ".miniagent")
	pr.persona = readTrimmedFile(filepath.Join(dir, "persona.md"))
	pr.rules = readTrimmedFile(filepath.Join(dir, "rules.md"))
	pr.memory = miniagent.FormatMemorySnippet(workdir)
	if data, err := os.ReadFile(filepath.Join(dir, "scripts.json")); err == nil {
		var s struct {
			Scripts []scriptDef `json:"scripts"`
		}
		if json.Unmarshal(data, &s) == nil {
			pr.scripts = s.Scripts
		}
	}
	return pr
}

func readTrimmedFile(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// mergeSystemPrompt 按优先级 persona.md > rules.md > defaults.system_prompt 合并 system prompt
// （persona 存在则取代 base 作身份基线，否则沿用 base=defaults.system_prompt 工作流约束），
// 追加 rules 段与 memory 片段，并在加载了任意项目规则/脚本时显式告知 LLM（P0）。
func mergeSystemPrompt(base, persona, rules, memory string, hasAny bool) string {
	if persona != "" {
		base = persona
	}
	parts := []string{base}
	if rules != "" {
		parts = append(parts, "## 项目规则\n"+rules)
	}
	if memory != "" {
		parts = append(parts, memory)
	}
	if hasAny {
		parts = append(parts, "（已加载 .miniagent/ 项目规则与脚本）")
	}
	return strings.Join(parts, "\n\n")
}
