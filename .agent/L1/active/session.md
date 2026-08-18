---
layer: L1
type: session
updated: 2026-08-18
---

# 当前会话

## 状态
Responses 支持已实现：新增 `kind=responses` 与 `internal/provider/responses`；无状态全量 `input` + `store:false` + encrypted reasoning 本地回放；core/session 增 `ReasoningState`；配置路由/文档/测试同步；3 个超 300 行文件已机械拆分。verify-gate 全绿。

## 备注
- 下一版本注意：`make build` 的 version 注入须在有 shell/make 的环境执行。
