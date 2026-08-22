# miniagent 评估报告（第三轮）· 全面复审

- 日期：2026-08-22 · 审查对象：HEAD 895b782（前两轮 45+15 项整改已全部提交，工作树干净）
- 方法：verify-gate 实测 + 全域深读（核心 loop/compaction/session/tools/provider/web 前后端/deploy/config）+ 边界推演
- 实测：`gofmt -s -l` 空 / `go build` / `go vet` / `go test -race -count=1 ./...` 14 包 558 测试全绿 / `golangci-lint` 0 issues / 非测试文件 ≤300 行 / 4 个 JS `node --check` 通过

## 0. 总体结论

**代码状态为三轮评估以来最佳。** 前两轮 60 项整改（H1-H4/M1-M9/L1-L20 + N1-N15）全部落地并提交，无一回退。核心五目标复核：

| 目标 | 评级 | 依据 |
|------|------|------|
| 正确 | ★★★★★ | tool_call↔tool_result 配对不变量全路径守住（含 panic 兜底、占位尾填）；NDJSON 契约 CLI/Web 逐字节一致；session jsonl 崩溃半行容错+原子重写+flock；558 测试含真实并发（N7 TryLock 409） |
| 轻量 | ★★★★★ | go.mod 零 require；前端 vanilla ES modules 无构建链；核心 Run 只做五件事 |
| 健壮 | ★★★★☆ | panic 三级兜底（safeCall/callLLMOnce/Run defer）；TOCTOU 多处明示防护；扣分项见 §2 O2（FIFO/中断挂起属已声明 trade-off） |
| 规范 | ★★★★★ | verify-gate 全绿；溯源注释可追回（N 编号→报告）；CHANGELOG 逐条挂编号 |
| 灵活 | ★★★★★ | 9 个 nil-able LoopHooks 全开放；compaction 为纯插件；策略-free 核心 |

WebUI 四目标复核：

| 目标 | 评级 | 依据 |
|------|------|------|
| PC+移动自适应 | ★★★★★ | 800px/640px 断点实测规则正确；抽屉 z-index 修复后 overlay 不吞点击；iOS ≥16px 防缩放 + safe-area + 100dvh + interactive-widget 齐备 |
| 美观 | ★★★★☆ | 双主题一致、折叠渐变、等宽工具块、tabular-nums 时间；无设计系统级打磨但远超功能基线 |
| 交互友好 | ★★★★★ | 三态会话列表（N5 补齐失败态+点击重试）、confirmInline 焦点圈闭、409/中断/重试全有反馈、Enter/Shift+Enter 按设备分流、aria 属性覆盖 |
| 逻辑呈现正确 | ★★★★☆ | call_id 精确配对、M8 generation 防串流、N13 计时重置、N14 回填守卫；扣分项见 §2 O3（replay 多轮 token 计数） |

## 1. 前轮整改复核（60 项抽验）

全部抽验通过，重点复核高风险项：

- **N1（CSP FOUC）**：app.css:72 `#login,#app{display:none}` 居末位压过 :70 的 `#login` flex 规则，CSSOM `style.display` inline 优先揭示——规则序正确。
- **N7（TryLock 409）**：锁点在 JSON 校验后、NDJSON header 前，409 走纯 JSON 不污染流契约；测试 hold 锁断言 409 语义匹配。
- **N5（三态）**：catch 分支「加载失败：<原因>（点击重试）」+ hint click 重试，代码与声明一致。
- **N14（回填守卫）**：`!$("workdir").value.trim()` 条件在，显式输入恒胜隐式回填。
- **会话删除竞态**：TryLock 持锁后删文件+删锁条目+清 tool-output 目录，注释如实声明「racing new turn 最坏短暂失去串行化」——推演确认该窗口内新 turn 走新锁条目，仅与"删除前已启动的 turn"失去互斥，而后者持旧锁且文件已删会自然失败，无可乘之机。

## 2. 新发现（第三轮）

### 高（0 项）

无。

### 中（2 项）

