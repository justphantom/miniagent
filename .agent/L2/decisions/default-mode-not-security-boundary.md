---
layer: L2
type: decision
tags: [security, sandbox, shell, credentials, config-driven, default-mode]
created: 2026-08-09
updated: 2026-08-12
confidence: high
---

# default 模式非安全边界 + 凭证剥离局限 + confirm/sandbox 配置化

## 背景
`-mode default` 是薄软约束，不构成安全边界。需明确安全模型边界，避免被误当沙箱。

## 决策
- **写工具**（`write`/`edit`）经 `confineWrap` 限定 workdir 子树（`path.Clean` + 前缀）；`confine_eval_symlinks` opt-in 收窄符号链接 swap 的 TOCTOU 窗口（real-root 比较避免 workdir 经符号链接到达时误报）。
- **read / grep / glob** 限 workdir；只读放行 workdir 根（读/列举整个 workdir 合法——`confineWrap`/`checkConfine` 加 `readOnly` 形参，read/grep/glob 传 true 跳过根覆盖检查，越界/symlink 不变；根覆盖拒绝仅 write/edit）。
- **shell** 词边界拒 11 个提权器（`sudo|su|doas|pkexec|gsudo|run0|setpriv|nsenter|unshare|chroot|machinectl`），仍可被变量拼接 / 拆分绕过。
- **shell opt-in guardrail**（2026-08-12，`run.shell_allowlist` + `run.shell_confine_cd`，`tools.GuardShell`）：仅 ModeDefault + 非空 workdir 生效，默认关。`shell_allowlist` 命令名白名单（管道/链式每段都校验，**精确匹配**——`/usr/bin/git` 不命中 `git`，防路径混淆；词法分词器处理引号/`VAR=val`/`&&`/`;`/`|`/`&`/子 shell `()`，`2>&1` 保持词内不误切）；`shell_confine_cd` 词法拦 `cd`/`pushd` 越出 workdir（绝对/`..`/`~`/`$VAR`/裸 `cd`/`cd -`）。经 `buildTools` 包装（镜像 `confineWrap`），**不改 `ShellTool` 签名**。best-effort 词法约束，可被 `eval`/`$()`/反引号/别名绕过——仍非安全边界。
- shell 子进程继承父环境，但剥离所有 `MINIAGENT_*` 前缀变量 + 变量名含 `KEY/TOKEN/SECRET/PASSWORD/CREDENTIAL/PWD/PASS/AUTH/PAT` 关键字的第三方凭证变量（`PAT` 排除含 `PATH` 的路径类）。
- **confirm_destructive + sandbox 命令均改 `config.run` 驱动**（commits `1b00e63` / `24bf12b`）。`ConfirmOnToolUse` 危险工具确认门 emit 先跑再 gate（防 pipe-closed 错误被吞后破坏性操作仍执行）；无 TTY（subagent / CI）且无 `MINIAGENT_AUTO_APPROVE` 时 deny-by-default。

## 关键局限（取舍）
该防护对 `/proc/<pid>/environ` **无效**——procfs 暴露 exec 时刻环境快照，`cat /proc/$PPID/environ` 仍可读 key。shell 可经 `cd` / 绝对路径越界、写工具可符号链接逃逸、auto 模式无任何约束。opt-in `shell_confine_cd` / `shell_allowlist` 仅 best-effort 词法收窄（cd 越界 / 命令名），可被 `eval`/`$()`/子 shell 绕过，不改变本结论。

**真隔离（防越界 / 逃逸 / 凭证泄漏）责任在调用方 OS 层**（低权用户 + 容器 + 只读 rootfs + cgroup）；代码侧仅 guardrail 防 misfired 工具调用，越界 / 逃逸不视为漏洞。安全漏洞走 GitHub Security Advisory 私有上报，禁公开 issue。

## 参考
- `cmd/miniagent/sandbox.go`（`checkConfine`，写工具 confine 边界）
- `internal/miniagent/tools/tool_shell.go`（`sudoSuRe`，shell 提权器拒绝）
- `internal/miniagent/tools/tool_shell_guard.go`（`GuardShell`/`checkAllowlist`/`checkConfineCD`/`tokenize`，shell opt-in guardrail）
- `internal/miniagent/policy/confirm_on_tool_use.go`
- commits `1b00e63`、`24bf12b`
