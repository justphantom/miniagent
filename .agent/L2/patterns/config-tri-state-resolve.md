---
layer: L2
type: pattern
tags: [config, resolve, tri-state, footgun, layering, implicit-behavior]
created: 2026-08-09
updated: 2026-08-09
confidence: high
---

# config 三态裁决 + 隐性 footgun

## 模式
`internal/miniagent/config/resolve.go` 的 `resolveRun` 用 `*bool` / `*int` 三态（`nil`=unset）仲裁 CLI 与 config：CLI override 非 nil 优先，`nil` 回落 `cfg.Run`（**实际优先级 `cli > config > builtin`**）。模型参数走 `pickMPG(model, provider, cfg.Run)` 三层覆盖（`max_tokens` / `context_window` / `thinking` 支持 `model > provider > global`）。

## 隐性 footgun（以 main 装配代码为准，不能只看字段零值）
1. **`tool_output_dir` 留空 ≠ 关闭**：字面空串文档含义「禁用」，但 `cmd/miniagent/main.go` 未配置时按 session 目录自动派生 `<sessionDir>/<id>.tool-output/`——即「显式禁用」与「未配置」行为不同，留空实际已启用。单文件封顶 `toolOutputMaxBytes=1MiB`、文件数上限 500（commit `c6b7999` oldest-mtime-first 淘汰，保护最近 read-back 契约）、7d 机会性 cleanup。
2. **`session.dir` 不展开 `~`**：`main.go` 未对 `~` 前缀做 `os.UserHomeDir()` 展开，config 写 `~/.miniagent/.sessions` 时 `~` 被当字面相对路径，在 workdir 下创 `./~/`——会话散落各 workdir、无法跨 workdir 接续、垃圾 `./~/` 入 git status。修复须统一 `~` 展开或写绝对路径。
3. **`pickMPG` 静默遮蔽**（`resolve.go`）：实际语义是 `model > provider > global` 非 nil 优先（不比较大小），非「取较大者」；模型 CW 一旦非 nil 即遮蔽 run CW，操作者 `run.context_window` 意图被静默丢弃（如 run=128K 但模型=200K → 压缩阈值按 200K 算）。

## 关键约束
1. 新增可被 CLI 覆盖的旋钮须走 `*T` 三态仲裁，保持 `nil` 语义（`bool + omitempty` 无法区分「未设置」与「显式 false」）。
2. 配置留空的默认行为以装配代码为准，非字段零值。
3. 注意 commit `24bf12b` message 措辞「config > CLI flag」是 loose 表述，实际代码是 CLI 非 nil 优先。

## 参考
- `internal/miniagent/config/resolve.go`（`resolveRun` / `pickMPG`）、`cmd/miniagent/main.go`（session.dir 派生）
- `internal/miniagent/policy/tool_output_store.go`（tool_output 封顶常量 1MiB / 500 / 7d）
- commits `c6b7999`、`24bf12b`