**O1：Web 会话列表 meta 缺失时 preview 兜底不一致。**
`web_sessions.go:75-78`：`LoadSessionMeta` 失败（meta.Type==""）时 summary 仍进入列表，但 Provider/Model/Workdir/Created 全空——前端 `s.model || s.id` 兜底了标题，但 `s.workdir` 空导致打开该会话时 workdir 输入框不回填（N14 的 replay 回填仅在 input 为空时生效，此处 meta 为空恰好走 replay 回填，可恢复；但 sessionMeta[id] 已缓存空 workdir，`workdirFilled` 初值 false，路径正确）。**实际影响低**：仅发生在 meta 行损坏但消息体完好的极端场景。建议：列表项 meta 缺失时以 mtime 兜底 Modified 排序（当前 Size/Modified 来自 e.Info() 已兜底），无需改动——**记录为观察，非缺陷**。

**O2：`web_turn.go` 会话锁 `__new__` 全局串行化的注释与实测有出入。**
`web_turn.go:68-72`：注释称空 session 键全局串行化「acceptable (interactive, not high-throughput)」。实测成立，但 `__new__` 锁条目在进程生命周期内常驻 `s.locks` sync.Map，与按 session id 的条目（删除时清）不同——无害但注释未提。**非缺陷**，注释扩展即可，不改。

### 低（4 项）

**O3：replay 多轮会话 token 累计显示为「打开后新增」，header 无历史用量。**
`events.js:162-163` + `store.js:48`：result 事件累加 sessionInTokens/sessionOutTokens，replay 时每条历史 result 也会触发累加——即打开旧会话后 header 显示的是**该次 replay 流中所有历史轮的累计**，而非"打开后新增"。CHANGELOG 自述「历史会话只累计打开后的轮次」与实现不符（实现是累计 replay 中的全部历史轮）。**影响**：数字含义偏宽，非错误（replay 确实重放了这些 result）；但语义与文档有出入。建议：replay 流的 result 事件不累加（events.js 无法区分 live/replay 流——需要在 openSession 中设标志位），或修正 CHANGELOG/README 措辞。**择一即可，倾向修文档**。

**O4：confirmInline 的 onKey 挂了三处（overlay + btnCancel + btnOk）。**
`app.js:259-261`：keydown 事件从按钮冒泡到 overlay，同一事件触发 onKey 两次。当前 close 幂等（resolve 已 settle + remove 幂等）故无害，但属于冗余监听——按钮自身 keydown 冒泡到 overlay 已足够。**清理项**：删 btnCancel/btnOk 两行监听，行为不变。

**O5：`tool_grep.go` globFn 的 filepath.Match 错误被吞。**
`tool_grep.go:82-84`：`ok, _ := filepath.Match(a.Glob, name)`——无效 glob（如 `[a-`）对所有文件返回 false+err，err 被丢弃，最终表现为"no matches"而非"invalid glob"。runGlob（glob 工具）有 globPatternMalformed 结构扫描前置校验，grep 的 glob 参数没有。**影响**：LLM 传错 glob 时得到误导性"no matches"。建议：runGrep 中对 a.Glob 做一次 `filepath.Match(a.Glob, "x")` 探针校验（对齐 runGlob 的两探针模式，成本一行）。

**O6：`deploy.sh` envsubst 渲染后仅 WARNING 不 fail。**
`deploy.sh:55-58`：检测到残留 `${VAR}` 占位符时 echo WARNING 继续——渲染残缺的 unit 会被 enable+restart，systemd 可能起不来或静默用错路径。**建议**：改 exit 1（渲染失败即部署失败，比带病启动更易排查）。属部署健壮性加固，非运行时缺陷。

### 观察（非缺陷，已核实）

