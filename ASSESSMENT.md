# miniagent 全面深度评估报告（第二轮）

- 日期：2026-08-22 · 基线：v6.2.0+（0345138 及工作树未提交的 L14/L16/L17/L18/L19/L20 收尾，最高约束禁提交）
- 方法：全库深读（核心 loop/session/provider/config + WebUI 前后端）+ 上轮 45 项整改逐条核验 + verify-gate 实测 + 新缺陷挖掘（CSP/键盘语义/连接生命周期/呈现子集）
- 实测：`gofmt -s -l` 空 / `go build` / `go vet` / `go test -race ./...` 14 包全绿（557 测试函数）/ `golangci-lint run` 0 issues / 非测试文件全部 ≤300 行 / 前端 4 个 JS `node --check` 通过
- 目标基准：正确、轻量、健壮、规范、灵活的 agent 核心 + PC/移动端自适应、美观、交互友好的 WebUI

## 0. 评分总览（对比上轮 8.2）

| 维度 | 上轮 | 本轮 | 变化依据 |
|------|------|------|----------|
| 架构与轻量 | 8.5 | 8.5 | 不变；零策略核心、9 钩子、零外部依赖经复核属实 |
| 健壮性 | 9 | 9 | 不变；新增 N3（serve 连接生命周期）属性能非正确性 |
| 规范性 | 8 | 8.5 | H4/M1/M2 文档漂移成片已修；剩 README 两处微漂移（N6）、L1 违规（N8） |
| 灵活性 | 8 | 8.5 | M4（ConfirmCfg.DestructiveTools/DangerWords 导出）已落地；L18 死字段已删 |
| 安全边界 | 8 | 8 | 边界声明自洽如旧；新增 N12 资源耗尽面披露 |
| WebUI | 7 | 8.5 | H1/H2/H3 正确性缺陷全清、M8/M9 修复、死 CSS 清零；剩 N1 FOUC、N2 Enter 危险、N4 有序列表 |
| **综合** | **8.2** | **8.6** | 上轮 P0-P3 全部落地且实测有效；本轮新发现以中低危打磨为主 |

## 1. 上轮整改核验（45 项 → 清零确认）

逐条抽查证据（非全列）：

- **H1**：`event.go:24` toolUseEvent 带 `call_id`；`replay.go:68` 传 `c.ID`；前端 `events.js:118` 按 call_id 配对 + fallback。✅
- **H2**：`events.js:157` `case "tool_use": finishText()` 先关当前文本块。✅
- **H3**：`app.js` replay 循环无 token 累加（仅 `events.js:162` renderEvent 单点累加）。✅
- **H4**：SECURITY.md 已重写为 v5.0.0 单模式零保障口径，含「不属于漏洞」三条。✅
- **M1/M2**：ARCHITECTURE.md OnStep 时序改「迭代顶部」、目录树含 web 层、不变量 4 认可 compaction→policy 横向边；README 示例 `SummaryMaxChars: 5000`、钩子表 9 行含 OnStep。✅
- **M3**：`tool_web.go:176` isPublicIP 补 `IsGlobalUnicast()`（RFC1918/受限广播/保留段/基准段全拒）。✅
- **M4**：`confirm_on_tool_use.go:22-39` `DefaultDangerWords`/`DefaultDestructiveTools` 导出 + ConfirmCfg 两个 override 字段。✅
- **M5**：`web.go:86-89` serve 启动时 stderr 警告 confirm_destructive 恒拒语义。✅
- **M6**：`models.go:32-35` Thinking 字段 + `app.js:285` `o.dataset.thinking` + `app.js:116` 随 body 发送。✅
- **M7/L20**：`web_sessions.go:207-208` 删除时 `locks.Delete` + `RemoveAll <id>.tool-output`；测试断言齐。✅
- **M8**：`app.js:60` generation 计数器，send/openSession/new-chat 三处快照比对，abort 后旧事件全部丢弃（含 title/focus 守卫 `app.js:397,401`）。✅
- **M9/L1-L10/L15/L16/L18/L19**：placeholder 文案、44px 触摸目标（app.css:58）、`.ghost.retry-btn` 选择器、链接协议白名单（md.js:15）、aria-label/Escape、ReasoningState 救回（loop_extra.go:42）、web turn 10min 默认超时（web_turn.go:90）、死字段删除等逐项在码。✅
- **L11/L12**：deploy 消费 `MINIAGENT_SESSION_DIR` + envsubst 渲染自检（commit 9dce7c1）。✅
- **L14**：L0 约束 14 已改现布局 + 横向边表述。✅（但 `.agent/L1/assessment.md` 本体仍在，见 N8）
- **L17**：README 常量表补 12 行覆盖全部 run.* 键。✅

