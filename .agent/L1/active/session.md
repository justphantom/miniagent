---
layer: L1
type: session
updated: 2026-08-18T00:25:00+08:00
---

# 当前会话

## 状态
system prompt workdir 约束已实现待提交：`defaultSystemPrompt` 加 "Stay inside the working directory" 条目（正向指令+点名工具/逃逸形态+给出口），`appendWorkdirLine` 扩展为 stay-inside 约束句（CLI `-workdir` 绝对路径行增强）。测试 +2 断言。verify-gate 全绿（707 passed）。无进行中任务。

## 备注
- 下一版本注意：`make build` 的 version 注入须在有 shell/make 的环境执行。
