---
layer: L1
type: session
updated: 2026-08-18
---

# 当前会话

## 状态
Responses 支持已实现：新增 `kind=responses` 与 `internal/provider/responses`；无状态全量 `input` + `store:false` + encrypted reasoning 本地回放；core/session 增 `ReasoningState`；配置路由/文档/测试同步；3 个超 300 行文件已机械拆分。verify-gate 全绿。

## 备注
- 2026-08-18：§P1-A 修复(e244cdd)已提交但 bin 未重编致旧二进制仍拦截 tool-output 读回。根因：bin vcs.revision=ab44adf(<e244cdd)。已 `make build` 重编，新 bin=e244cdd。排查：jsonl session[10] 拦截→`go version -m bin/miniagent` 比对 vcs.revision→源码链路自洽（main.go:188→191+194→286，readAllow 目录=store 写入目录=同一 toolOutputDir）。
- 下一版本注意：`make build` 的 version 注入须在有 shell/make 的环境执行。
