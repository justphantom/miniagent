package miniagent

// Limits 集中所有运行时可调阈值，替代散落于各模块的包级 atomic override（Set*）。
// 经工具构建函数 / session 函数 / 钩子工厂显式注入，消除包级可变状态——
// 支持多实例（如 subagent fork 用不同 limits）、无 race 风险（不需 atomic）、测试隔离（传参而非 Set 全局）。
// 零值字段在各注入点回落模块内置默认（<=0 用默认）。分步接入：第1步工具，第2步 session，
// 第3步 context-trim，第4步 compaction。
type Limits struct {
	// MaxReadFileBytes 是 read 工具单文件读取字节上限（默认 maxReadFileBytes=1MB）。
	MaxReadFileBytes int
	// MaxShellOutputChars 是 shell/glob/grep 共享的工具输出字符上限（默认 maxShellOutputChars=100KB）。
	MaxShellOutputChars int
	// ShellStreamWindowBytes 是 shell 输出滑窗字节上限（默认 2*MaxShellOutputChars*4）。
	ShellStreamWindowBytes int
	// MaxGrepMatches 是 grep 命中行上限（默认 maxGrepMatches=500）。
	MaxGrepMatches int
	// 以下字段后续步骤接入（此处定义待用，零值=各模块内置默认）：
	// MaxSessionBytes：session 文件字节上限（第2步）。
	MaxSessionBytes int
	// ContextTrimToolChars：context 超限时 tool content 压缩上限（第3步）。
	ContextTrimToolChars int
}
