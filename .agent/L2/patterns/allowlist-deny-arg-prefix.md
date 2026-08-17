---
layer: L2
type: pattern
tags: [tools, allow-list, deny-list, default-mode, dev-safety, subprocess, parameter-level-reject]
created: 2026-08-15
updated: 2026-08-15
confidence: high
---

# 子命令白名单 + 参数前缀拒绝：default 模式外部工具收紧模式

## 模式
default 模式下调用外部二进制（git/go/npm），不能用 deny-list（可写命令太多且不断新增），用 **allow-list 子命令白名单 + 参数级前缀拒绝** 双层收紧。

## 做法
1. **子命令 allow-list**（map[string]bool，默认拒绝未知）：
   - 只放开发必需子命令，拒绝可写/可执行/可改历史的命令
   - git：只读 14 + add/commit/pull/push；拒 reset/rebase/merge/checkout/switch/restore
   - go：build/test/vet/doc/list/version/clean；拒 run/get/install/mod tidy/generate/fmt/env -w
   - npm：install/ci/test/run/ls/outdated/audit/version；拒 publish/adduser/logout/create/init
2. **参数级前缀拒绝**（even allow-list 内的子命令也拦危险参数）：
   - git 拒 `--output`/`-O`/`--ext-diff`（只读子命令借此写文件/拉外部程序）
   - go 拒 `-w`/`-write`/`-fix`/`-modfile`（build/vet 借此改源码或 go.mod）
   - `strings.Fields(args)` 分词后逐词 `strings.HasPrefix` 检查
3. **错误消息列允许的子命令全名**，便于调用方/LLM 自纠
4. **删死参数**：构造函数签名上的未用参数（如 `confineSymlinks`）是误导，要么用要么删
5. **confineWrap 不适用于命令工具**：confineWrap 只解析 `{"path":...}`，而 git/go/npm 的 args 无 path 字段，包装是 no-op。只对文件工具（read/write/edit/grep/glob/rename/delete）做 confineWrap

## 为何不用 deny-list
deny-list 只拦已知危险命令，新命令默认放行——git/go/npm 的可写子命令数量大（stash/config/branch/replace/notes/remote/worktree/clean…），deny-list 维护成本高且易漏。allow-list 默认拒绝未知，新增命令安全（自动被拒）。新放行的子命令须按 `git tag` 先例（2026-08-16）做全 flag 面审计：破坏/文件/exec 三类逐 flag 定拒或显式接受先例（tag 的 `-d/-f` 删改、`-F` 文件读拒；`-s/-v` gpg exec 按 `commit -S` 先例接受）。

## 何时用
- default 模式下调外部二进制且需收紧
- 外部命令有大量子命令、其中可写子命令占比高
- 需要参数级防护（子命令只读但参数可写）

## 参考
- `internal/miniagent/tools/tool_git.go`（`allowedGitSubcommands` + `deniedGitArgPrefixes`）
- `internal/miniagent/tools/tool_go.go`（`allowedGoSubcommands` + `deniedGoArgPrefixes`）
- `internal/miniagent/tools/tool_npm.go`（`allowedNpmSubcommands`）
- commits `54af182`→`30ff117`