verify-gate 本轮实测全绿，整改不是纸面清零。

## 2. 新发现（按严重度）

### 2.1 中

**N1 · CSP 阻断 index.html 内联 style 属性 → 初始双面板闪现（FOUC）**
`web_guard.go:35` `style-src 'self'`（无 `unsafe-inline'`）会忽略 HTML 内联 style 属性（CSP 标准行为：元素 CSSOM 赋值不受限，但标记里的 `style="..."` 被丢弃）。`index.html:10,20` 恰用 `style="display:none"` 隐藏 #login/#app → 首次绘制两者同时渲染（登录表单居中 + 完整应用骨架堆叠），直到 `boot()` 的 fetch 返回后 JS 经 CSSOM 修正。静态资源 `Cache-Control: no-store` → **每次刷新都闪**，慢网络下可观测数秒。
修法（推荐第 1 种）：初始隐藏移入 CSS——#login/#app 默认 `display:none`，`showLogin/showApp` 经 CSSOM 显示（现有函数零改动语义）；不要加 `style-src 'unsafe-inline'`（弱化 XSS 面，本项目前端以「无内联」为荣）。

**N2 · confirmInline 的 Enter 语义危险：焦点在「取消」上按 Enter = 确认删除**
`app.js:235-237` 对 overlay/两按钮统一挂 `keydown`：`Enter → close(true)`。对话框打开时焦点初始在 btnCancel（`app.js:229`）——用户按 Enter 的直觉是「激活焦点按钮（取消）」，实际触发**确认**。这是删除会话等不可逆操作的确认框，方向反了。Escape=取消正确。
修法：Enter 仅在 `document.activeElement === btnOk` 时确认（或干脆删掉 Enter 处理，只留按钮点击与 Escape）；顺手补 `role="dialog"` 与焦点圈闭（Tab 不逃出框体）。

**N3 · serve 模式每 turn 新建 http.Transport：无连接复用 + 空闲连接不回收**
`web_turn.go:121` 每 turn 调 `buildRuntimeClients` → `setup_providers.go:29` `newHTTPTransport()`。后果二连：(a) 每 turn 重新 TCP+TLS 握手（同端点每轮多 ~1-2 RTT 延迟）；(b) `newHTTPTransport` 未设 `IdleConnTimeout`（零值=永不超时），旧 Transport 被弃后其 keep-alive 连接无主动 `CloseIdleConnections`，只能等对端关闭或进程退出——长驻 serve 进程 fd 缓慢累积。CLI 单 turn 形态无感，serve 形态是实打实的慢性损耗。
修法：turnEngine 持有 per-provider-name 的 `*http.Transport` 缓存（config 热加载不存在，启动后 provider 集合不变；`sync.Map` 或构造时一次构建）；顺带给 Transport 设 `IdleConnTimeout: 90s`。

### 2.2 低

| # | 位置 | 问题 |
|---|------|------|
| N4 | `md.js:79` | markdown 子集不支持有序列表（`1. `）与嵌套列表：LLM 高频输出有序列表，现渲染为 `<p>1. foo</p>`，呈现失真。补 `^\s*\d+\.\s+` → `<ol>` 即可 |
| N5 | `app.js` loadSessions | 会话列表无空态/加载态：新用户或全删后见空白侧栏，无「暂无会话」占位 |
| N6 | `README.md:147` | 两处微漂移：「>20 行折叠」实际 24（events.js:8）；「🌙/☀️」实际按钮字符 ◐ |
| N7 | `web_turn.go:73-75` | 同会话第二个 turn 请求阻塞在 mutex（≤10min）无任何反馈：客户端只见挂起。可 `TryLock` 失败即 409「turn in progress」，前端转友好提示 |
| N8 | `.agent/L1/assessment.md` | 仍存留且被 git 跟踪，违反「L1 唯一文件（active/session.md）」自建约定（历史遗留，上轮 L14 只修了 L0 表述） |
| N9 | `app.js:50-54` | 主题按钮 ◐ 无当前态指示（不区分 dark/light，title 恒「切换主题」）；切到 light 后按钮外观不变 |
| N10 | `web.go:118` vs `webstatic/assets.go:8` | 静态资源名单双维护（embed 指令 + mux 路由各写一份 6 文件），新增文件易漏一处。可遍历 `embed.FS` 自动注册 |
| N11 | `web_guard.go:92` | hostVariants 启动时快照网卡地址：DHCP 换 IP / 后插网线后新地址被 Host 门 403，需重启服务 |
| N12 | `web.go` | `/api/*` 无速率/并发上限：持有 key 的客户端可并发无限 turn（每 turn 一次 LLM 计费调用）；与已披露的「鉴权爆破限速未做」同族，属资源耗尽面。轻量修法：信号量限并发 turn 总数 |
| N13 | `events.js:148,174` | 回放时 elapsed 显示失真：turnStartTs 取会话首事件 ts，result 行的「· Ns」实为整会话时长跨度的显示口径，非该轮耗时 |
| N14 | `app.js:374` | openSession 填 workdir 依赖 sessionMeta（由 loadSessions 填充）：刷新后秒点会话时 meta 未就绪 → workdir 不回填，用户须手填 |
| N15 | `app.css:104` | `@media (min-width:800px)` 单行塞 4 条规则，可读性微瑕（无碍功能） |