- **O7（N11 遗留）**：动态 Host（DHCP 换 IP 后白名单不含新 IP）仍待产品决策，维持 P3。轻量路径：guard 对 loopback 恒放行 + 文档建议反代场景配 `web.allowed_hosts`。
- **O8（N12 遗留）**：turn 并发无上限（仅同 session 互斥），维持 P3。轻量路径：`handleTurn` 入口信号量（如 8 并发），溢出 429。
- **O9**：`tool_read.go:27-33` 明示的 FIFO/NFS 不可中断 read goroutine 泄漏——已声明 trade-off，隔离依赖调用侧，符合"有话直说"原则，不动。
- **O10**：`scrubEnv` 的已知缺口（DATABASE_URL/*_COOKIE/*_DSN）注释如实声明非隔离边界，fallback 靠 OS 隔离——与 README「Run Isolation」指引一致，不动。

## 3. 深度复核抽验记录（本轮新读区域）

| 区域 | 结论 |
|------|------|
| `miniagent/loop.go` Run 主循环 | defer 统一回写 12 返回路径零遗漏；panic recover 先于字段赋值（transcript 不丢）；summarizeAtLimit 闭包捕获正确；ctx 退出路径 130 语义守住 |
| `miniagent/loop_tools.go` 并行工具 | 信号量+ctx 优先 select 防取消后占槽；safeCall 单工具 panic 隔离；denied/unknown 短路与结果顺序保持 |
| `miniagent/session/session.go` | 崩溃半行三态（尾行容忍/中行严格/配对校验）逻辑自洽；ensureTrailingNewline 慢路径 mb+1 截断与超 mb 拒写互洽 |
| `miniagent/compaction/*` | estimateTokensFromUsage 锚点防陈旧（Ts>=latestSummaryTs）；usableTokens reserve 钳 CW/5 防小窗倒挂；compactHistory/trimRecentRounds 保首+保尾+保最新 summary；dedupShellCommands 参数与结果对称折叠 |
| `provider/openai/stream.go` | 重试分相（pre-delta 可重试/post-delta 不可撤销）正确；Transport 借用缓存对称；错误体不回显防 key 泄漏 |
| `tools/tool_shell.go` | 进程组 kill 防孤孙；后台作业持管道超时分支如实报告；scrubEnv 关键词表与 PAT/PATH 豁免正确 |
| `tools/tool_edit.go` | pathLocks 序列化同路径读改写；checkEditSize 防放大 OOM；多段事务全成功才落盘；TOCTOU LimitReader 防换大文件 |
| `tools/tool_grep.go` | 正则 AST 节点数限制防 ReDoS；二进制 NUL 嗅探；>50MB 跳过；FIFO/设备 IsRegular 双检（walk 级+open 级） |
| `tools/tool_write.go` | 空 content 显式拒绝（防误 truncate）；原子写 temp+sync+rename+父目录 sync；0600 新文件默认 |
| `tools/tool_web.go` | SSRF 每跳重检；GET-only；1MB body 上限；charset 检测 BOM>header>meta，不支持字符集明示降级 |
| `cmd/miniagent/web*.go` | guard 三防（Host/Sec-Fetch-Site/Origin）+ CSP 无 unsafe-inline；wildcard 绑定自动含主机名+网卡地址；409/404/500 语义分明 |
| `webstatic/*.js` | M8 generation 防串流全路径覆盖（send/openSession/new-chat）；appendDelta 流式纯文本零闪烁、finishText 一次性 mdRender；toolNodes Map 按 call_id 精确配对 + 旧会话回退 |
| `deploy/deploy.sh` | envsubst 渲染+占位符残留检测；系统用户/组创建；session 目录权限 0750；seed config 的 session.dir 指向部署目录 |
| `config/resolve.go` | 三层仲裁（model>provider>global）+ 对规则（provider/model 必须成对）+ thinking 四态；durations 统一解析错误定位 |

## 4. 处置建议（按优先级）

| # | 项 | 建议 | 工作量 |
|---|----|------|--------|
| 1 | O4 冗余监听 | 删 app.js:260-261 两行 | 1 分钟 |
| 2 | O5 grep glob 校验 | runGrep 加两探针（对齐 runGlob） | 5 分钟+测试 |
| 3 | O6 deploy fail-fast | WARNING→exit 1 | 1 分钟 |
| 4 | O3 文档对齐 | CHANGELOG/README 措辞改为「replay 流中累计」或代码加 replay 标志 | 5 分钟 |
| 5 | O7/O8 | 维持 P3 待决策，不建议本轮动 | — |

## 5. 结论

第三轮评估**新发现 0 高 2 中 4 低**，全部为中低 severity 的打磨项，无架构性缺陷、无安全性回退、无正确性破绽。60 项前轮整改零回退，verify-gate 全绿，558 测试覆盖真实并发与崩溃路径。

**综合评分 8.8 → 9.0**：两轮整改的复利效应显现（工具层 TOCTOU 防护、压缩锚点防陈旧、前端三态齐备均属"看不见但关键时刻救命"的健壮性），扣分项仅剩 4 个低 severity 打磨项与 P3 两项待决策。项目已达「正确、轻量、健壮、规范、灵活」五目标的可交付水位；WebUI 自适应/美观/交互/正确四目标中仅美观有主观提升空间（无设计系统），其余三项客观达标。

剩余动作：§4 四项打磨（可选，均 <15 分钟）；P3 N11/N12 维持待产品决策。
