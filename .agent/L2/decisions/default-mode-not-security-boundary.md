---
layer: L2
type: decision
tags: [security, sandbox, shell, credentials, config-driven, default-mode]
created: 2026-08-09
updated: 2026-08-16
confidence: high
---

# default 模式非安全边界 + shell 仅 auto 注册 + 凭证剥离局限 + confirm/sandbox 配置化

## 背景
`-mode default` 是薄软约束，不构成安全边界。需明确安全模型边界，避免被误当沙箱。

## 决策
- **写工具**（`write`/`edit`）经 `confineWrap` 限定 workdir 子树（`path.Clean` + 前缀）；`confine_eval_symlinks` opt-in 收窄符号链接 swap 的 TOCTOU 窗口（real-root 比较避免 workdir 经符号链接到达时误报）。
- **read / grep / glob** 限 workdir；只读放行 workdir 根（读/列举整个 workdir 合法——`confineWrap`/`checkConfine` 加 `readOnly` 形参，read/grep/glob 传 true 跳过根覆盖检查，越界/symlink 不变；根覆盖拒绝仅 write/edit）。
- **shell 仅 auto 注册**（2026-08-16）：`buildTools` 在 `mode == ModeAuto` 才 append `ShellTool`；default 注册 12 工具（无 shell，另 opt-in `web`，见下第 9 条），误调经 dispatch 报 `unknown tool`。**注册门替代两级词法过滤**——同批删除：①`ShellTool` 的 mode 形参与 `sudoSuRe` 提权器拒绝名单（sudo|su|doas|pkexec|gsudo|run0|setpriv|nsenter|unshare|chroot|machinectl，经 cmd 装配后不可达：auto 不触发检查）；②`GuardShell` 整文件 + `run.shell_allowlist`/`run.shell_confine_cd` 两 config 键（仅 default shell 生效，shell 不注册后成死代码）。default 下外部命令经 git/go/npm/golangci-lint 白名单子命令工具（见 `default-mode-dev-tools-allowlist.md`）。subagent fork 引导（经 shell）随之改 auto-only 注入。
- shell 子进程继承父环境，但剥离所有 `MINIAGENT_*` 前缀变量 + 变量名含 `KEY/TOKEN/SECRET/PASSWORD/CREDENTIAL/PWD/PASS/AUTH/PAT` 关键字的第三方凭证变量（`PAT` 排除含 `PATH` 的路径类）。
- **confirm_destructive + sandbox 命令均改 `config.run` 驱动**（commits `1b00e63` / `24bf12b`）。`ConfirmOnToolUse` 危险工具确认门 emit 先跑再 gate（防 pipe-closed 错误被吞后破坏性操作仍执行）；无 TTY（subagent / CI）且无 `MINIAGENT_AUTO_APPROVE` 时 deny-by-default。

## 关键局限（取舍）
该防护对 `/proc/<pid>/environ` **无效**——procfs 暴露 exec 时刻环境快照，`cat /proc/$PPID/environ` 仍可读 key。auto 模式 shell 可经 `cd` / 绝对路径越界、写工具可符号链接逃逸、auto 模式无任何约束。default 模式 git/go/npm 工具的子命令白名单也是防误调 guardrail，非沙箱（npm run/test 本身即任意代码执行，已接受）。

## 攻击面记账（2026-08-16 评估；同日两轮收紧后）
default 拦「一步直呼危险命令」+「直接文件路径绕过」（.git 封锁、参数级收紧），不拦「两步构造执行」——与开发闭环互斥，不可能全堵。已知通道全录：

**一档：开发闭环的定义内行为（等价 shell）**
1. `npm run/test` 经 package.json scripts 执行任意命令（1a 决策）；`npm install <pkg>` 触发依赖包 pre/postinstall 任意代码。
2. `go test` 编译并执行测试代码（含 init/TestMain）——本机任意代码执行。
3. 任一代码执行通道可读 `/proc/$PPID/environ` 或 `~/.miniagent/miniagent.json` 拿全部 key（scrubEnv 只防直读）。

**二档：间接执行/外传链（2026-08-16 二轮收紧，见下「.git 封锁」与「参数级收紧」）**
4. ~~**git hooks 执行链**~~ 已堵：write/edit/rename/delete/read 均拒 `.git/**`。
5. ~~**git push 外传**~~ 已堵（两路径）：改 `.git/config` remote（.git 封锁）+ `git push <url>` 位置参数（`checkGitPositionalArgs`：push/pull 首个非选项位置参数含 `://` 或绝对路径即拒）。
6. ~~**golangci-lint custom linter**~~ 已堵：`.golangci.yml` 属 workdir 可写文件但经 lint 工具执行的 custom linter 路径——**注：本轮未实现 config 拦截**，custom linter 声明仍可达（lint run 执行 `.golangci.yml` 声明的外部程序）。残余，接受（与 npm run 同类：执行开发工具链声明的程序）。
7. ~~**resolveModuleRoot 向上逃逸**~~ 已堵：上溯不得越出 workdir（见「参数级收紧」6）。
8. **无网络出口控制**（部分收窄）：npm install/audit、git pull/push 仍可出网到**配置的** registry/remote；`--registry` 重定向已拒，`.npmrc` 覆写残余。真出口控制靠调用方网络层。
9. **`web` 工具出网**（2026-08-17 opt-in，`run.web_fetch`）：GET 任意 HTTP(S) URL。SSRF 防护拦私网/环回/链路本地（含云 metadata 169.254.169.254）/组播/受限广播/v4-mapped v6，DNS 全 IP 校验，重定向每跳重查。残余：(a) GET 查询参数可携带数据外传（`?secret=...`）；(b) 响应内容直入上下文（prompt injection 面，同 `corrupted-summary` 族，但 web 无摘要守卫，靠模型自身）；(c) DNS rebind（解析后到连接前 IP 切换）绕单次校验。均 guardrail 定位，真防护靠调用方网络层。

