---
layer: L2
type: incident
tags: [tools, git, go, npm, rewrite, test-truncation, lost-logic, commit-hygiene]
created: 2026-08-15
updated: 2026-08-15
confidence: high
---

# tools.go 重写丢失三段逻辑 + 测试文件截断（单次修复定位全过程）

## 现象
`go test` 三处失败：两个 cmd/miniagent 功能测试（ResultLimit override、confine escape 拒绝）+ 一个 tools 包语法错误（expected '}' found EOF）。git status 干净——坏代码已入历史。

## 根因
commit `42aa93a`（git/go 工具首版）重写 `cmd/miniagent/tools.go` 时丢了三段既有逻辑，同时 `_test.go` 写入不完整：
1. **confine 推导丢失**：原 `confine := mode == ModeDefault || (auto && confineAuto)` 被删 → default 模式下 write/edit 越界路径不再被拒（`TestBuildTools_DefaultConfineRejectsEscape` 失败）。
2. **fileResultLimit 丢失**：原 `read/edit.ResultLimit = fileResultLimit` 覆盖逻辑被删 → 配置失效恒为内置 8000（`TestBuildTools_FileResultLimitOverride` 失败）。
3. **测试文件截断**：3 个 `_test.go` 缺结尾 `}` + 未用 import（os/filepath/exec），gofmt 直接报语法错。
4. 附带发现：`runLimitedOutput` 的 `werr := <-waitErr` 未用返回值直接 `return nil` → 子命令失败（如空目录 `go vet`）仍报成功，测试假阴性。

## 定位方法（有效路径）
- `go test -v` 拿精确断言差异 → 读当前实现 → **git show 旧版本对比**（`git show 127022c:cmd/miniagent/tools.go`）——重写类 bug 的根因几乎总在「新文件 vs 旧文件」diff 里，不在新文件本身。
- `tail -c | xxd` 确认文件末尾字节，区分「截断」与「语法写错」。

## 教训
- **重写文件 ≠ 增量修改**：全文件覆写时旧文件的每一段逻辑（尤其 confine/limit 这类「默默生效」的约束）都是丢失候选。重写前先 `git show HEAD:<path>` 通读，重写后 diff 逐段核对。
- **commit message 声称 ≠ 代码事实**：`42aa93a` 标题写 "with rtk proxy support" 但 rtk 调用从未实现（后续 commit `626819a` 才补）。评审时以 grep 代码为准，不信 message。
- **测试失败先分语法/断言两类**：gofmt/vet 报语法错时 test 结果不可信，先修语法再判逻辑。
- 子进程工具必须检查 `cmd.Wait()` 错误并透传，吞错 = 假成功。

## 参考
- commit `42aa93a`（引入）、`68affd7`（修复）
- `cmd/miniagent/tools.go`（confine 推导 + ResultLimit 覆盖，重写时丢过一次的位置）
