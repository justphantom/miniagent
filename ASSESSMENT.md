# miniagent 全面深度评估报告

- 日期：2026-08-22 · 基线：v6.2.0（db2891f，工作树干净）
- 方法：六维度并行评审（架构 / 健壮 / 规范 / 灵活 / 安全 / WebUI）+ 逐条对抗验证（怀疑者推翻 + 影响校准双视角）+ 跨维度综合 + 主评人独立复核。所有发现均有 `file:line` 证据；结论与 verify-gate 实测挂钩（本次全绿）。
- 目标基准：轻量、健壮、规范、灵活的 agent + PC/移动端自适应、美观、交互友好的 WebUI。

## 0. 评分总览

| 维度 | 评分 | 一句话判断 |
|------|------|-----------|
| 架构与轻量 | 8.5/10 | 核心零策略名实相符，钩子缝口设计是同类最佳实践；横向边与组合性有可改进处 |
| 健壮性 | 9/10 | 三层 panic 兜底、配对补全、取消语义、崩溃恢复、资源上限全覆盖，实测 -race 全绿 |
| 规范性 | 8/10 | 零依赖属实、551 测试、版本纪律严格；**SECURITY.md/ARCHITECTURE.md 存在成片漂移** |
| 灵活性 | 8/10 | 9 钩子 + 双层 provider 接口 + 三态仲裁表达力强；config 层换 provider 缝口是假的 |
| 安全边界 | 8/10 | 声明的边界自洽、WebUI 防护尽责（Host/CSRF/CSP/常数时间比较/SSRF）；个例声明超前于实现 |
| WebUI | 7/10 | 移动端适配与 XSS 防护扎实；**存在 2 个真实功能缺陷 + 1 个数据错误**，视觉细节有死规则 |
| **综合** | **8.2/10** | 后端四项约达成 90%；WebUI 正确性收尾欠一轮；文档漂移是最大规范债务 |

## 1. 确认的缺陷（按严重度）

### 1.1 高（用户可感知的功能错误）

**H1 · 并行多工具时 tool_result 挂载错乱**
`events.js:111` 按 `ev.call_id` 精确配对，但运行时 `tool_use` 事件**根本不携带 call_id**（`event/event.go:62` `EmitToolUse(w, name, input, ts)` 无该参数；`cmd/miniagent/emit.go:28` 经 OnToolUse 只传 name/input）。`toolNodes` map 恒空，全部结果走 `:118` 的 `last-of-type` fallback——同一步并行 2+ 工具时，所有输出堆进最后一个 `<details>`。v6.2.0 前端修的「精确挂载」在运行时链路上未生效（replay 路径 `replay.go:68` 同样缺）。
修法：`EmitToolUse` 增加 call_id 参数（`tool_handler.go:29` 已持有 `tc.ID`），事件结构体加字段；前端无需再改。

**H2 · 多步 ReAct 文本流并块 + DOM 顺序错位**
`events.js:48` `appendDelta` 复用存活的 `curText`；`appendToolUse` 不关闭当前文本块，只有 `finishText()`（result/error 时）清空。流式配置下：步骤 1 文本渲染后插入工具块，步骤 2 的 text_delta 命中同一 `curText`，被拼进首个文本块且位置在工具块之上——时间线错乱。
修法：收到任何非 delta 事件（tool_use 等）即 `finishText()` 关闭当前块；delta 事件已有 `step` 字段可用于兜底检测。

**H3 · 历史会话 token 计数双计**
`app.js:329-330` 解析 replay 流时先 `state.sessionInTokens += ev.input_tokens`，随后 `renderEvent(ev)` 的 result 分支（`events.js:162-163`）对**同一事件**再累加一次。打开任一历史会话后 header 显示的 in/out 即为真实值 2 倍，且后续 turn 在膨胀基数上继续累计。纯前端一行级修复（二选一去掉）。

