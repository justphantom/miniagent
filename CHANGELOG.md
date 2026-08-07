# Changelog

所有显著变更进入此文件。格式参考 [Keep a Changelog](https://keepachangelog.com/)，
版本号遵循 [Semantic Versioning](https://semver.org/)。

## [Unreleased]

### Fixed
- **`summaryMaxTokens` 与 `summaryMaxChars` CJK 不一致修复**：原固定 `summaryMaxTokens=1024` 对 CJK 偏紧（1024 token≈1500 汉字，远低于 `summaryMaxChars=5000`），中文摘要被 `MaxTokens` 隐性截短到设计值约 30%；现默认从 `summaryMaxChars` 派生（`/2`，与 `EstimateTokens` 的 `CJK≈1token/2chars` 同口径），保证纯中文摘要填满字符上限不被 token 先截。新增纯函数 `deriveSummaryMaxTokens` 在 `NewCompaction` 装配时派生——只配 `summary_max_chars` 时 token 自动跟随，显式配 `summary_max_tokens` 仍可覆盖。（对应 `docs/codebase-third-evaluation.md` L3-5）
- **`compactWithSummary` tail 预算改为联合预算（方向 B）**：原 tail 按 `preserveRecentTokens` 独立选取，不考虑摘要+head 已占空间，中等 ContextWindow（约 5k）下 `head+summary+tail` 超 `CW×4/5` → `trimRecentRounds` 后仍超 → 终止 error（白烧摘要调用）。新增 `jointTailBudget`：tail 预算 = `min(CW×4/5 − 请求开销 − head − 摘要估算, preserveRecentTokens)`，使不可压缩的 summary 优先、tail 让步，从源头消除中等 CW 的压缩后终止（实测 CW=5120 由终止→不终止）。UPDATE 路径下旧 summary 不进 out 时精确不扣 head。**边界**：极小 CW（≤~4k，summary 本身装不下 `CW×4/5`）仍终止，需 `summaryMaxChars` 随窗口缩放（已由下条方向 A 解决）。
- **`summaryMaxChars` 默认随 ContextWindow 缩放（方向 A）**：原固定 5000 在小 CW（≤~4k）下 summary 本身（~2500 token）> `CW×4/5` 致压缩后终止（B 的边界）。现默认 `min(5000, context_window/5)`（summary token 占 CW ~10%），小窗口自适应避免 summary 装不下；大窗口（CW≥25000）仍取 5000。与 B 联合预算、`summaryMaxTokens` 派生三者联动构成「summary 体积自适应 CW」体系；用户显式 `summary_max_chars` 仍覆盖。实测把「压缩不终止」CW 下界从 ~5120 降到 ~1536。**硬边界**：CW<~1536 仍可能终止（请求级 overhead + head 占比过高，物理极限，非 A 范围）。
- **`windowStartOf(keepN<=0)` 语义修正**：原 `keepN<=0` 返回 0，被 `dedupReadResults`/`foldStaleReadResults`/`foldStaleWriteEditArgs`/`dedupShellCommands` 解读为「全窗口内=全保留」，与 `keepN=0` 的「不保留=全压」语义矛盾。改为返回 `len(msgs)`（全窗口外=全压）。释放摘要前 middle strip 的 dedup/fold（中段重复 read / 被后续写入取代的旧 read / 同义 shell / 被取代的 write-edit args 现被去重），并修正 `context_keep_tool_args=0` 的语义；生产默认 `context_keep_tool_args=2` 不受影响。

### Changed
- **摘要前对 middle 全压 strip**：`compactWithSummary` 原把原始中段（含 reasoning / tool 结果 / write-edit args）直接喂给摘要 LLM，现摘要前对其全压 strip（`keepN=0`）——省摘要 input token（实测 ~56%，主为清中段历史 reasoning）+ 防 `middle + summaryMaxTokens` 超摘要模型 CW 致摘要失败回落。权衡：摘要 LLM 不再见中段历史 reasoning（要点仍在正文 / tool_calls，与 context 侧 `stripStaleReasoning` 同取舍）。注：dedup/fold（P6/P11/P8'/P9b）经 `windowStartOf(keepN=0)=0` 在 middle 全压时不生效（全窗口内），故当前主要省 reasoning；中段 tool 结果去重留待后续（需调整 windowStartOf 语义）。
- **Commit 语义改：非压缩步不替换 transcript，压缩 tail 保留原文**：`before` 原每步 `Commit=true` 把 strip 写回 transcript，致压缩轮 `RewriteMessages` 落盘 strip 版本（reasoning/args 滚动丢失），与 `stripStaleReasoning` 注释「持久化留原文」矛盾。现 `FitHistory` 加 `committed` 返回值：非压缩步 `committed=false`（strip 仅本轮 View，transcript 保留原文）；仅压缩步 `committed=true` 替换为 `[head,summary,tail]` 且 tail 不再 strip（原文 reasoning/args 保留）。LLM 所见不变（每轮 strip View），消除压缩轮持久化不对称（落盘完整近期上下文）。代价：transcript 内存略增（原文 vs strip，≈ newMsgs 量级）。`fallback`（摘要失败）仍 strip+committed=true 防循环。

## [4.2.0] - 2026-08-06

> 核心策略彻底外挂：用量估算+预算判定、LLM 失败恢复、工具结果成型+落盘从 `Run` 内联移到默认钩子工厂，`Run` 真正零策略（仅做工具注册 / 上下文拼接 / 调 LLM / 执行工具 / 无 tool_calls 退出五件事）。CLI 行为与 NDJSON 事件契约零变更，属非破坏性 minor。

### Changed
- **核心策略从 `Run` 内联移到默认钩子工厂**：原核心内置的三项策略——零 usage 本地估算 fallback + `MaxTotalTokens` 预算判定、`ErrContextLength` 历史收紧重试、工具结果截断（`trimForHistory`）+ 超限落盘（`toolOutputStore`）——提取为 `NewDefaultOnBudget` / `NewDefaultOnLLMError` / `NewDefaultShapeToolResult` 三个导出工厂。`Run` 不再读取 `LoopConfig` 的策略字段，仅累加真实 usage、不做估算/判定/截断/落盘/错误恢复；`Run` 注释承诺的「核心不做任何上下文管理」至此与实现一致。
- **`OnBudget` 扩签以承载估算**：签名从 `func(step int, total Usage) error` 改为 `func(ctx, step int, in BudgetInput, total *Usage) error`（新增 `BudgetInput` 携带本轮 `ToSend`/`System`/`Tools`/`Resp`，`total` 改指针可累加估算）。原核心内联估算无法外挂的根因——钩子拿不到请求侧上下文——由此消除。
- **`ShapeToolResult` nil 时核心透传原文**：此前 nil 走核心内置 `defaultShapeResult`（截断+落盘）；现 nil = 零成型、透传 `ToolResult.Output`，成型完全交钩子。
- **cmd 层一行装配默认钩子**：`main.go` 用三个 `NewDefault*` 工厂替代原自写的 `OnBudget` 闭包；`store` 连同落盘逻辑整体下沉进 `NewDefaultShapeToolResult`（构造时 `cleanup` 一次，等价原 `Run` 入口行为）。

### Added
- **`OnLLMError` 钩子缝**：LLM 失败路径的唯一开放缝（`BeforeLLM`/`AfterLLM` 均在成功路径）。返回 `recoveredMsgs` + `retry` 时核心替换 transcript 并重试一次；nil = 失败直接上抛。
- **`NewDefaultOnLLMError` / `NewDefaultOnBudget` / `NewDefaultShapeToolResult`**：承载原核心内置策略的默认钩子工厂，cmd 层组装即复用原行为。
- **`BudgetInput`**：`OnBudget` 的请求侧上下文类型。

### Breaking (internal API)
- `LoopHooks.OnBudget` 签名变更、`LoopHooks` 新增 `OnLLMError` 字段、`defaultShapeResult` 移除。相关类型均在 `internal/` 下（外部无法导入），仅影响项目自身调用方（`cmd/miniagent`，已同步）。CLI / 事件契约零变更。

### Notes
- `LoopConfig` 的策略字段（`MaxTotalTokens`/`MaxToolResultChars`/`ToolOutputDir`/`ToolOutputRetention`）保留作配置载体（核心不读，由 `NewDefault*` 工厂读取），避免牵连 `config.go`/`resolve.go`；字段注释已更新为「配置载体」。
- 与 4.1.0 一脉相承：4.1.0 解耦 provider（`LLM`/`Doer` 接口），本版解耦上下文/用量/成型策略。真正库化（移出 `internal/`）仍计划于 5.0.0。

## [4.1.0] - 2026-08-06

> 内部架构重构：核心经接口解耦具体 client、子包化（compaction/event/openai provider）。CLI 行为与 NDJSON 事件契约零变更，属非破坏性 minor。

### Changed
- **核心经 `LLM`/`Doer` 接口解耦具体 client**：`Run` 不再直接依赖具体 HTTP client 类型，改为依赖 `LLM`（`Do` 非流式 + `DoStream` 流式）与更窄的 `Doer`（仅 `Do`，供压缩摘要调用）；新增核心自带 `Provider`（OpenAI 兼容默认实现，组合 `ChatClient`+`StreamClient`）。自定义 provider 实现 `LLM` 即可替换，核心零改动（`a5bad2c`）。
- **LLM 契约与供应商实现分离**：openai 由「唯一内置后端」降级为 core 下的一个 provider 实现，落在 `internal/provider/openai` 子包；LLM 类型/接口提取到 core（`d5381a5`）。
- **compaction 引擎独立成子包**：摘要压缩引擎（超窗摘要中段 + 有损 fallback + 主动裁剪序列 + 静默溢出检测）从 `internal/miniagent` 提取到 `internal/miniagent/compaction`，并拆分为 `assemble`/`budget`/`split` 等文件（`3b2fe5b`、`d5381a5`）。
- **event emitters 独立成子包**：事件发射器提取到 `internal/miniagent/event`（`8c941e4`）。
- **导出供子包复用的 helper**：跨子包共享的 `role`、`KindSummary` 等提升为包级常量并经 `export.go` 暴露，消除子包对未导出符号的依赖（`8228334`）。

### Notes
- 上述均为 `internal/` 内部重构，无对外行为变更。
- 项目定位为「可被集成的库」，但核心公开面（`miniagent`、`compaction`、`event`、`provider/openai`）目前仍在 `internal/` 下，Go 禁止外部模块导入。将这些包移出 `internal/` 的真正库化计划于 **5.0.0**（属 breaking，故单列 major）。

## [4.0.1] - 2026-08-05

> session id 输出契约迁移到 stdout NDJSON、Provider 字段补全、id 随机段扩位。

### Breaking
- **session id 输出契约改为 stdout NDJSON 首条事件**：`-save-session` 新建会话时，session 元数据（与 jsonl 首行 metadata 同构：id/model/workdir/provider/created）作为 stdout 第一条 `session` 事件输出，替代此前的 stderr 文本行 `miniagent: session id: <id>`；外部 stderr grep 该文本行的消费方需改为读 stdout 首行 JSON 的 `id` 字段。
- **`SessionMeta.Provider` 字段补全**：新建会话此前未填 Provider，jsonl 首行与 stdout session 事件的 `provider` 恒为 `""`，与「多 provider 溯源」设计意图及字段定义不符；现填 config 解析出的 provider 名。仅影响此前落盘会话文件的该字段为空（不影响接续读写）。

### Changed
- **`-save-session` 与 `-result-only` 互斥**：subagent fork 无状态、不落盘会话；二者同传 stderr 报错退出码 1（此前未拦，理论上会向 result-only 的纯文本 stdout 掺入 NDJSON）。
- **会话 id 随机段提升到 64 bit**：`generateSessionID` 随机段从 4 字节（32 bit）提至 8 字节（64 bit），同秒并发新建碰撞阈值从 ~2^16 抬到 ~2^32 量级，覆盖 CI 矩阵/批量 fork；`crypto/rand` 失败回落由裸时间戳改为时间戳+pid，避免同秒不同进程必碰撞。

## [4.0.0] - 2026-08-05

> 上下文工程强化（stale 内容主动裁剪/去重/折叠、token 估算对齐真实体积）、codemap 工具、session CLI 重构、移除 `-interactive`。

### Breaking
- **移除 `-interactive` flag**：交互循环（`readTurn` 逐行 turn、跨轮预算/信号管理）与单轮 stdin 读取冗余。多轮对话通过多次调用同一 `-session` 实现；每次调用 stdin 的全部内容（含多行）作为一个 turn 的完整 prompt。
- **`-session` 重构为仅接续已有会话**：改为纯 id 校验（仅允许字母/数字/`-`，禁路径与扩展名注入），文件须已存在、不存在则报错退出；新增 **`-save-session`** 新建会话并落盘（id 内部生成，stderr 打印供接续），二者互斥。移除 `-session` 的路径双语义与「文件不存在则新建」行为；`CLIOverrides` 去掉 Session 透传；subagent fork 改无状态（不再落盘会话、不再注入父 session id）（`4cdc55e`）。

### Added
- **`codemap` 工具**：递归输出带缩进层级的目录树概览（目录标注子条目数），填补 glob 扁平列举与 read 单文件之间的结构感知缺口。参数 `path`（默认 workdir）+ `depth`（默认 3，0 同义；<0 不限）；条目上限 500，排除 `.git` 与符号链接，default 模式经 confineWrap 限定 workdir 子树。
- **跨消息去重/折叠（P6/P8'/P9b/P11）**：保留窗口（最近 N 条 assistant）外——同 `(path,offset)` 的 read 结果按时间序最后一次保留、更早压占位（P6）；被更晚同 path 成功 write/edit 取代的 write/edit args 整条折叠为 path+占位（P8'，成功判定依赖 tool 消息新增的 `IsError` 回填，仅用于持久化与判定、不泄漏给 LLM）；规范化同义 shell command 去重（P9b）；同 path 存在更晚成功写入的 stale read 结果折叠（P11，补「edit 后未再 read」盲区）。均仅改 context 侧拷贝，不动 session 持久化与 tool_calls/tool 配对（`825e224`、`9b596b6`）。
- **主动压缩 stale write/edit args（P4）**：非最近 N 条 assistant 的 write/edit 大 Args（content/old_string/new_string，写成功后已落盘纯占位）在 context 侧压为前缀+省略标记，保留 path（`5b49ea2`）。
- **主动清理 stale reasoning（P1）+ 保留窗口超长 reasoning 中段截断（P7）**：非最近 N 条 assistant 的 Reasoning 主动清空（思考模型 reasoning 常达正文数倍，每轮原样回灌是隐性 token 大户）；保留窗口内单条超 `run.context_keep_reasoning_chars`（默认 4000 rune，0=默认/负数=关闭）的做头 1/4 + 尾 3/4 分段截断（`a26c88e`、`5ec990c`）。

### Changed
- **token 估算对齐真实体积**：`estimateTokens` 信封开销从 flat 400 改为 base + 按消息数/tool_call 数线性增长（长 ReAct 会话嵌套 function 对象随条数累积，flat 估算系统性低估、压缩触发偏晚易撞 context_length_exceeded）；tools schema 开销从 flat per-tool 常数改为序列化实际 JSON schema 走 CJK/non-CJK 启发式（`5ec990c`、`5b49ea2`）。
- **工具结果截断头尾分段**：`trimForHistory` 对 shell/grep/script 类结果改「头 N/4 + 尾 3N/4」分段截断，保留尾部错误结论；read/edit 保持 head-only 符合分段读大文件语义（`a26c88e`）。
- **主动裁剪序列统一**：`FitHistory` 未超窗/超窗两分支的 P1/P4/P6/P7/P8'/P9b/P11 序列抽为 `applyContextStrips` 复用；logger Debug level 记录各阶段 token 差值与 fit 前后总量（Info level 零开销）（`43c8fdf`）。

## [3.5.1] - 2026-08-04

> 沙箱 confineWrap 空 path 直通、memory 抽取兼容 agnes 等要求 user role 的端点。

### Breaking
- **`memory.extract_prompt` 移除第 3 个占位符 `%s`(对话)**：对话内容现作为 user message 传入（不再内联进 system prompt），以兼容要求 messages 含 user role 的端点（如 agnes，此前会 400 `No user query found in messages`）。自定义 `extract_prompt` 现仅支持 `%d`(条数)/`%s`(已有记忆) 两个占位符。

### Fixed
- **会话结束记忆抽取对 agnes 等端点必失败**：`ExtractMemory` 此前把对话全塞 system 且不发 user message，要求 user query 的端点直接 400、记忆永不落盘（best-effort warn 仅走 stderr，常被调用方吞掉）。现 transcript 经 `renderTranscript` 渲染后作为单条 user message 传入。
- **沙箱 confineWrap 误伤 grep/glob**：此前对空 path 一律拒绝，导致 grep/glob（path 可选，默认 workdir）在 default 模式下基本不可用。改为仅当 args 能解析出非空 path 时才做越界校验，path 缺省/空或 JSON 非法时直通 orig（各工具自身校验 path）。

## [3.5.0] - 2026-08-04

> compaction/memory 跨 provider、会话结束自动记忆抽取、移除 `-key-file` CLI flag。

### Breaking
- **移除 `-key-file` flag**：API key 统一经 `provider.key`（config）或 `$MINIAGENT_API_KEY`（env）注入；`setup_keyfile_unix.go` / `setup_keyfile_windows.go` 已删除。密钥隔离依赖运行用户的 OS 权限与 config 文件 `0600`（`1da0de6`）。

### Added
- **compaction / memory 支持跨 provider 模型**：`compaction.model` / `memory.model` 可写 `provider/model`（三级回落：`X.model → defaults.model → 主会话模型`）；不同 provider 时自动新建独立 `ChatClient`（按 provider 名去重复用）（`59b759a`、`b6dcc00`）。
- **会话结束自动抽取项目记忆**：复用到 `memory.model` 的 client，对有过工具调用的 transcript 调用 LLM 抽取 ≤`memory.max_per_session` 条事实写入 `.miniagent/memory.jsonl`；best-effort（失败仅 warn），不计入 token 预算，信号中断跳过（`623a241`）。

### Fixed
- **`SetMemoryRecentN` 在 `loadProjectRules` 前生效**：修复 `run.memory_recent_n` 对启动快照的注入顺序（`f49807d`）。

### Changed
- **`memoryExtractor.extract` 内部用 `context.Background()`**：避免 `-max-duration` 到期后抽取立即失败（`f49807d`）。
- **`main.go` 提取 `secondaryClient` 闭包**：compaction / memory client 构建逻辑统一，减少重复（`b6dcc00`）。

## [3.4.0] - 2026-08-03

> 安全与健壮性硬化（P0-P2）、Windows 平台补齐、集成测试与发版准备。

### Security
- **session 保存期间忽略 SIGINT/SIGTERM**：非交互与交互模式均在 `AppendMessages`/`RewriteMessages` 期间临时忽略信号，防止 session 文件在半写状态被截断或残留临时文件；交互模式保存后重新注册信号通道（`cmd/miniagent/main.go`、`cmd/miniagent/interact.go`）。
- **key-file 读取硬化**：通过 `O_NOFOLLOW` 拒绝最终分量为符号链接的文件，防止被指向敏感文件；限制文件大小 ≤64KiB；权限宽松时 stderr 警告（`cmd/miniagent/setup.go`）。
- **default 模式路径约束**：`read`/`grep`/`glob` 在 default 模式下强制受限在 workdir 子树；`checkConfine` 强化符号链接检查；`glob` 不再吞掉 `WalkDir` 错误（`cmd/miniagent/tools.go`、`internal/miniagent/tool_glob.go`）。
- **请求体大小双重护栏**：marshal 前按消息长度估算，marshal 后再校验实际 JSON 字节，超过 4MiB 直接拒绝，防止超大请求 OOM/烧钱（`internal/miniagent/wire.go`）。
- **脚本/参数注入防护**：`script_<name>` 工具参数经 `shellQuote` 转义，禁止 `-` 开头参数，防止被构造进额外 flag（`internal/miniagent/tool_script.go`）。
- **正则复杂度限制**：`grep` 工具对 `regexp/syntax` 解析后的 AST 统计节点数，超过阈值直接拒执行，防止 ReDoS（`internal/miniagent/tool_grep.go`）。

### Fixed
- **roleSystem 消息持久化**：summary request 等内部 system 消息原样落入 session（之前被过滤导致上下文/持久化不一致）（`internal/miniagent/loop.go`）。
- **零 usage 预算熔断失效**：LLM 未返回 usage 时，用本地 token 估算兜底，避免 MaxTotalTokens 被静默绕过（`internal/miniagent/loop.go`）。
- **流式 OnDelta 错误传播**：`OnDelta` 返回 error 时终止流解析并上抛，不再静默吞错（`internal/miniagent/stream_parse.go`）。
- **ListModels 无重试**：对 429/5xx 与网络错误增加与 chat 调用一致的重试退避（`internal/miniagent/models.go`）。
- **glob 缺失根目录错误**：`filepath.WalkDir` 的根不存在/不可读错误现在返回给调用方（`internal/miniagent/tool_glob.go`）。
- **负 timeout 配置被接受**：`run.http_timeout`/`run.shell_timeout` 等负值在解析阶段报错（`cmd/miniagent/setup_http.go`）。
- **thinking 级别未校验**：配置或 CLI 传入非法 thinking 值时返回明确错误（`internal/miniagent/config.go`、`resolve.go`）。
- **memory.jsonl 无界增长**：增加 1MiB 上限与最近 N 条轮转（`internal/miniagent/memory.go`）。

### Added
- **Windows 平台实现**：新增 `internal/miniagent/platform_windows.go`，提供 `setPGID`/`killProcessGroup`/`openNoFollow`/`lockSession` 的 Windows 等价实现；Unix 专属测试迁移到 `*_unix_test.go`，新增 `platform_windows_test.go`。
- **集成/冒烟测试**：新增 `cmd/miniagent/main_test.go` 的 e2e 用例覆盖单次 session 追加、交互模式两回合持久化、`-max-duration` 到期退出码。

### Changed
- **`-list-models` 输出统一为 `provider/model_id`**：单 provider 也带前缀，与多 provider/`-model` 筛选路径格式一致；移除 `ListAvailableModels` 与 `providerForListModels`（静态回落逻辑并入 `ListAllModels`）。

## [3.3.0] - 2026-08-03

> 双层 `.miniagent/` 规则查找、多 provider `-list-models` 聚合、HTTP 工具抽离；修正发版前评估发现的旗舰示例不可加载与 run.* 配置键漏装配。

### Added
- **多 provider `-list-models` 聚合**（256c875）：多 provider 时聚合输出 `provider/model_id`，`-model` 可筛选单 provider；单 provider 保持纯 model id。
- **双层 `.miniagent/` 规则查找**（1ac831e）：`persona.md`/`rules.md`/`scripts.json`/`memory.jsonl` 优先 workdir、回退 `~/.miniagent/`（workdir > home > 空）。
- **HTTP 工具函数抽离**（1ac831e，`setup_http.go`）：`httpTimeoutFromConfig` 等供 list-models 与 buildLLM 复用。
- **`config.example.json` 配置模板**（1c1687c）：带注释的旗舰示例。

### Changed
- **默认 config 查找简化**（c51d91c）：仅 `~/.miniagent/miniagent.json`，移除 `./miniagent.json` 回退与 `${VAR}` 展开；不存在则报错。
- 测试文件按主题拆分为独立文件（bfab731）。

### Fixed
- **sandbox `checkConfine` 强化符号链接检查**（1c1687c）。
- **`resolveRun` 漏装配 4 个 run.\* 配置键**：`summary_max_tokens`/`grep_max_matches`/`memory_recent_n`/`context_trim_tool_chars` 自 v3.2.3 声明但从未赋值，配置值静默失效（main 的 `Set*` 收到 0 当未设置而回落内置默认）；现已透传到 `ResolvedRun`，对应 `Set*` 生效。
- **`config.example.json` 旗舰示例不可加载**：openai provider 显式 `thinking.field:"reasoning_effort"` 被 `thinkingFieldBlacklist` 拒（误配）；删除该映射块——默认即注入 `reasoning_effort`，无需显式声明。

## [3.2.5] - 2026-08-02

### Changed
- 提升 tool 结果/shell 输出/session 等默认上限（494f5f2、44ed373）。

### Fixed
- `session_test` 大字符串触发 race 超时：TestMain 覆盖上限调整为 1MB（5571d48）。

## [3.2.4] - 2026-08-02

### Changed
- 统一 timeout/limit 配置化实现模式（refactor，1bd76fb）。

## [3.2.3] - 2026-08-02

### Added
- 新增 `summary_max_tokens`/`grep_max_matches`/`memory_recent_n`/`context_trim_tool_chars` 配置项（4044c65）。

## [3.2.2] - 2026-08-02

### Added
- 文件读取/shell 输出/session 默认上限提升并实现可配置：`max_read_file_bytes`/`max_shell_output_chars`/`max_session_bytes` 等（f776bdd）。

## [3.2.1] - 2026-08-02

### Added
- 文件操作（read/grep/glob/edit）与 write 超时可配置：`file_op_timeout`/`write_timeout`（000a54a）。

### Internal
- 测试适配新的工具构造函数超时参数（a2e1465）。

## [3.2.0] - 2026-08-02

> 落地内部架构评估路线图：核心引擎重构（P3/P4）、减法（S1/S2/P2/S3）、
> 常量策略化（S4）、自进化机制（P0/P1/P5）。**破坏性变更**见 Removed。跳过 P6（反馈闭环）与 S5（schema 生成）。

### Added
- **项目级规则目录 `.miniagent/`**（P0/P1/P5，`cmd/miniagent/project.go` + `internal/miniagent/memory.go`/`tool_script.go`）：`workdir/.miniagent/` 下 `persona.md`（取代默认 system prompt 作身份基线，优先级 persona>rules>defaults）、`rules.md`（追加「## 项目规则」）、`scripts.json`（每条注册为 `script_<name>` 工具，复用 shell 的安全策略）、`memory.jsonl`（项目级记忆）。核心引擎不感知具体项目，只「知道如何发现项目规则」。
- **项目记忆工具**（P5）：`read`/`write` 的保留路径 `path="memory"` 路由到 `.miniagent/memory.jsonl`——read 渲染记录、write **追加**一条 `{type:"note",content}`（特殊语义：追加而非覆盖）；最近 10 条记忆注入 system prompt。
- **项目脚本工具**（P1）：`.miniagent/scripts.json` 声明的命令注册为 `script_<name>` 工具，复用从 shell 工具提取的共享 `runShellCommand`（mode 黑名单 / env 剥离 / 超时 / 进程组 / 输出截断），继承 default 模式约束。
- **常量策略化**（S4，`config.go`/`resolve.go`/`types.go`）：`run.max_tool_result_chars`/`max_file_result_chars`/`max_parallel_tools`/`context_keep_recent`/`summary_max_chars` 五项可在 `miniagent.json` 覆盖（`<=0` 用内置默认）。
- **`FitHistory` 单一上下文预算入口**（P3，新 `internal/miniagent/context.go`）：合并 `compaction.go`+`history.go`，暴露 `FitHistory(msgs, ContextBudget)` 返回 `(out, summary, summarized, usage, err)`；`ContextBudget.Summarize` 回调解耦与 client，便于测试注入。`loop.go` 的 compaction 穿插逻辑移出。
- **拆分 `HTTPClient` → `ChatClient` + `StreamClient`**（P4，`client.go`/`stream.go`/`client_util.go`/`stream_parse.go`）：非流式（带总 Timeout）/流式（无 Timeout）各一简单类型，共享纯函数（`shouldRetryStatus`/`parseRetryAfter`/`capRetryDelay` 等），消除单一 client 的 `sync.Once` 双缓存复杂度。`Run(ctx, chat, stream, …)`。
- **迭代上限后注入总结 prompt**（`internal/miniagent/loop.go` 阶段 3）：当 `step == iterLimit` 且刚执行完工具时，注入一条系统消息请求 LLM 输出总结性回复（而非继续调工具）。允许 1 次额外 LLM 调用；若仍请求工具或出错，回落 `finishMaxIterations`（`text` 为空）。
- **输出格式约束**（`cmd/miniagent/prompts.go`）：默认 system prompt 新增约束——最终回答不用多级标题（`###/####`）、不用表格，纯段落或简单列表即可，防过度排版。

### Changed
- **`edit` 工具支持 `edits` 数组**（S3）：单段（`old_string`/`new_string`）与多段事务（`edits`，全部成功才写盘、任一失败不改）合一；`edits` 与 `old_string` 互斥，同传报错。
- **`loop.go` 拆分**（修既有 324 行超限）：`loop.go`（Run+常量）/`loop_tools.go`（handleToolCalls/runToolsParallel 等）/`loop_extra.go`（callLLM*）；`callLLM` 符号保留供 `thinking_test.go`。
- **`defaults.summary_request` / `defaults.summarizer_prompt` 配置覆盖**：移出 CLI（仅 config），优先级 config > builtin。

### Removed（破坏性）
- **裸 CLI 模式**（S1）：删除 `-chat-url` / `-models-url` 与「软失败退裸模式」。始终要求 config；默认 `./miniagent.json` 不存在时**写一份最小模板**（`${CHAT_URL}`/`${MODEL}`）再加载。`Resolve` 不再处理 `cfg==nil`。
- **9 个 CLI flag**（P2/S1/S2）：`-chat-url`、`-models-url`、`-summary-request`、`-summarizer-prompt`、`-max-tokens-total`、`-context-window`、`-max-duration`、`-shell-timeout`、`-migrate-session`。这些参数仍在 `miniagent.json` 可配（`defaults.*` / `run.*`），仅不再暴露为 flag。
- **`-migrate-session` 子命令 + `MigrateSession`/`loadLegacySession`**（S2）：v2 JSON 迁移为一次性需求，移除常驻 CLI。
- **`multi_edit` 工具**（S3）：并入 `edit`（`edits` 数组）。
- **`compaction.go` / `history.go`**（P3）：合并入 `context.go`。


## [3.1.0] - 2026-08-02

> 迭代上限后注入总结 prompt、输出格式约束、常量策略化（S4）初版。

### Added
- **迭代上限后注入总结 prompt**（`loop.go` 阶段 3）：当 `step == iterLimit` 且刚执行完工具时，注入系统消息请求 LLM 输出总结性回复（而非继续调工具）。允许 1 次额外 LLM 调用；若仍请求工具或出错，回落 `finishMaxIterations`（`text` 为空）。
- **输出格式约束**（`prompts.go`）：默认 system prompt 新增约束——最终回答不用多级标题（`###/####`）、不用表格，纯段落或简单列表。

### Changed
- **`edit` 工具支持 `edits` 数组**（S3）：单段（`old_string`/`new_string`）与多段事务（`edits`，全部成功才写盘、任一失败不改）合一。
- **`loop.go` 拆分**：`loop.go`（Run+常量）/`loop_tools.go`（handleToolCalls/runToolsParallel 等）/`loop_extra.go`（callLLM*）。

## [3.0.0] - 2026-08-01

> v3 重大重构：双运行形态（config + 裸 CLI）、会话 jsonl append-only、摘要式压缩、
> 思考级别、权限模式（default/auto）、subagent fork 引导。**破坏性变更**见 Removed。

### Added
- **配置系统**（`internal/miniagent/config.go` + `resolve.go`）：`./miniagent.json` 或
  `-config <path>`，支持多 provider、`${VAR}` 展开、优先级 cli>config>builtin、校验。
  双运行形态：config 模式（完整）与裸 CLI 模式（`-chat-url`+`-model`+key 隐式单 provider，
  向后兼容）。默认 config 不存在 = 软失败退裸模式；显式 `-config` 不存在 = 硬错误（审查 v3 #1/#7）。
- **会话 jsonl**（`session.go` 重写）：append-only，首行 metadata（id/model/workdir/provider/created）
  + 每条 message 行（`type=message`）。`Message.Kind` 标记（如 `summary`）仅 session 层，wire 不泄漏。
  `Result.NewMessages` 收集本轮新增，main `AppendMessages` 增量追加（不重写全量）。`-migrate-session`
  把 v2 JSON 数组转为 jsonl。`ResolveSessionPath`：id 在 `session.dir` 解析，含 `/`/`.` 视为路径。
- **摘要式压缩**（`compaction.go` 新增）：Run 入口阶段 1 `applyCompactionBarrier`（屏障最新 summary
  之前的旧历史，仍留文件）；loop 每步阶段 2 超 window 80% 时 `compactWithSummary` 把中段摘要为单条
  `KindSummary` 消息（既进 context 又落盘）。中段 `validateToolPairing` 断言；失败/无中段回落有损
  `compactHistory`，仍超裁到最近轮，再超报错终止（避免循环烧请求，审查 v2 #8 + v3 #4）。
- **思考级别**（`-thinking`）：off（默认）/minimal/low/medium/high/xhigh/max，wire 透传
  `reasoning_effort`；provider `ThinkingMapping` 可覆盖字段名与取值映射。一次性降级：识别 thinking
  致 400（启发式 body 特征）→ 去字段重试一次 + warn（审查 v2 #7）。
- **权限模式**（`-mode default|auto`）：default（默认）= 写工具限 workdir 子树（薄版 `checkConfine`，
  去 EvalSymlinks）、shell 词边界拒 11 个提权器（`\b(sudo|su|doas|pkexec|gsudo|run0|setpriv|nsenter|unshare|chroot|machinectl)\b`）；auto 无限制。default 不构成安全边界
  （审查 v3 §6）。`buildTools(workdir, timeout, mode)`。
- **subagent fork**（`-result-only`）：仅输出 result.text（与 `-stream` 互斥），失败输出 `error: <msg>`
  + 退出码 1。system prompt 注入 subagent 引导（config 绝对路径 + 父 session id + 嵌套≤2 层约束，
  审查 v3 #6/#8）。
- **多 provider client**：`HTTPClient` 改 `ChatURL`/`ModelsURL`（完整 URL，构造时 parse 缓存，审查 v3 #10），
  `validateURL` 抽出。`ListAvailableModels`：ModelsURL 空直接静态 models（不 GET），皆空报错（审查 v3 #5）。
- 新增 flag：`-config` / `-chat-url` / `-models-url` / `-mode` / `-thinking` / `-result-only` / `-migrate-session`。

### Changed
- `-model`：config 模式 `provider/id`；裸模式裸 id。
- `-session`：id 或路径（id 解析为 jsonl）；不再整体写回，改 append-only。
- `-list-models`：经 `ListAvailableModels`（带静态回落）；不要求 `-model`。
- `cmd/miniagent/setup.go` 拆出（`loadConfigOrBare`/`collectOverrides`/`resolveFinalKey`/`validateConversation`/
  `loopCfg`/`buildHooks`/`providerForListModels` 等），main.go 仅入口编排。
- interactive 模式：有 `-session` 时以文件为唯一真源（每轮 LoadSession → Run → AppendNewMessages），
  过滤统一在 Run 入口（审查 v2 #3 + v3 #3）。

### Removed（破坏性）
- `-base-url`、`-approve`、`-confine` flag 与 `$MINIAGENT_BASE_URL` env。
- session 旧 JSON 数组格式（用 `-migrate-session` 迁移）。
- `SaveSession` / `checkApprove` / `HTTPClient.BaseURL` / `endpoint()`。

### Fixed（七轮「审查-修复-对抗复核」加固，用户可观察项）
- shell default 模式词边界拒提权器从 `sudo|su` 扩到 **11 个**（追加 doas/pkexec/gsudo/run0/setpriv/nsenter/unshare/chroot/machinectl），覆盖更多 Linux/BSD 提权路径。
- shell 子进程 env 在剥离 `MINIAGENT_*` 前缀基础上，追加剥离变量名含密钥关键字（KEY/TOKEN/SECRET/PASSWORD/CREDENTIAL/PWD/PASS/PASSPHRASE/AUTH/PAT，PAT 排除 PATH）的第三方凭证变量，收窄泄漏面（非隔离边界，`/proc/$PPID/environ` 仍可读，彻底方案 `-key-file`）。
- 交互输入 `readTurn` 改逐字节封顶 `maxPromptBytes`，防管道灌入超长无换行输入致无界分配 OOM（超限行 emit error 跳过、不喂给 Run）。
- SIGINT/SIGTERM 走退出码 **130**（128+SIGINT）干净退出，不再 emit `error` 事件（区别于真故障）。
- `-max-duration` 到期走 `DeadlineExceeded` 干净退出码 1，不 emit `error`（与信号取消语义对齐）。
- 流式 `DoStream` 端点断流（连接中断/非 200）改为透明重试（复用非流式重试策略），流式用法不再因瞬时断流失败。
- 交互模式本轮触发摘要压缩（`Result.Compacted`）时机会性 rewrite session 文件，真正丢弃被屏障的旧 summary，长会话 session 文件不再永不压缩。
- 摘要消息的 token 计入 `-max-tokens-total` 预算（跨轮预算可靠性）。
- thinking 致 400 降级后清 `baseCfg.ThinkingLevel`，交互模式下一轮不再重传原值重复撞 400。
- session jsonl append 经 `flock` 跨进程互斥（`withSessionLock`，单进程交互主用法不受影响；跨进程并发仍建议单写者）。
- key-file 读取加 `O_NOFOLLOW`（拒最终分量符号链接，防被换为指向 `/etc/shadow` 的软链）+ 64KiB 上限（防误读/构造巨型文件），并按平台拆分（Windows 回退 `os.Open`）。
- `-max-tokens-total` 预算熔断依赖端点 `resp.Usage`：端点不 honor `stream_options.include_usage` 时熔断静默失效（仅 slog warn），见 README。

## [2.0.0] - 2026-08-01

### Added
- `-list-models` flag：GET /v1/models 列出端点可用模型 id 后退出（早退路径，仅需
  key + base-url，不经对话流程）。`HTTPClient.ListModels` 复用 endpoint/鉴权。
- `-confine workdir` flag（默认空=free）：把写工具（write/edit/multi_edit）约束在
  workdir 子树内（`confineWrap` 执行前校验 args.path，`EvalSymlinks` 防符号链接逃逸，
  越界拒绝）。读工具与 shell 不约束（free 读无副作用；shell 沙箱靠 OS）。不改 free 默认。
- `fetch` 工具：抓取 http(s) URL 转 plain text（剥 script/style/标签，反转义实体）。
  SSRF 防护（`checkSSRF` 拒绝 loopback/私网/链路本地/未指定，限 http/https，重定向
  上限 5 跳每跳重检）；body 上限 200KB、输出超 20000 字符截断。彻底防护（DNS
  rebinding）仍需调用方网络隔离。
- `multi_edit` 工具：对同一文件的多处文本顺序精确替换（事务——全部成功才写盘，
  任一失败不改文件）。抽 `applyOne` 复用匹配逻辑，edits 按序基于前一处结果匹配。
- `todo` 工具：进程内任务清单（`add`/`update`/`list`/`complete`/`delete`），
  `sync.Mutex` 保护支持并行调用，不进 session（过程态）。默认 system prompt 引导
  复杂任务先拆解再逐步推进。
- 交互模式（`-interactive` flag，默认关）：循环读取 prompt（每行一个，空行跳过，
  EOF 退出），多轮对话进程内累积上下文（每轮 History = 上轮 transcript），每轮成功
  后增量写回 session；单轮错误不退出会话。非交互保持单次 stdin 行为。
- 工具前确认（`-approve` flag，默认 all）：`OnToolUse` 返回 `ErrToolDenied` 时
  `handleToolCalls` 跳过该工具（回填拒绝结果）、不终止循环；其他 error 仍终止。
  `dangerous` 仅 shell/write/edit 读 stdin 确认，`always` 全部确认；非交互（stdin
  已被 prompt 读光 → EOF）即拒绝危险工具，不静默放行。
- 主动历史管理（`-context-window` flag，默认 0=不限）：`Run` 每步前用
  `estimateTokens`（CJK 感知字符近似）估算，超窗口 80% 时 `compactHistory` 按
  「轮」成组删中段（保 tool_calls/tool 配对，首轮 + 末 6 轮保留）。被动
  `trimHistoryForContext`（ErrContextLength）仍作兜底。
- 流式输出（`-stream` flag，默认关）：`DoStream` 走 SSE，增量发
  `text_delta`/`reasoning_delta` 事件；流式 `tool_calls` 按 index 累积、末 chunk
  `usage` 聚合。`LoopConfig.Stream` 控制，`OnDelta` 回调推出。非流式 `Do` 仍为默认。
- `tool_result` NDJSON 事件：工具执行后输出结果（`output` 截断到 2000 字符，
  完整版仍入历史回灌 LLM）；`exit_code` 仅 shell 工具输出。
- `Run` 回调从散参 `onToolUse` 改为 `LoopHooks` 结构（`OnToolUse`/`OnToolResult`/
  `OnDelta`，内部 API breaking，仅 `cmd/miniagent` 调用）。
- `-max-iterations` flag：单轮 LLM 调用上限（0=默认 20）。原 `maxIterations`
  硬编码提升为 `LoopConfig.MaxIterations` 可配置，否则稍复杂任务必撞顶。
- `-shell-timeout` flag：单条 shell 命令超时（0=默认 60s），仍受 `-max-duration`
  总上限约束。覆盖大仓库 `go test` / `npm install` 等长命令。
- `-max-tokens-total` flag：单轮累计 token（输入+输出）预算上限；超限以
  `ErrBudgetExceeded`（error 事件 + 退出码 1）终止。判定用端点返回的真实 usage。
- `Tool.ResultLimit` 字段：工具结果入历史字符上限按工具区分。read/edit 取
  `maxFileResultInHistory=8000`——原统一 2000 截断会把读到的代码尾部丢掉（代码
  场景头号问题）；shell/grep/glob 仍默认 2000。
- reasoning 模型支持：`Message.Reasoning` 字段，wire 解析 `reasoning_content`/
  `reasoning`（双兼容），assistant 消息携带思考链并以 `reasoning_content` 回灌，
  随 session 落盘。
- context 超限降级：端点 400（context_length）返回 `ErrContextLength`，Run
  收紧历史（清 reasoning + 把 tool content 压到 `contextTrimToolChars`）后对
  本步重试一次（新增 `history.go`），仍超则上抛。只降级一次，避免循环烧请求。
- `grep` 工具：递归正则搜索文本文件，输出 `path:lineno:line`，命中 200 行封顶，
  跳过 `.git`/符号链接/二进制。
- `glob` 工具：递归列举匹配 filepath.Match 通配的路径，命中 500 条封顶。
- `edit` 加 `replace_all` 参数：替换全部匹配处。
- `-key-file` flag：从文件读取 API key（首尾空白截断），优先于
  `$MINIAGENT_API_KEY`。规避环境变量经 `/proc/$PPID/environ` 泄漏给 shell 子进程；
  文件 loose 权限（group/other 可读）时 stderr 警告。隔离不在代码层控制，由运行
  用户 OS 权限决定（见 README「运行隔离」）。
- 代码向默认 system prompt（ReAct 工作流：先观察→后修改→改后验证→失败复盘）；
  read/write 工具描述补全。

### Changed
- **breaking（外部可观察）**：`shell` 工具非 0 退出的 `is_error` 从 `true` 改为
  `false`（命令的合法结果，非执行失败），新增 `exit_code` 字段表达退出码。消费方须
  改据 `exit_code` 判命令成败——这是定为 2.0.0 的主因。
- **breaking（内部 API，仅 `cmd/miniagent` 调用，外部契约不变）**：`Run` 的
  `onToolUse` 散参改为 `LoopHooks` 结构；`buildTools` 新增 `confine` 参数；`Request`
  新增 `Stream` 字段；`ToolResult` 新增 `ExitCode`。
- `shell` 工具退出码结构化：`ToolResult` 新增 `ExitCode` 字段；非 0 退出从
  `IsError=true` 改为 `IsError=false` + `ExitCode=N`（命令的合法结果，非执行失败）；
  超时/启动失败 `ExitCode=-1`（`exitCodeNotSet`）。LLM 改据 `ExitCode` 判命令成败，
  `IsError` 仅表「命令未正常执行」。
- `ShellTool` 签名 breaking：新增 `timeout time.Duration` 参数。
- 默认 system prompt 从「简洁助手」改为代码向工程 prompt（可用 `-system` 覆盖）。
- 工具数 4 → 6（新增 grep/glob）；`buildTools` 签名 breaking（新增 `shellTimeout`）。

## [1.1.0] - 2026-08-01

### Added
- `-session <path>` flag：会话接续。文件存在则加载 `[]Message` 历史作为上下文，
  Run 成功后把完整 transcript 原子写回（0o600）；Run 出错不写回。思考内容
  （reasoning）不入上下文也不落盘（`Message` 无对应字段）。
- `LoadSession` 校验：文件大小上限 4 MiB、role 合法性、tool_calls/tool 消息
  一一配对，损坏即报错退出；`SaveSession` 拒绝空 transcript。
- BaseURL 为 `http://` 且非 loopback 时 stderr 警告 API key 明文上链。
- `-log-level <debug|info|warn|error>` flag：替代硬编码 debug 级别，默认 `info`。

### Fixed
- `edit` 拒绝非 regular 文件（FIFO/设备等），与 `read` 对齐，消除 open 永久
  阻塞主因；`read`/`edit` 增加内置 30s 操作超时兜底（挂起的文件系统不再卡死
  整轮）。
- `edit` 读取改单次 open + `LimitReader` 封顶，消除 Stat/Read 间 TOCTOU 与
  无界分配；`LoadSession` 同样改单次 open + `LimitReader`。
- 工具执行获取并发信号量改为可被 ctx 取消：整体取消后排队调用直接放弃。
- `shell` 输出达上限时主动 kill 进程组并关闭管道，高输出命令不再被误判为
  超时。
- `Retry-After` 头缺失与显式 `0` 区分：缺失按退避等待，显式 `0` 立即重试。
- 注册重名工具时输出 `Warn` 日志。
- 终态事件（error/result）写 stdout 失败时兜底写 stderr，不再只剩退出码。
- BaseURL 警告改用 `u.Redacted()`，不再把 userinfo 凭证落入 stderr；loopback
  判定改用 `net.IP.IsLoopback()`，覆盖整个 127.0.0.0/8（`127.0.0.2` 等不再误
  报警）。
- 端点返回空 `choices`（内容过滤/代理故障）现在报错，不再被当作"成功的
  空回答"（退出码 0、text 为空）。

### Changed
- `Run` 的 `Result` 新增 `Messages`（全量 transcript，所有返回路径均带回）；
  `LoopConfig` 新增 `History`（历史前缀）。最终 assistant 文本现在会追加到
  transcript 末尾（此前只经 `Result.Text` 返回）。
- 工具参数描述中的路径基准术语统一为 `workdir`（原 `workspace_root`，与
  CLI flag 一致）。
- 工具重命名：`read_file` → `read`、`write_file` → `write`、`edit_file` → `edit`
  （`shell` 不变）。工具 schema 属于外部契约，消费方需同步更新。
- 安全注释去承诺化：`O_NOFOLLOW` 仅拒最终路径分量的符号链接（中间目录不
  校验）、`scrubEnv` 仅剥 `MINIAGENT_*` 前缀（非密钥隔离边界，`/proc` 可读
  exec 前环境）。代码行为不变，README 已声明 free 模式边界由调用方保证。

## [1.0.0] - 2026-07-26

首次稳定版。锁定外部契约（CLI flags / NDJSON 事件结构 / 工具 schema）。

### Added
- LLM 请求最小重试：429 / **500** / 502 / 503 / 504 + 网络错误自动重试 2 次，
  指数退避（500ms 起，2× 增长），支持 `Retry-After` 头（秒数与 HTTP-date），
  单次封顶 8s。重试用尽后错误信息补 `after N retries:` 前缀便于排错。
- `-max-duration` flag：整体墙钟上限（覆盖所有 LLM 调用 + 工具执行），`0` 不限。
- 显式声明平台支持范围：仅 Linux/macOS（Unix），Windows 不支持。

### Changed
- `shell` 工具子进程环境剥离**所有 `MINIAGENT_*` 前缀变量**（`API_KEY` /
  `BASE_URL` 等），避免宿主配置与密钥泄漏给 LLM 派生的命令；其他环境变量仍
  按原样继承。
- `read_file` 输出统一带行号（`N │ line`），不再区分 offset/limit 是否提供；
  `offset` 超出文件行数作为 `IsError` 返回；空文件返回空串（不再输出伪空行）；
  **拒绝非 regular 文件**（FIFO/设备/socket，否则会无限阻塞 open）；
  **拒绝二进制内容**（含 NUL 字节，避免乱码污染 LLM 上下文）。
- BaseURL 校验失败时错误信息显式提示"缺少 scheme 或 host"。
- 单次 LLM 调用失败时日志改为 `llm call failed, retrying` + `failed_attempt`
  字段，避免与"重试第 N 次"语义混淆。

### Removed
- 删除过期内部审查文档 `REVIEW_REPORT.md`、`LOC_ASSESSMENT.md`（描述的
  记忆/webfetch/list-models/SSRF 等功能已在 v0.x 重构中删除）。
