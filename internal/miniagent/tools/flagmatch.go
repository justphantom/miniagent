package tools

import (
	"slices"
	"strings"
)

// deniedReason 分类一条被拒参数的拒因，进入错误文案（不同类别的说明完全不同，单一文案会误导排查方向）。
type deniedReason int

const (
	reasonWrite     deniedReason = iota // 写任意文件
	reasonExec                          // 执行外部程序
	reasonHistory                       // 改写历史/远端 ref
	reasonOutOfTree                     // 越出 workdir 子树
)

func (r deniedReason) String() string {
	switch r {
	case reasonWrite:
		return "writes a report file anywhere"
	case reasonExec:
		return "runs an external program"
	case reasonHistory:
		return "rewrites history or remote refs"
	case reasonOutOfTree:
		return "escapes the workspace subtree"
	}
	return "is not allowed in default mode"
}

// optSpec 声明一条被拒选项：本工具可识别的完整拼法。短旗标（单字符）与长选项分列，
// 使 git 的唯一前缀缩写（--am ≡ --amend）在 deniedLong 匹配阶段被识别，而短旗标不做前缀外推。
type optSpec struct {
	shorts []string // 全等/"=值"粘合/布尔簇（"-qf" 含 "f"）匹配（"-f"、"-d"）
	longs  []string // 完整长选项名（不带 --；唯一前缀缩写按长选项匹配规则命中）
	reason deniedReason
}

// matchOption 判断 token（splitArgs 产物，无引号）是否命中 spec。三类匹配：
//   - 短旗标（"-f"、"-F"）：token 全等或 "-f=" 粘合。不前缀外推——push 的 "-f" 不得误杀 "-fix"。
//   - 短旗标簇（git 风格 "-qf" ≡ "-q -f"）：无 '=' 的多字母单破折号 token，只要簇内出现
//     任一 spec 短旗标字母即命中。git 短簇实际全为布尔旗标（簇内带值必须写 "-f=val" 粘合，
//     已由上面的全等分支覆盖），故"簇含被拒布尔短旗标即拒"无漏报面；代价是簇中混入无关
//     布尔短旗标（-qf）会整体被拒——保守拒绝，向 LLM 报明可拆开重写。
//   - 单破折号长名（go/npm 风格 "-w"、"-modfile"、"-registry"）：只认与 longs 全等。
//     go 生态前缀同名人多（-work vs -w、-m vs -modfile），前缀外推必误杀。
//   - 双破折号长选项（git 风格 "--amend"）：全等或唯一前缀缩写（"--am" ≡ "--amend"，
//     git 原生语义）。注意：唯一性只在本 spec 的 longs 内计数，不是 git 该子命令的完整
//     选项表——与 git 真实缩写语义存在双向偏差（已知保守分歧，接受：过拒只是换了个错，
//     漏拒形前缀经核验在真实 git 中同样歧义）；缩写命中多于一个不判（保守放行，由更
//     精确条目负责）。
//
// "=value" 后缀在匹配前统一剥离。
func matchOption(token string, spec optSpec) bool {
	for _, s := range spec.shorts {
		if token == s || strings.HasPrefix(token, s+"=") {
			return true
		}
		if !strings.Contains(token, "=") && len(token) > 2 && token[0] == '-' && token[1] != '-' &&
			strings.Contains(token[1:], s[1:]) {
			return true
		}
	}
	if !strings.HasPrefix(token, "-") || len(token) < 2 {
		return false
	}
	long := strings.TrimPrefix(token[1:], "-") // 双破折号剥第二个 -，单破折号原样
	if i := strings.IndexByte(long, '='); i >= 0 {
		long = long[:i]
	}
	if len(long) == 0 {
		return false
	}
	if token[1] != '-' { // 单破折号：仅全等
		return slices.Contains(spec.longs, long)
	}
	hit := 0
	for _, l := range spec.longs {
		switch {
		case l == long:
			return true
		case len(long) < len(l) && strings.HasPrefix(l, long):
			hit++
		}
	}
	return hit == 1
}

// checkDeniedOptions 对 fields 逐 token 匹配 specs，命中即返回该 token 与条目（false 表示放行）。
func checkDeniedOptions(fields []string, specs []optSpec) (string, optSpec, bool) {
	for _, f := range fields {
		if !strings.HasPrefix(f, "-") || f == "-" {
			continue
		}
		for _, sp := range specs {
			if matchOption(f, sp) {
				return f, sp, true
			}
		}
	}
	return "", optSpec{}, false
}

// joinNames 输出条目的人类可读旗标列表（错误文案用）。
func (s optSpec) joinNames() string {
	parts := append([]string{}, s.shorts...)
	for _, l := range s.longs {
		parts = append(parts, "--"+l)
	}
	return strings.Join(parts, "/")
}

// gitDeniedOptions 按子命令给出被拒选项表。空键为全局；仅个别子命令有的条目放对应键下，
// 避免 log -F（fixed-strings）这类子命令本地只读旗标被全局误杀。短旗标条目（-F/-f/-d）
// 只做全等/等号粘合/布尔簇（-qf）匹配，不前缀外推；长选项支持唯一缩写（--am ≡ --amend）。
var gitDeniedOptions = map[string][]optSpec{
	"commit": {
		{shorts: []string{"-F"}, longs: []string{"file"}, reason: reasonWrite}, // -F 从任意文件读 message，内容经 log/show 回显
		{longs: []string{"amend", "pathspec-from-file"}, reason: reasonHistory},
	},
	"push": {
		{shorts: []string{"-f", "-d"}, longs: []string{"force", "force-with-lease", "delete", "mirror"}, reason: reasonHistory},
	},
	"*": {
		{longs: []string{"output", "ext-diff", "no-index"}, reason: reasonWrite},
		// 本工具把 args 一律拼在子命令之后（git --no-pager <sub> <args...>），git 会把
		// --upload-pack 等当作 pre-command 选项解释——fetch 族辅助程序与任意子命令组合都是 RCE
		// （审计实测 git pull --upload-pack=<cmd> 触发执行），故入全局表而非仅 push/pull。
		{longs: []string{"receive-pack", "upload-pack", "exec", "repo"}, reason: reasonExec},
	},
}

// gitDeniedFor 取子命令的合并选项表（全局 + 子命令本地）。
func gitDeniedFor(sub string) []optSpec {
	out := append([]optSpec{}, gitDeniedOptions["*"]...)
	out = append(out, gitDeniedOptions[sub]...)
	return out
}
