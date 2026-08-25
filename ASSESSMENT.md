# miniagent 评估报告（第四轮）· 规范与质量全面复审

- 日期：2026-08-26 · 审查对象：HEAD 9e62c71（工作树干净）
- 方法：verify-gate 实测 + 增量深读（第三轮基线 248b53c 之后 ~30 个提交：minisession 远程会话全链路、WebUI 配置漂移/服务重载、replay user_prompt 事件、.agent 记忆体系重构）+ 文档一致性核对
- 实测：`make verify` 全绿（gofmt 空 / build / vet / `go test -race` 14 包 **610 测试 0 失败** / golangci-lint 0 issues / 行数上限 / memory integrity PASS）
- 覆盖率（-cover，非 race）：metrics 100% / compaction 90.9 / httpretry 90.5 / tools 86.1 / webstatic 85.7 / config 85.4 / policy·event 85.2 / session 84.0 / miniagent 核心·cmd 82.7 / openai 78.3 / text 71.1
- 规模：225 个 .go 文件；非测试 13,291 行 / 测试 16,620 行（**测试:代码 ≈ 1.25:1**）；go.mod 零 require（go 1.25）；前端 vanilla ES modules 无构建链（20 文件，最大 293 行）

## 0. 总体结论

**规范与质量均维持三轮以来的高位，且上一轮两项 P3 待决策项均已落地。** 本轮新发现 **0 高 0 中 5 低**——全部为文档滞后与门禁盲区，无代码缺陷、无安全回退、无契约破绽。第三轮以来的新增量（远程会话集成）质量突出：fail-fast 语义、404→`os.ErrNotExist` 对齐、翻页修复、在途删除守卫均经测试固化。

| 目标 | 评级 | 依据 |
|------|------|------|
| 正确 | ★★★★★ | NDJSON 契约 CLI/Web 逐字节一致（replay 含 user_prompt 三处生效）；远程/本地分支错误映射同构（400/404/409/500 逐一测试）；610 测试 -race 全绿 |
| 轻量 | ★★★★★ | go.mod 零 require；前端无构建链；300 行上限 Go/JS 双约束（最大 293） |
| 健壮 | ★★★★☆ | LoadSession >1000 条翻页取全量（自检发现并修复）；读上限与本地 50MiB 对齐；扣分项为文档漂移（F1），非运行时 |
| 规范 | ★★★★☆ | verify-gate 七步机械编码 AGENTS.md 硬规则；CHANGELOG 逐条对账可溯源；扣分项：测试文件行数上限失守（F2）、记忆校验脚本死代码（F3） |
| 灵活 | ★★★★★ | 9 个 nil-able LoopHooks 全开放不变；远程存储经 `session.url` 一键切换且 fail-fast 不静默分叉 |

## 1. 前轮整改与待决策复核

- **O3–O6（第三轮 4 项低）**：确认已修复且未回退（replay 累计措辞、confirmInline 监听、grep glob 探针、deploy fail-fast）。
- **O7（动态 Host，原 P3 待决策）→ ✅ 已落地**：`config.web.allowed_hosts`（config.go:72-74）+ `hostVariants` 自动推导（web.go:127），CHANGELOG 已记。
- **O8（turn 并发无上限，原 P3 待决策）→ ✅ 已落地**：`web.max_concurrent_turns` 信号量门（web.go:40,50；默认 0=不限，溢出 429），CHANGELOG 已记。
- **O1/O2/O9/O10（观察维持项）**：语义未变，维持观察，无需改动。

## 2. 新发现（第四轮）

### 高（0 项）／中（0 项）

无。

### 低（5 项，均为文档滞后或门禁盲区）

