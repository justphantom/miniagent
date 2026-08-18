---
layer: L1
type: session
updated: 2026-08-18
---

# 当前会话

## 状态
v5.0.0 工具精简已实施：删 `-mode`/confineWrap/白名单工具（git/go/npm/lint/rename/delete）/.git 封锁/rtk/`ModeDefault`/`ModeAuto` 常量/`confine_eval_symlinks`/`confine_auto`。保留 8 工具：read/write/edit/grep/glob/ast/shell/web（单模式，shell 恒注册）。buildTools 签名瘦身为 `(workdir, 4 timeouts, fileResultLimit, limits)`。subagent guidance 去 `{mode}` 占位符。system prompt Verify 条目改引 shell、Stay inside 去 rename/delete。verify-gate 全绿（gofmt/build/vet/test -race/lint/行数）。

## 备注
- 待提交：diff 摘要已给用户，待审阅后 commit。
- L0 第13条已重写为「agent 层零安全保障」；两 L2 决策文件标 superseded。
