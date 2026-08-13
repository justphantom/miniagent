---
layer: L0
version: 1
updated: 2026-08-13
---

# 约束

## 交互与流程红线
1. 仅做用户明确要求的事；不确定时呈现选项，不替用户决策。
2. 提交前必须给 diff 摘要待审阅。
3. 禁止修改 `.gitignore`。
4. 多文件改动须先出方案待确认。
5. 编译 / 测试不过自主修复不超过 3 轮。

## 架构不变量
6. **核心零策略**：`loop.go:Run` 仅做注册钩子 / 拼上下文 / 调 LLM / 执行工具 / 无 tool_calls 退出。压缩、预算、溢出恢复、工具成型、事件输出、provider 全经 `LoopHooks`（`loop_api.go`）9 个可 nil 函数字段 + `CompactingHook` 外挂。换策略只换钩子，核心零改动。
7. **工具配对不变量**：`assistant.tool_calls` ↔ tool 消息一一对应。下游管道断裂由 `fillPlaceholderTail`（`loop_tools.go`）补占位 tool 消息，防续跑端点 400。`ShapeToolResult` 只可改 content，禁改 role / tool_call_id。
8. **session 层标记绝不进 wire**：`Message.Kind` / `Usage` / `IsError` / `Ts` 是领域层标记；`buildChatBody`（`provider/openai/wire.go`）独立构造厂商 JSON，防内部状态泄漏给 LLM。
9. **msgs / newMsgs 双轨**：`appendMsg` 同步追加；裁剪只动 `msgs`（LLM 视图），`main` 据 `newMsgs` append-only 落盘。压缩屏障依赖此语义。
10. **thinking 钉死**：启用必经 `provider.ThinkingMapping`（`{field, map[level]}`），`req.Thinking==nil` 不发该字段；启动期 `validateThinking` 枚举前置校验，`level∉map` 启动报错而非端点 400。
11. **session jsonl 持久化契约**：append-only + flock 跨进程锁 + 临时文件 `os.Rename` 原子 rewrite + 写前 `ensureTrailingNewline` 截崩溃半行 + 写盘期忽略 SIGINT/SIGTERM；新文件 0600、目录 0700。
12. **stdout 是 NDJSON 机器契约**：`result` 事件 `text/model/input_tokens/output_tokens/steps` 不带 `omitempty`（为 0 也出键）；人类 prompt / 确认走 stderr。流式有 content 无 `[DONE]` 无 `finish_reason` 视为连接中断硬错（`errStreamUnterminated`）。
13. **default 模式非安全边界**：shell 可 cd / 绝对路径越界、写工具可符号链接逃逸、auto 模式无约束；真隔离靠调用方（沙箱 / 容器 / 低权用户）。越界 / 逃逸不视为漏洞。
14. **依赖单向无环**：`cmd → core`、子包（compaction/event/provider/tools/policy/config/session）`→ core`，反向禁止；领域类型 `Message/Response/Usage/Request/Delta` 必须留核心包。

## 记忆系统元规则
15. 遇历史决策/陌生报错/选型时的检索路由见根目录 `AGENTS.md`「路由」，本层不重复。
16. 版本跟踪引用纪律见根目录 `AGENTS.md`（跟踪文件不引用未跟踪内容），本层不重复。
17. L0 更新需用户显式授权或手动编辑。
