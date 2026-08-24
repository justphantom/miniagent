---
layer: L0
version: 1
updated: 2026-08-24
---

# 约束

## 交互与流程红线
1. 提交前必须给 diff 摘要待审阅。
2. 禁止修改 `.gitignore`。
3. 多文件改动须先出方案待确认。
4. 编译 / 测试不过自主修复不超过 3 轮。

## 架构不变量
5. **核心零策略**（改 loop.go / 换上下文策略时）：
   - `loop.go:Run` 仅做：注册钩子 / 拼上下文 / 调 LLM / 执行工具 / 无 tool_calls 退出
   - 压缩、预算、溢出恢复、工具成型、事件输出、provider 全经 `LoopHooks`（`loop_api.go`）9 个可 nil 函数字段 + `CompactingHook` 外挂
   - 换策略只换钩子，核心零改动
6. **工具配对不变量**（改工具执行/钩子时）：
   - `assistant.tool_calls` ↔ tool 消息一一对应；下游断裂由 `fillPlaceholderTail`（`loop_tools.go`）补占位，防续跑 400
   - `ShapeToolResult` 只可改 content，禁改 role / tool_call_id
7. **session 层标记绝不进 wire**（改 provider wire 时）：`Message.Kind/Usage/IsError/Ts` 是领域层标记；`buildChatBody`（`provider/openai/wire.go`）独立构造厂商 JSON
8. **msgs / newMsgs 双轨**（改裁剪/压缩时）：`appendMsg` 同步追加；裁剪只动 `msgs`（LLM 视图），`main` 据 `newMsgs` append-only 落盘；压缩屏障依赖此语义
9. **thinking 钉死**（改 thinking 配置时）：启用必经 `provider.ThinkingMapping`；`req.Thinking==nil` 不发该字段；`validateThinking` 启动期校验，报错而非端点 400
10. **session jsonl 持久化契约**（改持久化时）：
    - append-only + flock 跨进程锁 + 临时文件 `os.Rename` 原子 rewrite
    - 写前 `ensureTrailingNewline` 截崩溃半行；写盘期忽略 SIGINT/SIGTERM
    - 新文件 0600、目录 0700
11. **stdout 是 NDJSON 机器契约**（改 CLI 输出时）：
    - `result` 事件 `text/model/input_tokens/output_tokens/steps` 无 `omitempty`（为 0 也出键）
    - 人类 prompt / 确认走 stderr
    - 流式有 content 无 `[DONE]` 无 `finish_reason` = 连接中断硬错（`errStreamUnterminated`）
12. **agent 层零安全保障**（评估安全边界时）：v5.0.0 删 `-mode`/confineWrap/白名单工具/.git 封锁；隔离完全靠运行用户 OS 权限（容器/低权 UID）
13. **依赖单向无环**（加 import 时）：
    - 方向仅 `cmd → core`、子包 → core；反向禁止
    - 子包 compaction/event/policy/tools/metrics 已移出 `internal/` 可被外部导入；config/provider/session/text 与 core 平级
    - 唯一横向边 compaction → policy（镜像常量钉死契约）
    - 领域类型 `Message/Response/Usage/Request/Delta` 必须留核心包

## 记忆系统元规则
14. L0 更新需用户显式授权或手动编辑。
