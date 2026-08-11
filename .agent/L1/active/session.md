---
layer: L1
type: session
updated: 2026-08-12T00:00:00+08:00
---

# 当前会话

## 当前任务
无。

## 已完成
- default 模式 shell guardrail（opt-in `run.shell_allowlist` + `run.shell_confine_cd`）：新增 `internal/miniagent/tools/tool_shell_guard.go`（`GuardShell` 包装 + 词法分词器 `tokenize` + `checkAllowlist`/`checkConfineCD`/`targetEscapesWorkdir`）；config RunConfig 加两字段（纯 passthrough，未改 resolve.go）；`buildTools` 在 ModeDefault+非空 workdir 条件包装 shell（镜像 `confineWrap`），**不改 `ShellTool` 签名**——涟漪仅 buildTools 6 处测试调用点 + main.go:162。设计抉择：wrapper 而非改签名（零 churn on 21 处 ShellTool 测试）；白名单精确匹配（`/usr/bin/git` 不命中 `git`，防路径混淆）；分词器处理 `2>&1`/引号/`VAR=val`/子 shell，可被 `eval`/`$()` 绕过（best-effort，非安全边界，不变量 #13 不变）。测试：guard 单元+集成（26 用例）+ config round-trip。文档：config.example/README/ARCHITECTURE/CHANGELOG。verify-gate 全绿（gofmt 空/build/vet/test -race/lint 0）。L2 `default-mode-not-security-boundary.md` 已补记。待用户审 diff / 决定提交。
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
- system prompt 来源再收口：移除 `workdir/.miniagent/persona.md`/`rules.md` 自动加载（继全局 `~/.miniagent/` 之后的第二次收口），system prompt 彻底统一为 config `defaults.system_prompt`（未配则内置 `defaultSystemPrompt`）+ subagent guidance。删 `cmd/miniagent/project.go` + `project_test.go`，`assembleSystemPrompt` 砍 `pr` 参数简化为两步；README/CHANGELOG 同步。→ 提交 `bebaf1c`。
- 发版前完善实施（31-agent 对抗式审计定版 v4.4.0，逐条独立复核）：①删杂散轻量 tag `list`（在 HEAD 致 `git describe --tags`=list、`make build` 烧 version=list）+ Makefile/release.sh `git describe` 加 `--match "v*"` 防御未来杂散非-v tag；②CHANGELOG 重构——`[Unreleased]` 置顶且空、两 breaking（codemap/todo 移除、.miniagent 收口）合入 `[4.4.0]` Removed、fcbf64c 补 `Fixed`（compaction tail 预算回归 + system_prompt 去重）、补 deploy install / loopCfg 空兜底移除 `Changed`；③`config.example.json` 删 19 行 `//` 注释成合法 JSON（原 LoadConfig 直接拒、cp 即崩）；④扫清 `script`/`rule files` 残留注释（config.go×2 / tool_shell.go / history_util.go / loop_api.go / trim.go / SECURITY.md×2 / ARCHITECTURE.md×2 / README shellTimeout）；⑤README 补三处（L116 .miniagent 过期来源加前向指针、工具清单加 codemap/todo 迁移说明、内嵌配置示例 `models` 改对象数组 + `thinking` off）。verify-gate 全绿（gofmt 空 / build / vet / test -race / lint 0）。环境：沙箱 Go 1.24.4 + go.mod 1.25 + 无网，`go env -w GOPROXY=https://goproxy.cn,direct` 解工具链拉取阻塞。待用户审 diff / 决定提交。
- P2 补完：①anthropic `TestStreamClient_AllowUnterminatedAcceptsPartial`（flag=true 接受 content-then-EOF 半截回复，镜像 openai）——首版误把 `NewStreamClient` 第 6 参当 flag，实为 `cache`（flag 须构造后赋值，与 setup.go wiring 一致），测试当场抓出；②`TestClient_CacheTokensFoldedIntoInput`（非流式 parseResponse cache-token 折叠，镜像 sse_test）；③`TestBuildTools_AlwaysRegisters6` 加名称集断言（防一换一替换漏检）；④Makefile `verify` 目标聚合五步 gate + release.sh 改 `make verify`。CI（.github/workflows）经用户否决未加（贴合 AGENTS.md 反工程化立场，项目维持零 CI）。verify-gate 全绿。
- 精简评估 + 机会1 实施：抽 `internal/provider/httpretry` 共享包——openai/anthropic 两 provider 原各自复制的厂商无关重试原语（常量 MaxRetries/RetryBaseDelay/RetryMaxDelay + ParseRetryAfter/CapRetryDelay/SleepCtx + 状态码分类）抽到共享包；ShouldRetryStatus 参数化（公共 429/5xx 基线 + 厂商 extra 码，anthropic 传 529），各 provider 私留 isThinkingError（协议本质不同）+ shouldRetryStatus 薄包装。两 retry.go 95→38/43 行，生产代码净减约 80 行。结构根治 copy-asymmetry 对称维护（L2 incident 起因）。lint 修 2 处（slices.Contains + errors.Is）。verify-gate 全绿（build/vet/test -race/lint 0/gofmt 空）。CHANGELOG [Unreleased] 落 Changed。待用户审 diff/决定提交。

## 未决问题
无。
