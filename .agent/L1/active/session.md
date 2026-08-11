---
layer: L1
type: session
updated: 2026-08-11T13:00:00+08:00
---

# 当前会话

## 当前任务
无。

## 已完成
- 接入 Anthropic Messages API provider（方案 A：核心零改动 + wire 边界有损投影）。新建 internal/provider/anthropic/（retry/wire/wire_blocks/sse/client/stream/provider，7 文件全 ≤300 行）；config 加 Kind/Cache 字段 + anthropic 校验；cmd 装配按 Kind 字符串分支（buildLLM/buildDoer 返回接口，buildRuntimeClients/compactionOptions 改 Doer 签名，删 main.go:193 硬编码）；config.example.json 改真 anthropic 示例。核心领域类型 + openai 包零改动。thinking 经 ThinkingMapping（Map 值 JSON 对象串，覆盖 4.7+ adaptive-effort / 4.5- enabled-budget）；首版含 prompt caching + interleaved-thinking beta；signature 多轮靠 wire DROP 历史 thinking 规避。全包测试（wire/sse/retry/client/stream）+ verify-gate 全绿。待用户审阅 diff / 决定提交 + L2 沉淀。
- v4.3.0 由用户提交+打 tag（18bb801），「发版前完善」闭环。
- `.agent` 全量评审（14 文件）+ 代码核验（含复审二轮深度结构性扫描）。
- 沉淀 L2 元规则 `patterns/memory-freshness-pointer-over-count.md`。
- 编译优化：Makefile `build` 加 `-s -w`，产物 9.7M→6.8M（-30%）+ CHANGELOG → 提交 `22ed483`。
- L0 护栏变更：用户授权删除「禁止自动提交代码；提交只能由用户执行」，保留「提交前给 diff 摘要待审阅」（约束 2）→ 提交 `672c41f`。
- lint 修复：`compaction_split.go` 反向遍历改 `slices.Backward`，`golangci-lint` 清零 → 提交 `d6bc475`。
- 发版前完善 5 项：①CHANGELOG 定版 v4.3.1 ②config.example.json 补 3 项 opt-in ③README 补 2 flag + 1 环境变量 ④release.sh 加 test/lint gate ⑤session.md 同步 → 提交 `32dbffb`。
- anthropic 完善：StreamAllowUnterminated 装配、body role=system 折叠到顶层 system、stop_reason 映射、model max_tokens<=0 与 thinking.map 启动校验 + 测试 → 提交 `d6d0c61`（已 push）。
- v4.3.1 发版完成：push main + annotated tag v4.3.1 + release.sh（verify-gate 全绿 + build），版本号 `miniagent v4.3.1`。
- 本轮三项（见下，已提交并 push；详见 CHANGELOG.md [4.4.0]）：①core 层「LLM 端点 400 硬错」补丁 + session JSONL 持久化可靠性组（flock 跨进程锁 + 临时文件 Rename 原子 rewrite + ensureTrailingNewline 截崩溃半行 + 写盘期忽略 SIGINT/SIGTERM，新文件 0600/目录 0700）+ SSE 流式健壮性（连接中断/空内容行/[DONE] 处理/幂等重试）——三项均对应 L2 经验教训文件；②anthropic model-level `max_tokens<=0` 在 config 层启动报错（会覆盖 provider 合法值致每次 400）+ `thinking.map` 值启动校验（须为含 `type` 的合法 JSON 对象串，防静默禁用 thinking）；③anthropic 流式 SSE 重试/超时/`retry_after` 对齐（429 body 带 retry_after 计入 rate-limit）+ `cache_control` 计数修复（含 null 分支）+ 对应测试。
- v4.4.0 定版：session.md + CHANGELOG.md 同步落版（[4.4.0] 2026-08-11），verify-gate 全绿（gofmt 空 / build / vet / test -race / lint 0 issues）。
- 工具集收缩：移除 codemap（与 glob+read 功能重叠）+ todo（纯内存认知脚手架，与核心零策略冲突）两个内置工具，内置工具 10→6（read/write/edit/grep/glob/shell）。核心库逻辑零改动，纯工具集收缩；prompts.go 删 todo 规划引导，ARCHITECTURE/README/config/tool_glob/tool_helpers 注释同步，CHANGELOG [Unreleased] 加 Removed + breaking 迁移说明。→ 提交 `4506ce5`。

## 未决问题
无。