**三档：残余词法窗口**
9. 写工具 confine 纯词法不追 symlink（`confine_eval_symlinks` 仅 opt-in 收窄 TOCTOU）；rename/delete 拒 symlink 源。

## 参数级收紧（2026-08-16 二轮，.git 封锁后残余通道）
1. **git 外读**：deny 前缀加 `--no-index`（diff 比较仓库外任意文件）、`-F`（commit message 从任意文件读，log/show 回读）。
2. **git push/pull URL 位置参数**：`checkGitPositionalArgs`——首个非选项位置参数含 `://` 或绝对路径即拒（refspec 不会有 `://`）；堵不改 config 的外传路径。
3. **.gitattributes 外部驱动**：`checkGitAttributes`——每次 git 工具调用前扫 repo 根 `.gitattributes`，`filter=`/`diff=`/`textconv=` 属性 token 即拒（workdir 内可写文件即可声明，`git add/diff` 触发执行，绕过 .git 封锁）。保守方向：内置 diff 驱动的误报代价是一次手动编辑，漏报是代码执行。pull 进来的恶意 attributes 不可预检（供应链面，接受）。
4. **go 参数级写出/执行**：deny 前缀加 `-o`（编译产物写子树外）、`-toolexec`（构建期执行外部程序）。
5. **npm 重定向**：拒 `--prefix`/`-C`（cwd 移出模块树）、`--registry`（依赖流指向攻击者服务器）。残余：workdir 内 `.npmrc` 同样可覆写 registry——接受（文件工具白名单化 .npmrc 的成本 > 收益，guardrail 定位）。
6. **resolveModuleRoot 限界**：从 workdir 向上找 go.mod 不得越出 workdir（`tool_go.go`，前缀判停）。堵 go/npm/lint cwd 上溯到父模块、模块级写越出子树。已知代价：workdir 在模块子目录内（repo/cmd/x 布局）时 npm 找不到 package.json 会报错（保守方向）。

## .git 封锁（2026-08-16）
文件工具直接访问 `.git/**` 绕过 git 工具 allow-list（hooks 执行链 / remote 改写外传 / config 凭证泄漏），已封：
- **两层检查**：`cmd/miniagent/sandbox.go checkConfine`（confineWrap 侧：read/write/edit/grep/glob）+ `internal/miniagent/tools/tool_helpers.go resolveConfinedPath`（rename/delete 侧）各加 `.git` 前缀拒绝（`dotGitWithinRoot`：rel 路径任一分量 == `.git` 即拒，覆盖嵌套 submodule 布局；两处同名 helper 各自私有，维持 cmd→core 依赖单向）。
- **读也拒**（read/grep/glob/grep 显式 path 指向 `.git/config` 会泄漏 remote URL/凭证）；grep/glob 递归遍历本就 SkipDir `.git`（`tool_glob.go`/`tool_grep.go`），显式 path 现在同拒。
- **auto 模式不套 confineWrap，不拦**（auto 本就无约束，语义一致）；仅 default + confineAuto 生效。
- 拒绝信息指向 git 工具：`path %q targets the .git directory (default mode); use the git tool instead`。
- 注：`git add -f`/`git clean` 等可再次引入 `.git` 内文件的方式已在 git allow-list 外（clean/add -f 不涉及 `.git` 内部写）。

**真隔离（防越界 / 逃逸 / 凭证泄漏）责任在调用方 OS 层**（低权用户 + 容器 + 只读 rootfs + cgroup + 网络出口白名单）；代码侧仅 guardrail 防 misfired 工具调用，越界 / 逃逸不视为漏洞。安全漏洞走 GitHub Security Advisory 私有上报，禁公开 issue。

## 参考
- `cmd/miniagent/tools.go`（`buildTools`，shell auto-only 注册门）
- `cmd/miniagent/sandbox.go`（`checkConfine`，写工具 confine 边界）
- `internal/miniagent/tools/tool_shell.go`（`scrubEnv`，环境剥离）
- `internal/miniagent/policy/confirm_on_tool_use.go`
- commits `1b00e63`、`24bf12b`；`30ecd18`（GuardShell，2026-08-16 删除）
