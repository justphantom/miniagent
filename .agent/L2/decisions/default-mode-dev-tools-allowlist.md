---
layer: L2
type: decision
tags: [tools, default-mode, git, go, npm, rename, delete, rtk, allow-list, dev-tasks]
created: 2026-08-15
updated: 2026-08-18
confidence: high
status: superseded
---

> **v5.0.0 已废弃**：`-mode`/confineWrap/白名单工具/git/go/npm/lint/rename/delete/rtk 集成全删。见 `default-mode-not-security-boundary.md` 的 v5.0.0 段与 CHANGELOG v5.0.0。

# default 模式外部命令工具集：allow-list 收紧策略 + rtk 代理

## 背景
default 模式（2026-08-16 起 shell 不注册，此前为词法 guardrail 禁用）下需覆盖 go/js/css/html 代码开发全链路。工具必须是「执行→输出→退出」模型（无常驻进程），通过 allow-list 收紧到仅开发必需的子命令。

## 决策

### 工具集（12 个默认 + opt-in web，禁 shell 后）
| 工具 | 作用 | allow-list 粒度 |
|---|---|---|
| read/write/edit | 文件读写改 | — |
| grep/glob | 搜索查找 | — |
| git | 版本控制 | 子命令白名单 |
| go | 编译/测试/文档 | 子命令白名单 |
| npm | JS 生态构建/测试/依赖 | 子命令白名单 |
| golangci-lint | 静态检查 | 子命令白名单 |
| ast | Go 符号声明搜索 | — |
| rename | 移动/改名 | 子树校验 |
| delete | 删除文件 | 子树校验 + 精确路径 |
| web | URL 抓取（默认注册，SSRF 防护内置） | 目标主机校验 |

### allow-list 收紧原则
1. **只放开发必需子命令**，拒绝可写/可执行/可改历史的命令：
   - git：只读 15（含 `tag` 列举）+ `add/commit/pull/push/tag`（tag 仅创建/列举：拒 `-d/-f` 删改、`-F` 任意文件读）；拒 `reset/rebase/merge/checkout/switch/restore/stash/config/branch/remote/worktree/clean`
   - go：`build/test/vet/fmt/doc/list/version/clean`（`fmt` v4.7.0 入列，补 verify-gate 首步）；拒 `run`（等同 shell 执行任意代码）、`get/install/mod tidy/download/init/generate/env -w/bug`
   - npm：`install/ci/test/run/ls/outdated/audit/version`；拒 `publish/adduser/logout/create/init`
2. **参数级拒绝**：即使 allow-list 内的子命令也拒危险参数——v4.7.0 起用 `optSpec` 归一化匹配器（短旗标全等+等号粘合、go 单破折长名全等、git 双破折唯一前缀缩写识别，`flagmatch.go`），取代原 `strings.Fields`+`HasPrefix`。具体拒拦项见 `default-mode-not-security-boundary.md`「参数级收紧」段。
3. **delete 最严**：仅精确路径，拒非空目录/glob/通配符/workdir 根/symlink（空目录 `os.Remove` 非 `RemoveAll`）
4. **rename 拒 symlink 源**：防符号链接跟随逃逸
5. **npm install 前置校验 workdir 内有 package.json**：防在错误目录全局装包
6. **go mod tidy 被禁**：Go 依赖同步留给用户在 default 外执行（放开它需放开 go get 拉远程写 go.sum，与收紧原则冲突）

### npm run/test 的任意代码执行张力
`npm run`/`npm test` 经 package.json scripts 执行 shell 命令 = 任意代码执行。接受（决策 1a）：JS 生态别无选择，依赖安装和脚本执行是"开发"的定义内行为。与砍 `go run` 的先例矛盾，取舍按"生态别无选择"判定。

### 不需要的工具
本地 http server 预览、浏览器调试：常驻交互式进程，不适合工具调用模型，留给用户。

## rtk 代理（commit `626819a`）
git/go/npm 工具优先经 rtk（输出紧凑代理），rtk 未部署时回退原生二进制，零行为变化：
- `rtkBin = sync.OnceValue(exec.LookPath("rtk"))` 进程内只探测一次
- 各工具自持 `rtkXxxSubcommands` map 决定代理哪些子命令——rtk 每个工具只支持子集
- git：`rtk git -C <repoRoot> --no-pager <sub> <args>`（仅 status/diff/log/show/add/commit/pull/push）
- go：`rtk go <sub> <args>`（仅 build/test/vet）
- npm：`rtk npm <args...>`（全量 8 子命令）
- rtk 输出格式与原生不同（如 git status → `* main...origin/main` 而非 `On branch main`），测试断言按内容词不按精确格式

## 关键不变量
- default 模式非安全边界（见 `default-mode-not-security-boundary.md`），allow-list 是防 misfired 工具调用的 guardrail，非沙箱
- 真隔离责任在调用方 OS 层

## 参考
- `internal/miniagent/tools/tool_git.go`（allow-list + rtk 代理）
- `internal/miniagent/tools/tool_go.go`（allow-list + rtk 代理）
- `internal/miniagent/tools/tool_npm.go`（allow-list + rtk 代理）
- `internal/miniagent/tools/tool_golint.go`（allow-list）
- `internal/miniagent/tools/tool_rename.go`、`tool_delete.go`（子树校验）
- `internal/miniagent/tools/tool_helpers.go`（`rtkBin`/`rtkWrap`/`resolveConfinedPath`）
- commits `dd47d3c`→`30ff117`（逐步演进）
