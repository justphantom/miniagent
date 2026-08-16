package tools

import "testing"

// git 布尔短旗标可簇写（-qf ≡ -q -f）：只做全等/等号粘合匹配时，push -qf/-df 的 force/delete
// 语义绕过 deny 表（审计实测 -qf 完成了一次真实 force push）。
func TestMatchOption_ClusteredShorts(t *testing.T) {
	var pushSpec optSpec
	for _, sp := range gitDeniedFor("push") { // 全局表在前，按 shorts 定位 push 本地条目
		if len(sp.shorts) > 0 {
			pushSpec = sp
		}
	}
	if len(pushSpec.shorts) == 0 {
		t.Fatal("push spec with shorts not found")
	}
	for _, tok := range []string{"-qf", "-df", "-vf", "-fq", "-fd"} {
		if !matchOption(tok, pushSpec) {
			t.Errorf("clustered short %q must hit the push force/delete spec", tok)
		}
	}
	for _, tok := range []string{"-q", "-v", "-u", "-qf=x"} {
		// "-f=x" 不在负例：等号粘合形是改动前既有的命中分支（注释明载）。
		if matchOption(tok, pushSpec) {
			t.Errorf("token %q must not hit the push spec (no denied letter / '=' present)", tok)
		}
	}
	// 簇形对全局 exec 表（纯长选项 spec，无 shorts）无效：不引入新匹配面。
	for _, tok := range []string{"-qf", "--upload-pack"} {
		if matchOption(tok, gitDeniedOptions["*"][1]) && tok == "-qf" {
			t.Errorf("token %q must not hit the exec spec (longs only)", tok)
		}
	}
}

// --upload-pack 使 git 执行给定命令：本工具把 args 拼在子命令之后，git 按 pre-command 选项
// 解释，任意子命令入口都生效（实测 git pull --upload-pack=<cmd> 触发执行），故在全局表。
func TestGit_UploadPackDeniedGlobally(t *testing.T) {
	for _, sub := range []string{"pull", "push", "log", "status", "diff"} {
		if _, _, hit := checkDeniedOptions([]string{"--upload-pack=/tmp/rp.sh"}, gitDeniedFor(sub)); !hit {
			t.Errorf("git %s --upload-pack must be denied (fetch-family helper is RCE pre-subcommand)", sub)
		}
		if _, _, hit := checkDeniedOptions([]string{"--exec=/tmp/rp.sh"}, gitDeniedFor(sub)); !hit {
			t.Errorf("git %s --exec must be denied", sub)
		}
	}
}