### 2.3 特性边界（非缺陷，记录备查）

- 流式期间纯文本累积、finishText 一次性渲染 markdown——有意设计（零闪烁），代价是流中无格式。
- `appendDelta` 的 `textContent +=` 高频拼接理论 O(n²)，实际 delta 体量下不可测。
- replay 的 tool_result 无 exit_code、finish 恒 "stop"——session 格式不存这些，已在 replay.go 注释声明。

## 3. 分维度结论

**核心 agent（正确/轻量/健壮/规范/灵活）——目标基本达成。**
正确性：配对不变量三路径收口、命名返回 defer 防漏字段、取消语义逐路径（含末步防吞）、溢出识别 24 正则+4 排除、thinking 降级固化——本轮深读未发现新的正确性缺陷。轻量：go.mod 零 require、核心 9 钩子全可 nil、前端无构建链。健壮：三层 panic 兜底、session 崩溃安全（flock/原子 rename/半行截断/信号屏蔽）、SSE 分层重试（delta>0 不可重试）。规范：557 测试、lint 0 issues、≤300 行纪律全绿、CHANGELOG/SemVer 对齐。灵活：View/Commit/Persist 三态、LLM/Doer 双层接口、ConfirmCfg 可扩展（新落地）、CLI/Web/库三形态共享 turn 引擎。剩余提升点：BeforeLLM 单槽位组合需手工包装（HOOKS.md 已自知）、config 层换 provider 仅一个合法 kind（文档应明示缝口只在库形态）。

**WebUI（自适应/美观/交互）——上轮三大正确性缺陷清零后进入打磨期。**
自适应到位：断点（800px 侧栏/640px 堆叠）、100dvh、safe-area、输入 ≥16px 防 iOS 缩放、44px 触摸目标、Escape+overlay 关抽屉、触屏不强制弹键盘、isTouch Enter 语义分离、IME 守卫。交互到位：流式累积、停止/重试、智能滚动+回底按钮、断流检测（sawTerminal）、行内确认替代原生弹窗、代码复制、长文折叠渐隐。美观到位：暗亮主题变量成对、错误红边、tabular-nums 时间。剩余欠账即 N1（FOUC 最刺眼）、N2（危险 Enter）、N4/N5/N9 呈现细节。

## 4. 建议（按优先级）

**P0（半小时级，正确性/危险交互）**
1. N1：#login/#app 初始隐藏移入 CSS
2. N2：confirmInline Enter 改为仅 btnOk 聚焦时确认 + role="dialog" + 焦点圈闭

**P1（一小时级，性能与呈现）**
3. N3：serve 模式 Transport 缓存 + IdleConnTimeout
4. N4：md.js 补有序列表
5. N7：同会话并发 TryLock → 409

**P2（打磨，择机）**
N5 空态、N6 README 微漂移、N8 删 assessment.md、N9 主题态指示、N10 资源名单自动注册、N13 回放 elapsed 口径、N14 openSession workdir 兜底

**P3（需产品决策）**
N11 动态 Host（可文档披露替代）、N12 并发上限

## 5. 结论

上轮评估驱动的 45 项整改**全部真实落地且经实测验证**，项目综合从 8.2 升至 8.6。后端核心五个目标（正确、轻量、健壮、规范、灵活）已无已知正确性缺陷，剩的是表达力边界与文档微瑕。WebUI 三个功能性错误清零后，「正确呈现」成立；距「视觉无瑕、交互无忧」还差一轮小改：N1 的 FOUC 是当前最影响观感的单点，N2 是唯一带风险方向的交互，N3 是 serve 形态唯一的慢性损耗——三件合计约两小时工作量，完成后 WebUI 可上 9 分档。