**H4 · SECURITY.md 安全模型整体失实（跨两个大版本未更新）**
`SECURITY.md:7-9` 仍描述 v5.0.0 已删除的 default/auto 双模式（「write/edit 限 workdir 子树、shell 拒 sudo/su」）。实际：`cmd/miniagent/tools.go:10-13` 「Single permission mode…isolation is delegated entirely to the OS layer」；`config/config.go:109` 注释「miniagent has no mode since 5.0.0」；README/ARCHITECTURE 均已是零安全口径，唯 SECURITY.md 相反。后果：按此文档会把「write 越出 workdir」误报为漏洞，或误信「shell 拒 sudo」防护存在。自 v4.4.0（d998460）后未更新。
修法：重写为单模式零保障口径，与 ARCHITECTURE §10/README「运行隔离」对齐。

### 1.2 中（文档失实 / 声明与实现矛盾）

**M1 · ARCHITECTURE.md 结构性漂移**（多处，均经代码核实）
- `:40`/`:253` 声称核心有 `token_estimate.go`——文件不存在（c764f19 已外迁至 `policy/estimate.go:37,101,125`）。
- `:116` 伪代码与 `:147` 表把 OnStep 画在「每步结束」——实际 `loop.go:128-137` 在迭代**顶部**（BeforeLLM 前）。
- `:252` 不变量 4 称「无任何跨子包横向引用」——`compaction/budget.go:11` 等三文件 import policy（运行时默认回退，非仅测试）。
- `:203` 「并行度默认 5」——`loop_tools.go:11` `const maxParallelTools = 8`。
- `:227` config 查找链声称含 `$MINIAGENT_CONFIG`——全库 .go 零命中，实际仅 `-config`/`~/.miniagent/miniagent.json` 两路（`setup.go:26-37`）。
- §2 目录树缺 web*.go/emit.go/run_turn.go（-serve 路径在架构文档中缺席）。

**M2 · README 库用法示例不可编译**
`README.md:43` `MaxTokens: maxTokens`——`CompactionOptions` 无此字段（摘要上限是 `SummaryMaxTokens`/`SummaryMaxChars`，`compaction_split.go:41-44`）。其余部分可编译。README 钩子速查表（:18-29）还漏了第 9 个钩子 `OnStep`（`grep -c OnStep README.md` = 0）。

**M3 · SSRF 防护声明超前于实现**
`tool_web.go:169-173` `isPublicIP` 未含 `IsGlobalUnicast` 检查：255.255.255.255（受限广播）、240.0.0.0/4（保留段）、198.18.0.0/15（基准段）实测均放行；README:308 宣称「拒…受限广播」。可利用性低（Linux 默认不路由、GET 只读），属文档失实而非漏洞。一行修复：补 `!ip.IsGlobalUnicast()` 或删 README 措辞。

**M4 · 审批门禁不可扩展**
`confirm_on_tool_use.go:37` `destructiveTools` 为包级私有 map 硬编码 `{write,edit}`，`ConfirmCfg` 无字段可声明自定义破坏性工具；`OnToolUse` 签名只能拒绝不能改写参数。库形态挂自定义工具时审批流无法复用。

**M5 · Web 形态下 confirm_destructive 塌缩为恒拒绝**（跨维度）
serve 模式 stdin 非 TTY → `confirm_on_tool_use.go:79-80` deny-by-default，write/edit 及危险 shell 恒被拒，无浏览器确认通道补位。config 开启该选项的 serve 用户会遇到 Web 会话工具全部静默失败（除非 `MINIAGENT_AUTO_APPROVE=1`）。

**M6 · thinking 选择在 Web 链路断裂**（跨维度）
后端就绪（`web_turn.go:28,123-125` 注入 CLIOverrides），但 `/api/models` 的 providerModel 无 thinking 字段（`models.go:28-32`），前端硬编码 `o.dataset.thinking = ""`（`app.js:230`）——README:146 宣称的「thinking 下拉」不可用，四态仲裁在 Web 形态不可达。

