package main

import (
	"fmt"
	"strings"
)

// defaultSystemPrompt 是面向工程代码开发的默认系统提示词：约束 ReAct 工作流
// （先观察 → 后修改 → 改后验证 → 失败复盘），降低模型盲改/臆测的概率。用户可用
// -system 覆盖。prompt 只写"为什么/怎么做"的约束，工具语法在各工具描述里。
const defaultSystemPrompt = `你是一名务实的软件工程师，在一个真实代码仓库里工作。遵守以下工作方式：

- 先观察后动手：改任何文件前，先用 read/grep/glob 确认当前内容与结构；路径或符号不确定就先定位，不要猜。
- 改后必须验证：代码改动后用 shell 跑相关的构建/测试（如 go build、go test）；未验证不要声称"完成"。
- 失败先复盘：命令或工具返回错误时，先 read 错误信息和相关文件，理解根因再改；不要反复盲改同一处。
- 精确修改：用 edit 时 old_string 须与文件精确匹配且唯一；多处相同改动用 replace_all；新建文件用 write。
- 大文件分段：read 返回带行号；文件较大时用 offset/limit 分段读取，不要一次吞下。`

// assembleSystemPrompt 装配最终 system prompt：空 base 兜底 defaultSystemPrompt → merge 项目规则
// （persona>rules>defaults）→ inject subagent 引导。集中三步使默认兜底可单测（NEW-1 回归）。
//
// 默认配置（无 -system / config 无 system_prompt / 无 .miniagent/persona）下 resolved.System 为空：
// 须兜底 defaultSystemPrompt。否则 injectSubagentGuidance 向空串追加 subagent 引导使其非空，
// loopCfg 的 `if system == ""` fallback 永不触发（死代码），agent 静默丢失全部 ReAct 约束。
func assembleSystemPrompt(base string, pr projectRules, configAbsPath, mode string) string {
	if base == "" {
		base = defaultSystemPrompt
	}
	return injectSubagentGuidance(mergeSystemPrompt(base, pr.persona, pr.rules, pr.hasAny()), configAbsPath, mode)
}

// injectSubagentGuidance 把 subagent fork 引导附加到 system prompt：注入 config 绝对路径
// （审查 v1 #12 + v2 #9 + v3 #6/#8）。configAbsPath 空则不注入。mode 透传父会话权限模式
// （审查 v3 P3）：不再硬编码 default，auto 父会话 fork 出的 subagent 继承 auto；空值回落 default。
// subagent 为无状态单次调用（不落盘会话，stdout 即结果），故不再注入父 session id。
func injectSubagentGuidance(system, configAbsPath, mode string) string {
	if configAbsPath == "" {
		return system
	}
	if mode == "" {
		mode = "default"
	}
	return system + "\n\n" + subagentGuidance(configAbsPath, mode)
}

// subagentGuidance 构造 fork 命令模板与递归约束文案。mode 透传父会话权限模式
// （审查 v3 P3）：默认 default，父 auto 时 subagent 命令用 -mode auto。
// shellSingleQuote 单引号包裹 s 并转义内部单引号，使含空格/元字符的 config 路径（如 macOS
// /Users/First Last/.miniagent/miniagent.json）安全作为 subagent 引导命令的参数。
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func subagentGuidance(configAbsPath, mode string) string {
	return fmt.Sprintf(`- 子任务委派：可并行的子任务用 shell 再调一次 miniagent（仅在必要时 fork，建议嵌套≤2 层）：
  echo "<子任务>" | miniagent -config %s -workdir . -mode %s -result-only
  subagent 为无状态单次调用（不落盘会话）；stdout 纯文本即结果。`, shellSingleQuote(configAbsPath), mode)
}
