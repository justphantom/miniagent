package main

import (
	"encoding/json"
	"log/slog"
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

// projectRules 是 loadProjectRules 的返回：persona/rules 文本与 scripts 列表。
// *_Set 标记表示对应文件在目录中显式存在（即使内容为空），用于 workdir 覆盖 home 时区分“未提供”与“提供空值”。
type projectRules struct {
	persona    string
	personaSet bool
	rules      string
	rulesSet   bool
	scripts    []scriptDef
	scriptsSet bool
}

// hasAny 报告是否加载到任意项目规则/脚本（决定是否在 system prompt 末尾告知 LLM）。
func (p projectRules) hasAny() bool {
	return p.persona != "" || p.rules != "" || len(p.scripts) > 0
}

// loadProjectRules 读 <workdir>/.miniagent/ 和 ~/.miniagent/ 的项目规则。
// 优先级：workdir/.miniagent/ > ~/.miniagent/ > 空。各文件单独覆盖（非合并）。
// workdir 为空时仅从 home 目录读取。
func loadProjectRules(workdir string, logger *slog.Logger) projectRules {
	var pr projectRules
	// 从 home 目录读基线（若存在）
	pr = loadProjectRulesFromDir("", logger)
	// workdir 覆盖基线
	if workdir != "" {
		pr = mergeProjectRules(loadProjectRulesFromDir(filepath.Join(workdir, ".miniagent"), logger), pr)
	}
	return pr
}

// loadProjectRulesFromDir 从指定目录读 persona/rules/scripts。
// dir="" 是特殊哨兵，表示从 ~/.miniagent/ 读取（由函数内部解析 home 目录）。
func loadProjectRulesFromDir(dir string, logger *slog.Logger) projectRules {
	var pr projectRules
	if dir == "" {
		home, err := os.UserHomeDir()
		if err == nil {
			dir = filepath.Join(home, ".miniagent")
		} else {
			return pr
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "persona.md")); err == nil {
		pr.personaSet = true
		pr.persona = readTrimmedFile(filepath.Join(dir, "persona.md"))
	}
	if _, err := os.Stat(filepath.Join(dir, "rules.md")); err == nil {
		pr.rulesSet = true
		pr.rules = readTrimmedFile(filepath.Join(dir, "rules.md"))
	}
	scriptsPath := filepath.Join(dir, "scripts.json")
	if _, err := os.Stat(scriptsPath); err == nil {
		pr.scriptsSet = true
		data, rerr := miniagent.ReadFileLimited(scriptsPath, maxProjectFileBytes)
		if rerr != nil {
			if logger != nil {
				logger.Warn("读取 scripts.json 失败，跳过项目脚本", "path", scriptsPath, "error", rerr)
			}
		} else {
			var s struct {
				Scripts []scriptDef `json:"scripts"`
			}
			if jerr := json.Unmarshal(data, &s); jerr != nil {
				if logger != nil {
					logger.Warn("scripts.json 解析失败，跳过项目脚本", "path", scriptsPath, "error", jerr)
				}
			} else {
				pr.scripts = s.Scripts
			}
		}
	}
	return pr
}

// mergeProjectRules 用 workdir 的规则覆盖 home 规则（非合并，workdir 文件存在即覆盖，空文件也覆盖）。
func mergeProjectRules(workdir, home projectRules) projectRules {
	if workdir.personaSet {
		home.persona = workdir.persona
	}
	if workdir.rulesSet {
		home.rules = workdir.rules
	}
	if workdir.scriptsSet {
		home.scripts = workdir.scripts
	}
	return home
}

// maxProjectFileBytes 是 persona.md/rules.md/scripts.json 等规则文件的字节上限。
const maxProjectFileBytes = 1 << 20 // 1 MiB

func readTrimmedFile(path string) string {
	b, err := miniagent.ReadFileLimited(path, maxProjectFileBytes)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// mergeSystemPrompt 按优先级 persona.md > rules.md > defaults.system_prompt 合并 system prompt
// （persona 存在则取代 base 作身份基线，否则沿用 base=defaults.system_prompt 工作流约束），
// 追加 rules 段，并在加载了任意项目规则/脚本时显式告知 LLM（P0）。
func mergeSystemPrompt(base, persona, rules string, hasAny bool) string {
	if persona != "" {
		base = persona
	}
	parts := []string{base}
	if rules != "" {
		parts = append(parts, "## 项目规则\n"+rules)
	}
	if hasAny {
		parts = append(parts, "（已加载 .miniagent/ 项目规则或脚本）")
	}
	return strings.Join(parts, "\n\n")
}