**M7 · 删除会话泄漏工具输出目录**（跨维度）
`handleSessionDelete` 仅 `os.Remove(path)`（`web_sessions.go:193`），而每会话派生兄弟目录 `<id>.tool-output`（`run_turn.go:130`）；retention cleanup 只在同名会话后续 turn 构造 store 时触发（`tool_output_store.go:115-125`）。会话删除后该目录成永久孤儿——长驻 serve 进程的磁盘慢性泄漏。

**M8 · abort-then-clear 竞态**
`new-chat`/`openSession` 同步 `abort()` 后立即 `innerHTML=""`（`app.js:78-82,302-309`），而 fetch 的 AbortError 在微任务里才到达 catch（`app.js:166`）渲染「已停止」错误卡——落入**新视图**，含回填旧 prompt 的重试按钮。

**M9 · 移动端 placeholder 与行为矛盾**
`index.html:47` 「Enter 发送，Shift+Enter 换行」——但 `app.js:89-91` 触屏设备 Enter 实为换行（isTouch 分支），指引与行为相反。

### 1.3 低（打磨项，择要）

| # | 位置 | 问题 |
|---|------|------|
| L1 | `app.css:47` vs `events.js:187` | `.ev .tag .retry` 选择器与实际类名 `ghost retry-btn` 不匹配，重试按钮无样式 |
| L2 | `app.css:87-89` vs `events.js:77` | `.code-wrap .copy-btn` 浮动定位永不相配（DOM 无 .code-wrap 祖先），复制按钮以块级内联出现在代码尾部，视觉破相 |
| L3 | `app.css:100-103` | `@media (hover:none)` 的 `visibility:visible` 是死规则（无基础 hidden），删除入口本就全设备常显 |
| L4 | `app.css:55` | `#to-bottom` 36px 触摸目标（建议 ≥44px）+ `bottom:150px` 硬编码，composer 增高后可能压住输入区 |
| L5 | `index.html:28` | `#model-badge` 无 min-width/ellipsis，长模型名窄屏撑破 header |
| L6 | `app.js:340,85` | 移动端打开/新建会话无条件 focus，强制弹键盘遮挡半屏 |
| L7 | `md.js:6` | markdown 子集不支持链接，LLM 高频输出显示为字面 `[x](y)` |
| L8 | `app.js:283,295` | 删除会话用原生 confirm()/alert()，与自家 inlineHint 行内规范冲突 |
| L9 | `index.html:39` | 无障碍：无 aria-live 流式区、Escape 不关抽屉、删除控件是 span 不可键盘操作 |
| L10 | `app.js:310` | 切换/新建会话后 `#to-bottom` 残留可见、stickBottom 不同步 |
| L11 | `deploy` 三件套 | `MINIAGENT_SESSION_DIR` 无人消费（Go 零读取、unit 无 Environment=、播种 config 用相对 `.sessions`），授权目录空转 |
| L12 | `deploy/deploy.sh:51` | envsubst 变量列表与模板占位靠人工对齐，渲染后无 `${` 残留自检 |
| L13 | tracked 文件 | `confirm_on_tool_use.go:16,41`、CHANGELOG:302 引用被 .gitignore 忽略的 `docs/*.md`（悬空引用，违反自家最高约束）；`package-lock.json` 空壳入库（无 package.json，与「无构建链」叙事相悖） |
| L14 | `.agent/L1/assessment.md` | 违反「L1 唯一文件」自建约定（历史遗留）；L0 constraints.md:25 不变量 14 仍写 internal/* 布局 |
| L15 | `web_turn.go:71-92` | turn 锁内全程执行 runTurn，run.max_duration 默认 nil——若工具不尊重 ctx 挂起，该 session 永久 409，无超时兜底 |
| L16 | `loop_extra.go:40` | length-空文本救回只看 `resp.Reasoning`，ReasoningState-only 端点漏救 |
| L17 | `README.md:366-384` | 常量表缺近两版新增 8 个可覆盖键（config.example.json 反而更全）；README:146「>20 行折叠」实际是 24（events.js:8）；README 内嵌配置示例含已删键 `mode` |
| L18 | `config/resolve.go:15` | `CLIOverrides.ResultOnly` 被采集但 Resolve 从不消费（公共 API 死字段） |
| L19 | `tool_web.go:140-165` | SSRF 校验先解析后连接的 TOCTOU（README 已披露 DNS rebind 非边界，纵深记录） |
| L20 | `web_sessions.go:185` | `s.locks` 条目永不回收（每 session 16 字节级，可接受；量级大时需惰性清理） |

## 2. 分维度详评

### 2.1 架构与轻量（8.5/10）

**亮点**（均经代码验证）：
- **核心零策略属实**：`loop.go:16-20` 注释与实现一致；LoopConfig 四个策略载体字段（MaxTotalTokens 等）在核心 loop 文件零读取，由 `policy/loop_hooks_default.go` 工厂消费、cmd 装配。
- **钩子契约精确**：9 字段皆可 nil、nil 行为逐字段注释；`StepOutput` 的 View/Commit/Persist/ExtraUsage/Compacted 五字段语义在 `applyBeforeLLM`（loop_extra.go:106-137）逐一落实，含空 View 防误清守卫（:115-119）。
- **持久化由结构保证**：defer 统一写命名返回（loop.go:39-59）杜绝 12 个 return 漏字段；msgs/newMsgs 双轨经 `appendMsg` 单点同步。
- **300 行纪律全绿**：非测试文件最大 285 行。
- **跨包耦合有防护**：compaction 镜像 policy 常量 + `policy_const_consistency_test.go` 测试钉死契约，漂移即测试失败。

**不足**：唯一横向边 compaction→policy 与文档不变量冲突（M1）；thinking 降级重试与上限总结注入是核心内嵌的两条例外策略，未在「零策略」声明中限定（`loop_extra.go:22`、`loop.go:14`）；BeforeLLM 单槽位无组合辅助，叠加注入与压缩需手工包装且 ViewEstimate 会静默失真（HOOKS.md:185 自己承认）。

### 2.2 健壮性（9/10）——最强项

- **panic 三层兜底**：safeCall 兜工具、callLLMOnce 兜 LLM 解析、Run 顶层 defer recover 兜全部 9 钩子且 panic 后仍写出 NewMessages 供持久化；各有测试。
- **配对不变量收口**：handleToolCalls 全部错误出口走 `fillPlaceholderTail` 补占位；LoadSession 末行容忍 + ValidateToolPairing 严格校验。
- **取消语义逐路径验证**：迭代顶部检查、末步防吞（loop.go:214-216 注释即契约）、信号量双 select、stdin goroutine+select。
- **会话崩溃安全**：flock 5s 超时、临时文件+Rename 原子交换+父目录 fsync、写前截崩溃半行、写盘期忽略信号、超 50MB 拒写。
- **SSE 分层**：delta>0 不可重试防重复输出、errStreamUnterminated 检测、4MB 累积上限、错误体不回显防 key 泄漏。
- **压缩防死循环**：三段降级后仍超窗即报错终止；摘要垃圾检测重试一次后降级。
- **资源上限全覆盖**：prompt 1MB/chat body 4MB/stream 4MB/session 50MB/工具输出 1MB×500 文件/shell 120s/HTTP 超时。
- 实测 `go test -race -count=1 ./...` 14 包全绿（本次评估实际运行）。

剩余为可接受取舍（stdin goroutine 泄漏于退出路径、ENOSPC 丢一轮增量已告警、流式瞬态重试无跨 step 熔断）与 L15/L16 两个低危。

### 2.3 规范性（8/10）

**亮点**：零外部依赖属实（go.mod 无 require、无 go.sum）；`.golangci.yml` 注释逐条给理由、实测 0 issues；93 个 _test.go / 551 个测试函数，抽查均为真实行为断言；CHANGELOG Keep a Changelog + SemVer 与 33 个 git tag 一一对应；release.sh 强制 annotated tag + 干净树 + verify；README 工具语义写得比代码还细且抽验为真；config.example.json 与结构体逐键对应。

**最大债务是文档漂移**：H4（SECURITY.md）+ M1（ARCHITECTURE.md 六处）+ M2（README 示例不可编译）构成一片——项目自建「文档结论溯源到 file:line」标准（HOOKS.md:1），这批漂移形成自指矛盾。次要：L13 悬空引用与空壳 lockfile、L14 记忆体系自建约定违规、L17 常量表滞后。

### 2.4 灵活性（8/10）

**亮点**：View/Commit/Persist 三态精确区分压缩与注入；LLM/Doer 双层接口让换 provider/摘要模型成本接近零；压缩策略可整体替换（不挂即极简）；加工具是函数字段级成本；run.* 约 30 项可配 + 模型参数三层 + thinking 四态仲裁；CLI/Web/库三形态共享同一 turn 引擎、NDJSON 契约逐字节一致。

**不足**：config 层 `provider.kind` 仅一个合法值（`validate.go:62`）——换 provider 的缝口只在库形态真实存在，文档应明示；`CLIOverrides.ResultOnly` 死字段（L18）；LoopConfig 混入 4 个 Run 不读的策略载体字段，API 表面与语义错位；Web 覆写面窄于 CLI（不能按请求切 stream/max_iterations）；M4 审批门禁不可扩展。

### 2.5 安全边界（8/10）

该项目明示「agent 层零安全、隔离靠 OS」——评估重点是**声明的边界是否自洽、WebUI 面是否尽责**，结论：基本自洽且防护密度高。

- WebUI：无 key 非 loopback 启动即拒（fail-closed at startup）；x-api-key 常数时间比较；Host 白名单防 DNS rebinding + Sec-Fetch-Site/Origin 双查 + CSP/XFO/nosniff + Content-Type 强制（CORS-safelisted 不可伪造）；session id 白名单字符级校验防穿越；MaxBytesReader 限 body。
- SSRF：IP 直查 + 域名逐 IP 校验 + 重定向每跳重查 + 覆盖云 metadata；唯 M3 声明超前于实现。
- 凭证剥离如实披露非边界（procfs 不可挡）；symlink/FIFO/device 拒绝统一 O_NOFOLLOW + IsRegular。
- systemd 硬化到位且注释如实（「仅防服务自身越界，不约束 agent 行为」）。
- 前端 XSS：esc 先行 + textContent 流式 + CSP 二道防线，顺序正确。

### 2.6 WebUI（7/10）——用户重点关注项

**已到位**（自适应与交互基础扎实）：
- 移动端：viewport-fit=cover + interactive-widget=resizes-content + 100dvh + safe-area-inset-bottom + 输入 ≥16px 防 iOS 缩放（app.css:57-58 有 why 注释）；抽屉交互含 z-index 层叠注释；`@media(hover:none)` 已考虑触屏差异；≤640px 断点堆叠 composer 行。
- 交互：智能滚动（距底 ≤80px 跟随 + 浮动按钮）、发送/停止一体化、断流检测（sawTerminal）、等待指示、重试按钮回填 prompt、长文本折叠 + 渐隐遮罩、代码块复制、暗亮主题变量成对、IME isComposing 守卫、切会话先 abort 的状态保护。

**差距**（对照「自适应、美观、友好」目标）：
1. **正确性**：H1/H2/H3 三个真实缺陷——多工具输出挂载错乱、多步文本并块、token 双计。这是「可用」层面的欠账。
2. **美观**：L1/L2/L3 三条死 CSS 规则（重试按钮无样式、复制按钮浮动失效、触屏媒体查询空转）——规则写了但选择器与 DOM 不匹配，属「写了没验证」的破相。
3. **交互友好**：M8 abort 竞态、M9 文案与行为相反、L6 移动端强制弹键盘、L7 链接不可点、L8 原生弹窗、L4 触摸目标偏小、L10 状态残留。
4. **无障碍**：L9（aria-live/键盘操作/Escape）完全缺位——「友好」对读屏与键盘用户尚未成立。
5. 会话列表无空态/加载态/失败提示（新用户见空白栏）。

## 3. 与目标的差距全景

| 目标 | 达成度 | 关键差距 |
|------|--------|---------|
| 轻量 | ★★★★★ | 零依赖、单二进制、无构建链前端；无实质差距 |
| 健壮 | ★★★★☆ | 接近满分；剩 L15 turn 锁无超时兜底、L16 救回条件偏窄 |
| 规范 | ★★★★☆ | 工程纪律强；文档漂移成片（H4/M1/M2）是唯一大项 |
| 灵活 | ★★★★☆ | 库形态表达力强；config 换 provider 假缝口、审批不可扩展 |
| WebUI 自适应 | ★★★★☆ | 移动端基础到位；触摸目标/键盘弹出/窄屏溢出待打磨 |
| WebUI 美观 | ★★★☆☆ | 体系有了（变量/断点/折叠/复制）；死规则与视觉破相拉低 |
| WebUI 交互 | ★★★☆☆ | 骨架好；三个正确性缺陷 + 弹窗/竞态/无障碍缺口 |

**结构性观察**（综合评审结论）：CLI/Web 共享 turn 引擎是正确的架构决策，但把 confirm 交互、thinking 仲裁、工具输出生命周期一并带进 Web 形态后**没有人跨形态验收**——产生了 M5 恒拒绝、M6 断链、M7 孤儿目录三类问题。单维度评审（含 v6.2.0 的 WEBUI-REVIEW 修复轮）都在各自边界内正确，边界之间漏了。

## 4. 改进路线建议（按优先级）

**P0 正确性收尾（WebUI 一轮修完，均为小改）**
1. H1：EmitToolUse 加 call_id（后端事件层 + replay 两处调用点）
2. H2：非 delta 事件触发 finishText()
3. H3：token 双计去重（删 app.js:329-330 或 events.js:162-163 之一）
4. M8：abort 前置 finishText + 渲染前检查视图代（generation counter）
5. M7：handleSessionDelete 顺带 RemoveAll `<id>.tool-output`

**P1 文档对齐（半天工作量，消除自指矛盾）**
1. H4：重写 SECURITY.md 安全模型段
2. M1：ARCHITECTURE.md 六处修正（token_estimate/OnStep 时序/横向边/默认 8/查找链/目录树补 web 层）
3. M2：README 示例删 `MaxTokens` + 钩子表补 OnStep + L17 常量表增补
4. L13：删悬空 docs/ 引用与空壳 package-lock.json

**P2 WebUI 体验打磨**
死 CSS 修复（L1/L2/L3）、触摸目标与浮动按钮（L4）、model-badge 溢出（L5）、移动端不强制 focus（L6）、markdown 链接（L7）、行内删除确认（L8）、无障碍（L9）、placeholder 文案（M9）、会话列表空态

**P3 架构补强（按需）**
M5 浏览器端确认通道（或 serve 模式启动时警告 confirm_destructive 语义）；M6 thinking 下拉接通；M4 ConfirmCfg.DestructiveTools 导出；L15 web turn 默认超时；SSRF 补 IsGlobalUnicast（M3）；deploy 变量自检（L12）与 MINIAGENT_SESSION_DIR 消费或删除（L11）

## 5. 结论

miniagent 的**后端内核是同类项目中的高完成度实现**：零策略核心 + 9 钩子外挂的架构名实相符，健壮性经得起 -race 与对抗验证，工程纪律（测试/版本/lint）罕见地严格。距离「轻量、健壮、规范、灵活」四项目标，后端只剩文档对齐与少数扩展性打磨。

短板集中且明确：**WebUI 差一轮正确性收尾**（三个真实缺陷 + 一批死规则），以及**跨形态验收缺失**暴露的三个断点。全部列出的修复均为小改（最大单处约 20 行），无架构级返工。按 P0→P1 完成后，综合评价可上一个台阶。

---

*评估过程：6 维度评审 agent 并行深读 → 42 份对抗验证（怀疑者/影响双视角）→ 跨维度综合 → 主评人对全部高危/中危发现逐条独立复核（含 verify-gate 复跑与逐文件代码确认）。被对抗验证推翻的表述已排除在外。*