**F1：ARCHITECTURE.md §8 未覆盖远程会话模式。**
`ARCHITECTURE.md:224-231`（§8 会话持久化）仍只描述本地 jsonl 路径（Rewrite/Append/Load 原子性与信号保护），`session.url` 非空时的 minisession Client 路径（`miniagent/session/client.go`、`cmd/miniagent/session_remote.go`）完全未提。ARCHITECTURE.md 是 AGENTS.md 路由指定的架构权威文档；远程模式改变了「持久化在哪、失败语义是什么」（fail-fast 不回退本地）这一架构级事实。README 与 MINISESSION_INTEGRATION.md 已覆盖，唯 ARCHITECTURE 缺席。**建议**：§8 补一段远程模式（触发条件/写语义 Rewrite+Create 补建/失败 fail-fast/读侧翻页），5 分钟。

**F2：测试文件 300 行上限失守，且门禁不查。**
`cmd/miniagent/web_config_test.go` 316 行。v6.6.0（CHANGELOG）明确「测试文件执行 300 行上限」并拆分过超限文件，但 `Makefile:25` 的行数检查只扫非 `_test.go`——约定靠自觉，无机械防线，已实际漂移。**建议**择一：① verify 增加测试文件行数检查（一行 awk，对齐 7ec5def 把 static/*.js 纳入的同款做法）；② 承认测试豁免（CONTRIBUTING.md 现文即豁免口径），删掉 CHANGELOG 里的上限声明。**倾向 ①**，与项目「约定必须门禁化」的一贯风格一致。

**F3：`scripts/verify-memory.sh` §2 是死代码，宣称的检查未发生。**
脚本头注释与步骤标题称检查「index 可达性」，但 `verify-memory.sh` 第 2 节的 while 循环体只做变量赋值与空 case，无任何校验动作（真正生效的是 2b「index 列出所有 L2 条目」）。门禁步骤名实不符——未来 index 引用断链不会被发现。**建议**：补全（对 index 中每条路径 `-f` 判存在）或删掉该节与头注释中的宣称。

**F4：CONTRIBUTING.md 两处过时。**
① `CONTRIBUTING.md:35`「核心库化（移出 internal/）计划于 5.0.0；当前 internal/ 下代码禁止外部导入」——库化已于 v5.0.0 完成（README 明示「所有核心包已移出 internal/」），现 internal/ 仅剩测试辅助包 looptest，表述误导；② `CONTRIBUTING.md:33` 内置工具列 6 个（read/write/edit/grep/glob/shell），实际 8 个（漏 ast/web）。**建议**：两行修正。

**F5：`.golangci.yml` 头注释平台范围陈述与实现相反。**
`.golangci.yml:14-15`「Platform scope: Linux/macOS only (Unix) … no windows fallback」，但 `miniagent/tools/platform_windows.go`、`miniagent/platform_windows.go` 存在且 README 首段即宣传 Windows 支持（CREATE_NEW_PROCESS_GROUP/taskkill/字节区间锁）。注释会误导贡献者跳过 Windows 验证。**建议**：改注释为三平台，或在 CI/文档中明确 Windows 为 best-effort。

### 观察（非缺陷，已核实）

- **MINISESSION_INTEGRATION.md / README 引用 `../minisession`**（仓外未跟踪路径，含其 ARCHITECTURE §10/§12）。按 AGENTS.md「纳入跟踪的文件不可引用未跟踪文件内容」的严格口径属于边缘违规；按实践口径是刻意的跨仓集成指针且指向内容已摘要入文。维持现状可，但若严格执行自家规则需把关键事实内联。待用户决策，不单方面改。
- **`internal/miniagent/looptest` 无测试（覆盖率 0）**：fake LLM 测试辅助包，被上层测试间接覆盖，可接受。
- **`text` 包覆盖率 71.1%（全库最低）**：100 行 HTML 转义/截断工具，README 声明其为 XSS 防线一部分——建议后续补齐转义边界用例，非紧急。

## 3. 深度复核抽验记录（本轮新读区域：minisession 集成全链路）

| 区域 | 结论 |
|------|------|
| `miniagent/session/client.go` | 404 统一包 `os.ErrNotExist`（与本地哨兵对齐）；翻页取全量 + 读上限 `maxSessionBytes+1MiB` 双自检修复（:66-68/:160 注释记录为什么）；`do` 统一鉴权/状态码/解封；30s 超时 fail-fast 无隐式重试 |
| `cmd/miniagent/session_remote.go` | resolve/save 远端镜像本地哨兵（`errSessionNotFound` 同款 404 映射）；`warnSessionMismatch` 远端同样生效；Rewrite 为主 + 404→Create+Rewrite 幂等补建，注释如实 |
| `cmd/miniagent/run_turn.go:100-112,222` | 远程/本地分支对称装配；`session.dir` 忽略语义一致；信号保护仅本地路径（serve 形态交服务器，注释说明） |
| `cmd/miniagent/web_sessions_remote.go` | 列表 500 不掩盖配置错误（对齐本地 ReadDir 语义）；删除保留 beginDelete 在途守卫（防远端 Rewrite 复活）；tool-output 清理跳过有 retention 兜底的注释依据 |
| `cmd/miniagent/web_sessions.go:143-180` | replay 远端分支 id 白名单前置（400 而非 500）；`meta.Type==""`→404 提为两分支共享（迁入垃圾首行防御） |
| `remote_stub_test.go` + `web_session_remote_test.go` | stub 模拟服务端语义（Create 幂等/Rewrite 404/modified 定宽字符串保字典序=时间序）；测试断言契约形状含降级字段（workdir 空键必须在）、409/404/400 全错误路径 |
| `.agent` 记忆体系（L0/L1/L2 + verify-memory.sh） | 分层职责清晰、schema/索引齐备、检索反馈写入 session.md 有实据；唯脚本 §2 死代码（F3） |
| CHANGELOG Unreleased vs 实现 | 三条 Added/Fixed 与代码逐条对账一致（含「已知局限」两处 README 同步） |

## 4. 处置建议（按优先级）

| # | 项 | 建议 | 工作量 | 状态 |
|---|----|------|--------|------|
| 1 | F2 行数门禁盲区 | Makefile verify 增测试文件 300 行检查 + 拆 web_config_test.go | 10 分钟 | ✅ 已修复（本日，未提交） |
| 2 | F1 ARCHITECTURE §8 | 补远程会话小节 | 5 分钟 | ✅ 已修复（本日，未提交） |
| 3 | F4 CONTRIBUTING 过时 | 修 internal/ 表述与工具清单 | 2 分钟 | ✅ 已修复（本日，未提交） |
| 4 | F5 lint 配置注释 | 修平台范围陈述 | 1 分钟 | ✅ 已修复（本日，未提交） |
| 5 | F3 verify-memory.sh | 补全或删除 §2 死代码 | 5 分钟 | ✅ 已修复（本日，未提交；顺带修复实现中发现条目可带 .md 后缀的边界） |
| 6 | 观察：../minisession 引用 | 严格口径需内联关键事实；待决策 | — | ⏸ 待用户决策 |

## 5. 结论

第四轮评估**新发现 0 高 0 中 5 低**：五项全部是「文档/门禁滞后于代码」类——这本身是项目健康度的侧写（代码跑在文档前面，且无一处行为缺陷）。第三轮两项 P3（O7/O8）已实现并文档化；远程会话集成在 300 行约束下保持了本地分支的错误语义同构与测试密度，两处集成自检缺陷（翻页截尾、读上限）在合入前被发现修复。

**综合评分维持 9.0**：五目标中正确/轻量/灵活满星；健壮与规范各扣在文档漂移与门禁盲区（F1–F3），修复成本合计 <30 分钟，不构成架构或安全风险。项目处于「可交付且可长期维护」水位；下一阶段的实际考验是 minisession 双仓协同（Unreleased 待发版）与 .agent 记忆体系的持续运营密度。
