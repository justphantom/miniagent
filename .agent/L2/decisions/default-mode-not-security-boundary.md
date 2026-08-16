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
- **shell 仅 auto 注册**（2026-08-16）：`buildTools` 在 `mode == ModeAuto` 才 append `ShellTool`；default 注册 11 工具（无 shell），误调经 dispatch 报 `unknown tool`。**注册门替代两级词法过滤**——同批删除：①`ShellTool` 的 mode 形参与 `sudoSuRe` 提权器拒绝名单（sudo|su|doas|pkexec|gsudo|run0|setpriv|nsenter|unshare|chroot|machinectl，经 cmd 装配后不可达：auto 不触发检查）；②`GuardShell` 整文件 + `run.shell_allowlist`/`run.shell_confine_cd` 两 config 键（仅 default shell 生效，shell 不注册后成死代码）。default 下外部命令经 git/go/npm/golangci-lint 白名单子命令工具（见 `default-mode-dev-tools-allowlist.md`）。subagent fork 引导（经 shell）随之改 auto-only 注入。
- shell 子进程继承父环境，但剥离所有 `MINIAGENT_*` 前缀变量 + 变量名含 `KEY/TOKEN/SECRET/PASSWORD/CREDENTIAL/PWD/PASS/AUTH/PAT` 关键字的第三方凭证变量（`PAT` 排除含 `PATH` 的路径类）。
- **confirm_destructive + sandbox 命令均改 `config.run` 驱动**（commits `1b00e63` / `24bf12b`）。`ConfirmOnToolUse` 危险工具确认门 emit 先跑再 gate（防 pipe-closed 错误被吞后破坏性操作仍执行）；无 TTY（subagent / CI）且无 `MINIAGENT_AUTO_APPROVE` 时 deny-by-default。

## 关键局限（取舍）
该防护对 `/proc/<pid>/environ` **无效**——procfs 暴露 exec 时刻环境快照，`cat /proc/$PPID/environ` 仍可读 key。auto 模式 shell 可经 `cd` / 绝对路径越界、写工具可符号链接逃逸、auto 模式无任何约束。default 模式 git/go/npm 工具的子命令白名单也是防误调 guardrail，非沙箱（npm run/test 本身即任意代码执行，已接受）。

**真隔离（防越界 / 逃逸 / 凭证泄漏）责任在调用方 OS 层**（低权用户 + 容器 + 只读 rootfs + cgroup）；代码侧仅 guardrail 防 misfired 工具调用，越界 / 逃逸不视为漏洞。安全漏洞走 GitHub Security Advisory 私有上报，禁公开 issue。

## 参考
- `cmd/miniagent/tools.go`（`buildTools`，shell auto-only 注册门）
- `cmd/miniagent/sandbox.go`（`checkConfine`，写工具 confine 边界）
- `internal/miniagent/tools/tool_shell.go`（`scrubEnv`，环境剥离）
- `internal/miniagent/policy/confirm_on_tool_use.go`
- commits `1b00e63`、`24bf12b`；`30ecd18`（GuardShell，2026-08-16 删除）
