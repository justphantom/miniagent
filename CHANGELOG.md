# Changelog

所有显著变更进入此文件。格式参考 [Keep a Changelog](https://keepachangelog.com/)，
版本号遵循 [Semantic Versioning](https://semver.org/)。

## [Unreleased]

## [6.6.3] - 2026-08-24

### Changed
- **WebUI header 精简**：仅显示会话工作目录；会话 ID 移至浏览器标签页标题与会话面板，in/out token 移至状态栏与用量面板（信息无损重分布）。连带删除死代码 `#tab-meta`。
- **WebUI 工具卡片摘要**：折叠态显示 `工具:关键参数`（如 `edit:readme.md`、`shell:go test ./...`；path 取 basename，`command/url/pattern/query` 优先，未知工具回退首个字符串字段），参数超宽省略号截断自适应桌面/移动端。卡片仍默认折叠。

## [6.6.2] - 2026-08-24

### Added
- **WebUI 顶部显示工作目录**：header 在会话 ID 旁显示当前会话的 workdir（新会话首个 session 事件填充、切换到历史会话时由回放/列表元数据填充），空时不占位；移动端与长会话 ID 一同隐藏。

## [6.6.1] - 2026-08-24

### Fixed
- **WebUI 横向滚动条防御（桌面 + 移动端）**：三层防御——①局部滚动保留（代码块/表格/tab 条/status bar 各自滚动，tab 条隐藏滚动条）；②元素级钳制补齐：`.md img` 加 `max-width:100%`（贴宽图不再撑破卡片）、窄屏配置表单 label 列 180→110px、usage 步骤条窄屏换行、dirpicker 路径 chip 折行、trajectory 步元数据截断省略、配置错误串/路径 `overflow-wrap:anywhere` 折行；③`body { overflow-x: clip }` 全局兜底（`clip` 不创建滚动容器、不劫持 sticky），任何未预见溢出不可能升级为页面级横向滚动。窄屏全屏模态 `100vw`→`100%`——`100vw` 含经典滚动条宽度，Windows 桌面下自身就是溢出源。

## [6.6.0] - 2026-08-24

### Added
- **WebUI 配置漂移可见化**：`GET /api/config` 返回扩展结构——编辑基准改为**配置文件内容**（PUT 的写入目标，此前以运行值为基准导致保存后重开模态框改动"消失"的假象）、`diverged`/`diff[]`（文件与运行配置不一致的字段路径列表，仅路径不含值，secret 漂移不泄漏）、`file_error`（文件损坏时的降级标记）。设置模态框顶部显示"运行配置与配置文件有 N 处差异，重启后生效"横幅（悬停列路径）、差异字段 label 加 `●` 标记；文件被外部改坏时降级显示运行值并警告"保存将覆盖该文件"。
- **WebUI 服务重载**：设置模态框新增"重载服务"按钮。`runServe` 重构为重启循环（`serveOnce`），`POST /api/reload` 校验配置文件可加载（否则 400）、无进行中轮次（否则 409，提示在跑数量）后触发整代重建——auth key、`web.listen`、`allowed_hosts`、并发上限、provider 配置全部真正生效，等价于重启进程而不断开 baseCtx；世代交替间文件再损坏则保持旧配置继续服务（不崩溃）。前端点击后轮询 `/api/whoami` 穿过断连窗口，恢复后自动刷新编辑基准。重复触发返回 503。

### Fixed
- **WebUI 版本号双前缀**：`setVersion` 对已带 `v` 的版本字符串（git tag 注入，如 `v6.5.0`）再拼一次 `v`，登录页与状态栏显示 `vv6.5.0`。现按服务端原样显示。

### Changed
- 测试文件执行 300 行上限（原仅约束非测试文件）：`main_test.go`、`web_turn_lifecycle_test.go`、`loop_integration_test.go` 等超限文件按场景拆分；全库 import 分组经 goimports 归一（纯位置调整）。

## [6.5.0] - 2026-08-24

### Added
- **mid-data 截断透明化（R4）**：上游中途断流被降级为截断成功（v6.4.0 行为）时用户此前无任何感知——答案静默半截。现 `miniagent.Response.TruncatedStream` / `miniagent.Result.Truncated` 领域标记透传，result 事件新增 `"truncated":true`（omit-if-zero，仅实际发生时出现，旧消费方不受影响），WebUI result 卡片 usage 行显示「上游截断」。与 `finish:length`（模型 max_tokens 截断）区分：两者都非自然结束，但成因不同。
- **NDJSON `stop` 终态事件**：轮次被取消（显式停止 / 上下文取消）时输出 `{"type":"stop","reason":"canceled"}` 后再以退出码 130 结束。此前取消路径静默返回、无任何终态事件，web 前端以 result/error 判完成（sawTerminal）只见干净 EOF，误报「连接中断：流意外结束」——点停止按钮也落此文案。前端现渲染「已停止」卡片（保留重试续跑按钮）。消费方需将 `stop` 计入终态（与 result/error 并列）。
- **WebUI 配置管理页面**：新增 `GET/PUT /api/config` 端点（`web_config.go`），读取当前配置（secret 掩码回显）、`SaveConfig` 校验后原子写回（`config_save.go`，0600 + O_NOFOLLOW + 临时文件重命名）；前端 `config.js` 配置页——**表单模式**（6 个可折叠分组 + 60+ 字段，支持 text/number/bool/duration/array/secret 类型）、**providers 卡片编辑**（增删 provider，每 provider 的 URL/key/限额/headers 键值对/thinking 映射/增删 model 及模型级参数）、**JSON 高级编辑器模式**（textarea 直接编辑完整配置）、**客户端预校验**（提交前本地拦截空 providers/重复名/必填缺失等常见错误，减少无效往返）、**保存后回填表单**（后端返回权威 saved config 前端重渲染）；保存后提示「需重启服务生效」；配置侧栏按钮 `⚙ 配置`。`config.ValidateConfig` 导出供 web 层调用的校验入口。
- **WebUI 目录选择器 / Token 用量 / 工具轨迹 + UI 全面重设计**（`9096515` 起至 `1211853` 共 12 个提交的最终态）：
  - 后端：新端点 `GET /api/tree`（目录树浏览，`web_tree.go`）；NDJSON 新增 `step_usage` 事件类型（逐步 `input_tokens/output_tokens/tool_calls`，仅 web 路径启用，CLI 默认关）；`tool_use` / `tool_result` 事件新增 `step` 字段（增量，非破坏）。
  - 前端：目录选择器（`dirpicker.js`，含最近目录记忆）、Token 用量条与逐步明细（`usage.js`）、工具轨迹面板（`trajectory.js`，按步骤分组卡片）；设计令牌双主题（明/暗）、CSS Grid 响应式布局（800px/640px 两档断点、移动端抽屉侧栏）；后续迭代为 IDE 风格骨架（图标栏 / 标签页 / 滑入式会话面板）、消息卡片步骤图标与时间线、状态栏逐步指标、会话按项目树分组、滚动条样式统一、会话列表横向溢出修复。
- **部署配置目录迁移**：`deploy.sh` 安装配置从 `/etc/miniagent` 迁至 `/opt/miniagent/config`（目录归服务用户所有）——原路径在 systemd `ProtectSystem=full` 下 `/etc` 只读且无目录写权限，WebUI 配置页保存（temp+rename 需目录写权限）必失败。已部署环境需手动迁移配置文件。

### Breaking
- **`LoopHooks` 钩子签名变化（自定义钩子需改代码）**：`OnToolUse` / `OnToolResult` 签名首参新增 `step int`（当前步骤号，1 起）；`LoopHooks` 新增 `OnStepUsage func(step, inTokens, outTokens, toolCalls int)` 字段（每步 usage 记录后触发，nil 安全）。核心库 `miniagent` 的钩子实现方（含 `policy` 包的 confirm 钩子）需同步适配签名，否则编译失败。

### Fixed
- **WebUI 订阅者落后掐线伪装成正常结束（「连接中断」第三根因）**：turn 事件总线对落后超过缓冲的订阅者 `close(ch)`（D3 慢消费者策略），而发起端 handler 无法区分「轮次结束」与「lag 掐线」，一律按正常结束写干净 EOF——服务端 turn 完整跑完、journal 全绿，浏览器却报「连接中断：流意外结束」。现 handler 在通道关闭时检查 `entry.done`：lag 掐线写显式 `stream_cut` 非终态标记（/live 不再误写 live_end）；前端收到 stream_cut 或 EOF 无终态时自愈重建视图（runningKnown 判定走 /live 旁观重放或会话 jsonl 全量回放，失败才兜底「连接中断」卡）；缓冲 64→256 吸收终局 delta 突发；掐线记 `subscriber lag-closed` WARN（session/buf），此前该故障在日志中零痕迹。
- **WebUI 客户端断开误杀后台轮次（D1 违反）**：`handleTurn` 的 `defer cancel()` 绑定在 handler 作用域——订阅循环因 `r.Context().Done()`（刷新页面/断网/视图切换）返回时立即取消 turnCtx，正在运行的 agent 轮次被杀（journal 表现为 `read sse: context canceled`，llm-proxy 同毫秒 `aborting connection mid-stream`）。`cancel` 改由 turn goroutine `defer` 持有，turn 生命周期与请求连接彻底解耦；busy 路径的显式 cancel 与 stop API 不变。与上游掐流叠加时构成「半截答案→刷新→杀轮次→无终态→连接中断文案」的放大循环，此为「频繁连接中断」主根因之一。
- **deltaSent>0 的 SSE 截断不再丢弃已流出的 partial answer**：此前 `parseSSE` 在截断 chunk 上报 `parse sse chunk` 时返回空 Response，`DoStream` 在 `deltaSent>0`（流已不可逆，P2-4）时直接抛错导致整轮已流式输出丢失。现 `parseSSE` 返回已聚合的 partial Response，`DoStream` 在 `deltaSent>0` + transient 错误且 partial 含 text/reasoning 时接受为截断成功（`FinishReason=length` 走核心现有 truncation 警告路径）。这样上游 429 限流中途掐断 SSE 时，已推送给用户的内容仍作为本步结果落地，不再整步中止。工具调用-only partial（截断 JSON args 会污染下游 tool parse）仍走报错路径。

## [6.4.0] - 2026-08-23

### Added
- **WebUI 多会话并发 + 跨浏览器同步**：turn 事件总线（`web_bus.go` turnRegistry）解耦轮次与请求连接；新端点 `GET /api/events`（全局生命周期流：`turn_started`/`turn_finished`/`session_deleted`/15s `ping`）、`GET /api/sessions/{id}/live`（旁观进行中轮次：事件 0 起重放 + live 续流，结束以 `live_end` 收尾）、`POST /api/sessions/{id}/stop`（显式停止，取消服务端 turn 并保存已执行部分）；`GET /api/sessions` 行新增 `running` 布尔；前端多视图（`views.js`，#events 变视口、每会话独立 .view，切换不中断流）、旁观模式（发送 409 自动升级为 live 旁观）、会话列表 running 呼吸点；`web.max_concurrent_turns`（默认 0=不限，溢出 429）。
- **thinking 流式 markdown 渲染**：流式阶段（`text_delta`/`reasoning_delta`）从纯文本直出改为 300ms 节流 `mdRender` 增量重绘（首块立即绘制、>64KB 停止重绘等 finish、隐藏视图跳过重绘、复制按钮/折叠仍在 finish 一次性绑定）。此前推理模型数分钟的 thinking 流式期间原始 md 语法（`**bold**`、围栏）直出。

### Fixed
- **上游 SSE 截断 chunk 透明重试**：`isTransientStreamError` 覆盖 `unexpected end of JSON input`（mid-data 连接中断，例：上游 429 限流失效前掐断 SSE 数据行），预-delta 阶段（`deltaSent==0`）透明重试一次，不再因上游限流中途断开导致整轮中止抛「连接中断」。此前该错误被误判为持久 JSON 解析错误，不触发重试。

### Changed
- **新建会话 id 预生成**：`resolveSession` 新增 `presetID` 形参（CLI 恒传 ""），web 层在注册 turn 槽位前预生成 id——两个并发「新会话」轮次不再被全局 `__new__` 锁串行，真正并行。
- **turn 同会话互斥实现迁移**：per-session `sync.Map` 锁删除，互斥/删除互斥统一由 turnRegistry 承载（语义不变：同会话 409、删除中拒绝新 turn、成功删除释放槽位）。

### Breaking
- **WebUI 断连不再取消轮次**：`POST /api/turn` 的轮次上下文改为派生自服务器生命周期（`s.baseCtx`）而非 `r.Context()`——关闭标签页/断网/切换视图后轮次继续执行并落盘。此前依赖「关页面=停轮次」的用法改用界面「停止」按钮或 `POST /api/sessions/{id}/stop`。轮次仍受 web 默认 10min 超时与 `run.max_duration` 约束；服务器关闭（SIGTERM）时在途轮次取消并保存已执行部分。

## [6.3.0] - 2026-08-22

### Added
- **有序列表支持**（N4）：markdown 渲染器补 `<ol>`，LLM 高频输出的有序列表不再显示为 `<p>` 段落。
- **会话列表空态/加载态**（N5）：新用户/全删后左侧栏显示「暂无会话」占位，加载中显示「加载中…」。
- **主题按钮当前态指示**（N9）：按钮根据当前主题显示 ◐（暗色）/ ◑（亮色），title 提示切换方向。
- **静态资源自动注册**（N10）：`webstatic.Names()` 遍历 `embed.FS` 注册路由，新增文件只需改一处（go:embed 指令）；`TestNames` 钉死文件集合契约（审查补）。

### Fixed
- **CSP 内联 style 阻断致 FOUC**（N1）：`#login`/`#app` 初始隐藏移入 CSS，`style-src 'self'` 不会再丢弃 display 属性，首次绘制不再双面板闪现。
- **confirmInline Enter 语义危险**（N2）：Enter 仅在焦点在 btnOk 时确认（否则由按钮原生行为接管）；`role="dialog"`+焦点圈闭防 Tab 逃逸。
- **serve 模式 Transport 连接泄漏**（N3）：`turnEngine.transportCache` 按 provider 名缓存 `*http.Transport`，每 turn 不再新建 TCP+TLS 连接；`IdleConnTimeout=90s` 回收空闲连接。
- **同会话并发 turn 无反馈挂起**（N7）：`TryLock` 替代 `Lock`，失败即 409 `"turn in progress"`，不再阻塞 ≤10min 无反馈。
- **会话列表失败态缺失**（N5 审查补）：API 失败后侧栏不再停留「加载中…」，改为「加载失败：<原因>（点击重试）」占位，三态齐。
- **回放 elapsed 显示失真**（N13）：result 后重置 `turnStartTs`，多轮会话的每轮耗时分开计算。
- **openSession workdir 不回填竞态**（N14）：replay 流首条 session 事件携带 workdir，刷新后秒点会话时 workdir 自动填回；审查后补空值守卫——用户已显式输入的 workdir 不再被会话值覆盖。

### Changed
- **`buildClients` 签名**（N3 内改）：新增 `cache *transportCache` 形参，`buildLLM`/`buildOpenAILLM`/`buildDoer` 共享缓存的 Transport。
- **README 两处微漂移**（N6）：主题按钮字符从 🌙/☀️ 修正为 ◐/◑，长文本折叠阈值从 20→24 行（与代码 `LONG_TEXT_LINES=24` 一致）。
- **app.css `@media (min-width:800px)` 格式化**（N15）：单行 4 规则拆分为分块多行。

### Removed
- **`.agent/L1/assessment.md`**（N8）：遗留文件，违反「L1 唯一文件 active/session.md」自建约定。

## [6.2.0] - 2026-08-22

### Added
- **部署模板化**：`deploy/.env.example`（tracked 默认值）+ 可选 `deploy/.env`（git-ignored，局域覆盖）注入 `MINIAGENT_WORKDIR`/`MINIAGENT_CONFIG`/`MINIAGENT_USER`/`MINIAGENT_GROUP`/`MINIAGENT_SESSION_DIR`；`deploy.sh` 经 `envsubst` 渲染 `miniagent.service.tpl` 生成 unit。`make deploy` 自动创建系统用户/组、播种配置、安装并重启服务。
- **systemd 部署单元**：`deploy/miniagent.service`（后改为 `.tpl` 模板）含 `NoNewPrivileges`/`ProtectSystem`/`PrivateTmp` 硬化，`ReadWritePaths` 声明 workdir 与 session 目录；`deploy/deploy.sh` 安装脚本。
- **`config.web.allowed_hosts`**：Host 头白名单额外条目（反代域名/外部 IP 手动豁免，`hostVariants` 自动推导之外的场景）。
- **统一 User-Agent**：全部出站请求（chat/models/stream + web 工具）携带 `User-Agent: miniagent/{Version}`，`miniagent/version.go` 单源，`-X` ldflags 注入。

### Fixed
- **WebUI 安全加固**（WEBUI-REVIEW 高优先级项）：新增 `web_guard.go` guard 中间件——Host 白名单防 DNS rebinding + Sec-Fetch-Site/Origin 跨站拒绝 + CSP/XFO/nosniff 头；`POST /api/turn` 强制 `Content-Type: application/json`（CSRF 简单请求阻断，415）；resume 不存在 session 返回 404（`errSessionNotFound` sentinel）；sessions 列表目录不可读返回 500（替代静默 200+[]）；NDJSON 流式响应加 `X-Accel-Buffering: no`；`IdleTimeout=2m`。
- **通配监听 Host 白名单**：`0.0.0.0`/`[::]` 时自动添加 `os.Hostname()` + 全部网卡地址（跳过 link-local）到白名单，IPv6 去 `[]` 比较，解决 LAN IP 访问 403。
- **部署故障修复**：`ProtectHome=true` 导致 `/home` 下 workdir 不可写（`CHDIR Permission denied`）；`deploy.sh` 末尾追加 `systemctl restart` 确保新二进制生效。
- **前端修复**：折叠 CSS 类名匹配（`.ev.collapsed .md`）；工具错误输出红色边框（`.err`）；model-badge 从下拉/result 回填；overlay 点关；sending 时切会话/新建先 abort 再切换；tool_result 按 `call_id` 精确挂载；输入 >=16px（iOS 缩放）；safe-area-inset-bottom 内边距；宽表格 `overflow-x:auto`；等待指示（非流式配置）；模型选择持久化；alert->行内提示；登录页初始隐藏防闪现；aria-label；移动端抽屉 z-index 避免 overlay 吞点击。
- **summary 垃圾 fence 检查收紧**：估算常数锁定。

### Changed
- **部署单元模板化**：`deploy/miniagent.service` 重命名为 `miniagent.service.tpl`，硬编码值替换为 `${MINIAGENT_*}` 环境变量，`deploy.sh` 经 `envsubst` 渲染后写入 `/etc/systemd/system/`。

## [6.1.0] - 2026-08-21

### Fixed
- **CLI 补 `-serve` 互斥校验**：`-serve` 与 `-session`/`-save-session`/`-replay`/`-result-only` 组合此前静默忽略后者启动服务（CHANGELOG 已声明互斥但 main.go 无校验）；现在启动前报错退出 1。
- **shell 非零退出码镜像进 Output**：`[exit N]` 追加到输出文本末尾（`ExitCode` 结构化字段保留，仅 NDJSON 事件层可见）。此前 `IsError`/`ExitCode` 均不进 wire（L0 #8），静默失败（空输出 + exit 1）在 LLM 看来与成功无异；文本标记是模型侧唯一失败信号。
- **WebUI 会话列表不再整文件读盘**：`GET /api/sessions` 每条目改用 `session.LoadSessionMeta`（只读首行 meta，新增于 `miniagent/session`），替代 `LoadSession` 全量读+校验（O(全部会话字节)→O(元数据行)；每轮 turn 结束前端都会刷新列表）。首行非 meta/损坏/缺文件均零值 meta，校验仍归 `LoadSession`。
- **WebUI 模型下拉切分 bug**：model id 含 `/` 时前端 `value.split("/")` 取 `[0]/[1]` 错切片 → provider/model 传空静默回落默认模型。改为 provider/model 走 `option.dataset`，不依赖文本切分。

### Added
- **WebUI 会话删除**：`DELETE /api/sessions/{id}` + 会话列表项 ✕ 按钮（confirm 确认；删当前会话重置聊天视图）。进行中轮次的会话拒绝删除（409，经 turn 锁 `TryLock` 检测），防写方续写复活/损坏文件。移动端可见：`@media (hover: none)` 强制 `.sess-del { visib…[args omitted]流式 `text_delta`/`reasoning_delta` 累积到内存，`finishText()`（遇 `result`/`error`/工具事件打断）时一次性 `mdRender` 转 DOM；`result`/`tool_result` 的 `text`/`output` 也渲染。子集：# 标题、``` 代码块、-/* 列表、> 引用、---、**粗体**、*斜体*、`行内代码`、~~删除线~~、**表格**（`| col |` + `|---|---|` 分隔行 + `:-` `:-:` `-:` 对齐；单元格先 esc() 再 mdInline()）。**XSS 防护：先 HTML 实体转义（&<>"）再 markdown 替换**，`innerHTML` 仅注入转义后内容；用户 prompt 保持 `textContent` 不渲染。
- **WebUI header 会话 token 累计**：`result` 事件 `input_tokens`/`output_tokens` 累加显示于 header（`in=N out=N`）；新会话清零，打开历史会话时 replay 流中所有历史 result 的 token 均计入累计（无单独的历史用量数据源）。
- **WebUI 版本号展示**：登录页 h1 下 + header 右侧 `model-badge` 旁双点显示（`GET /api/whoami` 已返回 `version`，boot 时 `setVersion` 填充；移动端 <500px 隐藏 header 版，登录页常显便于排查服务版本）。
- **WebUI 体验全面优化**：
  - 中断：发送后按钮变「停止」，AbortController 断开流（后端 req ctx 取消 → 存已执行部分）。
  - 流中断检测：NDJSON 流结束但无 result/error 事件 → 插入「连接中断」error 提示；网络异常也显示为 error 事件（附「重试」按钮）。
  - 发送后清空 prompt + 保持焦点；桌面 Enter 发送 / Shift+Enter 换行（触屏设备反之）；发送中锁 composer 整行。
  - 智能滚动：距底部 ≤80px 才跟随流，上翻时不拽回；「↓ 新消息」浮动按钮。
  - 会话列表项加最后 assistant 消息预览（后端读 jsonl 末尾 ≤64KB 倒扫，不整文件读）。
  - 长 assistant 文本 >20 行折叠 + 渐变遮罩 + 「展开」。
  - 代码块一键复制按钮（mdRender 后处理 pre，clipboard API + execCommand 回退）。
  - 暗/亮主题切换按钮（CSS 变量双主题，localStorage 记忆）。
  - 移动端键盘适配：viewport-fit=cover + interactive-widget=resizes-content + 100dvh。
  - 前端拆分 ES modules：md.js（markdown 渲染）/ store.js（状态+持久化）/ events.js（事件渲染）/ app.js（引导+会话+发送）。
- **`-serve` WebUI（嵌入单页 + JSON/NDJSON API）**：`miniagent -serve` 启动 `net/http` 服务（纯 stdlib，go:embed 静态前端无构建链），提供登录页（`x-api-key` 鉴权，`subtle.ConstantTimeCompare` 常数时间比较）、`POST /api/turn`（NDJSON 流式，契约与 CLI stdout 逐字节一致，`session` 空则新建、非空则续跑）、`GET /api/sessions`（列表）、`GET /api/sessions/{id}`（重放最近 200 条消息为 NDJSON 事件流）、`GET /api/models`（provider/model 下拉数据源，复用 `-list-models` 逻辑）、`GET /api/whoami`（公开探测 `auth_required`）。配置新增 `web` 段：`web.listen`（默认 `127.0.0.1:8787`）、`web.key`（空回落 `$MINIAGENT_WEB_KEY`）；**远程 listen（非 loopback）必须配 key，否则启动拒绝**（key 是唯一安全边界，L0 #13 零安全 agent）。同 session 轮次经 `sync.Mutex` 串行化（防 `RewriteMessages` 首行竞态）；断连经 req ctx 取消 → `saveSession` 存已执行部分。

## [6.0.0] - 2026-08-21

> major：库化核心包（internal/ 外迁），破坏性移除 anthropic 与 responses provider；内部重构去 cliFlags 依赖；轮管道库化。迁移见下。

### Changed
- **轮管道库化：`turnEngine.runTurn`**（`run_turn.go`）把 CLI 一次轮的 resolve→session→clients→tools→hooks→Run→save 全序抽出为可复用函数（writer 注入、os.Exit-free），CLI 主路径与 `-serve` 共用；`buildLLM`/`buildDoer`/`buildRuntimeClients` 改返回 error（不再 `os.Exit`，服务进程可 500 应答而非崩溃）。`-serve` 与 `-list-models`/`-replay`/`-save-session`/`-session` 互斥。
- **内部重构：`loopCfg`/`warnNoBudgetFuse` 去 `cliFlags` 依赖**（改收 `maxIterDef`/`streamDef`/`sessionArg`），`buildHooks`/`emitRunError`/`emitRunResult`/`assembleHooks` 参数化 `io.Writer`（setup.go 拆出 `emit.go`）。CLI/NDJSON/config 既有契约零变化（全量测试回归通过）。
- **破坏性变更：移除 anthropic 与 responses provider**：删除 `internal/provider/anthropic/`（17 文件）与 `internal/provider/responses/`（14 文件）；`ProviderConfig.Kind` 仅接受 `""`/`"openai"`（其他值加载报错）；删除 `provider.cache`、`thinking.map` 的 anthropic JSON 对象语义与 responses 专属校验；`-list-models`/`FetchModelLimits` 统一走 openai client。迁移：`kind=anthropic` 配置改走 OpenAI Chat Completions 兼容网关；`kind=responses` 移除。
- **库化：核心包移出 `internal/`**：`miniagent/`、`config/`、`provider/`、`text/` 升至顶层，外部 Go 模块可直接 `import "github.com/justphantom/miniagent/miniagent"`。CLI 行为零变化。

## [5.1.0] - 2026-08-19

### Added
- **`result` NDJSON 事件新增 `compacted`/`thinking_downgraded` 布尔字段**：恒出键（false 默认），外部消费方可感知"本轮触发过摘要压缩"/"thinking 被降级丢弃"，无需解析 transcript。`EmitResult` 从 `Result.Compacted`/`ThinkingDowngraded` 回填。新增字段非破坏（既有消费方忽略未知键）；用严格 schema 校验的消费方需同步放宽。

### Changed
- **内部重构**：模型清单聚合逻辑从 `openai` 包上移至 `cmd` 层，消除 `openai→anthropic` 跨 provider 依赖；`execBackedTools` 清理 v5.0.0 已删工具残留。对外契约（CLI/NDJSON/config）零变化。


## [5.0.0] - 2026-08-18

> major：删 agent 层全部安全保障（`-mode`/confineWrap/白名单子命令工具/.git 封锁），工具精简至 8 个；新增 OpenAI Responses API provider + `web` 抓取工具。迁移见下。

### Added
- **OpenAI Responses API provider（`kind=responses`）**：新增 `internal/provider/responses`，支持文本/function tools/reasoning、非流式与语义事件流式、usage/finish 映射、重试与 thinking 降级。采用全量本地 `input` 投影，不用 `previous_response_id`；固定 `store:false` + `include:reasoning.encrypted_content`，新增 `Message/Response.ReasoningState` 持久化并回放 encrypted reasoning item（旧 session 缺该字段兼容）。`chat_url` 指 `/v1/responses`，`models_url` 复用 OpenAI models 端点；未配置 Responses 内置托管工具。
- **`web` 工具（网页抓取）**：GET URL → 文本入上下文（查文档/API/issue）。默认注册 + `run.web_timeout`（默认 30s）。SSRF 防护：拒私网/环回/链路本地（含云 metadata）/组播/受限广播及 v4-mapped v6 地址，DNS 全 IP 校验，重定向每跳重查；HTML 剥 `<script>/<style>`+标签+实体解码+空行折叠（极简正则级，非浏览器）；拒非 text/* 与 application/json；body 1MiB 封顶（对齐 read），输出 `max_shell_output_chars` 同源限幅+截断标记；仅 GET/HTTP(S)；User-Agent 标识。**非安全边界**：GET 查询参数可携带数据外传、响应内容直入上下文（prompt injection 面），guardrail 定位（攻击面记账已更新）。测试 httptest 全覆盖（12 用例）。

### Changed
- **v5.0.0 破坏性变更：删 agent 层安全保障，工具精简至 8 个**：移除 `-mode` 双模式（default/auto 合并为单模式，`shell` 恒注册）、`confineWrap`（workdir 子树路径限制）、`.git` 目录封锁、git/go/npm/golangci-lint/rename/delete 6 个白名单子命令工具及其 allow-list/deny-args/`.gitattributes` 防线/`resolveModuleRoot` 上溯约束、rtk proxy 集成（`rtkBin`/`rtkWrap`）、`confine_eval_symlinks`/`confine_auto` config 项、`ModeDefault`/`ModeAuto` 常量。保留工具：read/write/edit/grep/glob/ast/shell/web（8 个）。安全完全靠运行用户的 OS 权限（容器/低权 UID/文件权限），agent 层不再做任何保障。subagent guidance 模板去 `{mode}` 占位符（单模式无 mode 传递）。system prompt 「Verify」条目改引 shell（不再点名 go/golangci-lint/npm dev tools），「Stay inside」去 rename/delete 提及。迁移：外部命令改用 `shell` 工具（`git status`/`go test`/`npm install`/`golangci-lint run`/`mv`/`rm` 等），删除 config `defaults.mode`/`run.confine_eval_symlinks`/`run.confine_auto` 键（JSON 忽略未知键，旧 config 无需改即可加载）。
- **default system prompt 加 workdir 约束**：工作流清单新增 "Stay inside the working directory" 条目（点名 read/write/edit 工具 + 绝对路径/`..` 逃逸形态 + "state the need instead of accessing" 出口）；`appendWorkdirLine` 注入行从 "Relative paths resolve against it" 增强为 "all file tools operate only under it; relative paths resolve against it, absolute paths and .. must not escape it"。CLI `-workdir` 已注入 system prompt。软约束（v5.0.0 删 confineWrap 后为唯一引导），减少模型越界尝试的无效往返。

## [4.7.0] - 2026-08-17

> minor：工具面大版本——新增 `ast`/`golangci-lint` 内置工具（default 12 / auto 13）、`git tag` 放行；**breaking**：`shell` 工具收为 auto-only 注册（`GuardShell` 两 config 键随之删除）；default 模式封锁 `.git` 目录并参数级收紧（git/go/npm/lint deny 重构为 `optSpec` 归一化匹配器）；anthropic `models_url` 动态模型列表 + limits auto-fill；数十项工具行为修复（子命令重复剥离、壳元字符拒绝、UTF-8 流处理、glob `**` 等）。既有 openai 主链路行为不变。

### Added
- **`ast` 工具（Go 符号声明搜索）**：经 go/parser 递归搜索 Go 源文件的符号声明——函数/方法/类型/接口/结构体/常量/变量，输出 `path:lineno:kind Name`（方法作 `method (R) Name`），parser 级免疫注释/字符串误报；找定义不找引用（引用搜索归 grep）。参数 `pattern`（Go 正则，匹配符号名）+ `kind` 过滤 + `path` 搜索根 + `glob` 文件名过滤；命中 500 封顶，跳过 `.git`/vendor/testdata/非 `.go`；default 模式经 confineWrap 只读限 workdir 子树。README/ARCHITECTURE 工具清单同步（计数 default 12 / auto 13）。
- **golangci-lint 工具**（subcommand allowlist: run/version/linters; --fix/--write/--enable-all/--new* 被拒），default 模式禁 shell 下可闭环 verify-gate 末步。
- **git tag 子命令放行（创建/列举）**：`git tag` 入 allow-list，默认模式可打 annotated tag（发版流程 `git tag -a vX.Y.Z -m` 此前需用户在 default 外手动执行）。参数级拒绝：`tag -d/-f/--delete/--force`（删/移本地 tag ref）与 `tag -F/--file`（任意文件读入 message，经 show/cat-file 回显外泄）。exec 族（`-s/-u/-v/-e/--trailer`，gpg/编辑器调用）维持不拒——与 `commit -S/--gpg-sign` 同类先例，非新攻击面。远端 tag 面不变（`push --tags`/refspec push 此前已可用，强推/删除仍被 `+`/`:` refspec 规则拦）。已知保守过拒：`-m<msg>` 粘合形含 d/f/F 字母被簇分支拒（引号形 `-m "msg"` 不受影响）。
- **anthropic `models_url` 动态拉取模型列表 + limits 字段生效**：`kind=anthropic` 的 provider 现可配 `models_url`（指向 Anthropic `GET /v1/models`），`-list-models` 与 `FetchModelLimits` 动态拉取；不配置时回退静态 `models` 列表。认证沿用 `x-api-key` + `anthropic-version`；响应解析 `data[].id` 及非标准扩展字段 `context_window`/`max_output_tokens`（代理/网关上游可返回，与 openai 同名）；`display_name` 不透传（CLI 输出只用 id）。limits 参与主运行 auto-fill：填充 `MaxTokens`（传参）与 `ContextWindow`（压缩时机）；官方端点不报这两个字段时保持 nil。`anthropic.NewClient` 签名加 `modelsURL` 参数。
- **auto-fill model limits from models_url**：启动时若 config 三层（model>provider>global）均未设 `context_window`/`max_tokens`，GET provider 的 `models_url` 一次，解析非标准 `context_window`/`max_output_tokens` 扩展字段并填充。从不覆盖显式配置；fetch 失败 warn 后继续（不阻塞运行）。`ListModels`/`ListAllModels` 返回类型从 `[]string`/`[]ModelRef` 升级为 `[]ModelInfo`/`[]ProviderModel`（含 `Limits`）；`-list-models` 路径行为不变。

### Changed
- **provider 包与 CLI 配置层解耦（库化前置 P1/P2，详见 L2 library-defer-provider-config-decouple）**：`ValidateURL` 从 `config` 挪至 `internal/text`（纯 URL 校验，语义不属于 config）；`openai.ListAllModels` 签名改吃新中间结构 `ModelSource`（`config.ProviderConfig` 在 cmd 层映射），`openai`/`anthropic` 包现在零 `config` import。`FetchModelLimits`/`-list-models` 行为不变。
- `go` 工具白名单加入 `fmt`：default 模式禁 shell 下可执行 gofmt 等价格式化，补齐 verify-gate 首步。
- **default 模式封锁 `.git` 目录**：read/write/edit/grep/glob（`checkConfine`）与 rename/delete（`resolveConfinedPath`）拒绝指向 `.git` 及其子树的路径（含嵌套 submodule 布局；读也拒，防 config 凭证泄漏）。堵 git hooks 执行链（写 `.git/hooks/*` 后 `git commit`/`git pull` 触发）与 remote 改写外传（改 `.git/config` 后 `git push`）两条绕过 git 工具 allow-list 的通道；`.git` 内操作经 `git` 工具子命令白名单进行。auto 模式不套 confine，不受影响。
- **default 模式参数级收紧**（.git 封锁后残余通道）：
  - git deny 前缀加 `--no-index`/`-F`（仓库外任意文件读：diff 比较 / commit message 注入回读）；`git push/pull <url>` 位置参数拒绝（首个非选项位置参数含 `://` 或绝对路径即拒，堵不改 config 的外传路径）。
  - `.gitattributes` 外部驱动拒绝：每次 git 工具调用前扫 repo 根 `.gitattributes`，`filter=`/`diff=`/`textconv=` 属性即拒（workdir 可写文件声明 clean/smudge/diff 驱动 → `git add/diff` 执行外部命令，绕过 .git 封锁）。
  - go deny 前缀加 `-o`/`-toolexec`（编译产物写子树外 / 构建期执行外部程序）。
  - npm 拒 `--prefix`/`-C`/`--registry`（cwd 移出模块树 / 依赖流指向他方 registry）。残余：workdir 内 `.npmrc` 可覆写 registry，接受（guardrail 定位）。
  - `resolveModuleRoot` 上溯不得越出 workdir：父目录有 go.mod 时不再把 go/npm/lint 的 cwd 定到 workdir 外（模块级写越出子树）。已知保守代价：workdir 为模块子目录（repo/cmd/x）时 npm 报找不到 package.json。
- **git/go/npm/lint 工具签名加 `maxOutputChars int`**（`GitTool`/`GoTool`/`NpmTool`/`LintTool`）：输出上限从硬编码常量改为调用方注入（cmd 层接 `Limits.MaxShellOutputChars`）；库直接调用方传 `0` 走默认。
- **`decodeStrict` 拒未知字段 + `splitArgsStrict` 拒未闭合引号**：git/go/npm/lint 四工具的 args 解析收紧，字段 typo 与引号笔误从静默错行为改为带指引的显式报错。
- lint deny `-w` 前缀删除（无对应改写语义，纯误杀）；git 全局 `-O`/`-F` 收敛为 commit 专属 `-F`（diff 的 `-O<orderfile>` 只读）。
- **lint 不再经 rtk 代理**：rtk wrapper 吞退出码（见 Fixed），exit_code 契约优先于输出紧凑；git/go/npm 的 rtk 路由不变。
- **共享 exec 基建抽文件（300 行预算）**：`drainPipe`/`waitOutputTimeout` 抽入 `run_limited.go`（shell 与 git/go/npm/lint 共享同一跨块 UTF-8 缓冲与有界等待，`runLimitedOutput`/`utf8FullRune` 同文件）；`.gitattributes` 防线拆 `tool_gitattr.go`、`runWithTimeout` 拆 `tool_timeout.go`。

### Removed
- **breaking：shell 工具仅 `-mode auto` 注册**。default 模式不再注册 `shell`（12 工具，误调返回 `unknown tool`），auto 模式不变（13 工具）。注册门替代两级词法过滤，同批删除：
  - `ShellTool` 的 mode 形参与 sudo/su 等 11 提权器拒绝名单（`sudoSuRe`）——经 cmd 装配后不可达（auto 不触发该检查）。
  - `GuardShell` 及 opt-in config 键 `run.shell_allowlist` / `run.shell_confine_cd`（v4.5.0 引入）——仅 default shell 生效，shell 不注册后成死代码；已配该键的 config 加载不再生效（字段已删，JSON 中残留键被忽略）。
  - subagent fork 引导（经 shell fork）改 **auto-only 注入**；default 模式 system prompt 不再含 fork 指引。验证句措辞改工具中立（dev tools，auto 下另有 shell）。

### Fixed
- **git 工具进程内串行化**：核心默认并行执行同一步的多个 tool_calls（maxParallelTools=8），同轮 `add`+`commit` 撞 `.git/index.lock`（会话 20260817-082103 实测 `Unable to create .git/index.lock: File exists`，模型随后徒劳尝试经 git rm / delete 工具删锁）。包级 `gitMu` 覆盖 exec 全程，同轮 git 调用按到达顺序串行；拒绝路径不排队、跨进程竞争不涉。
- **args 未引号壳元字符前置拒绝**（git/go/npm/lint）：模型把 shell 管道/重定向写进 args（`test ./... 2>&1 | head -100` → go 收到 flag `-100`；`run ./... | grep -E …` → `-E` 报错），argv 直传 exec 无 shell，元字符成为字面参数报出无从诊断的 flag 错误。现检原始 args 引号外的 `| & ; > < \`` 并拒绝，文案指明「一次一条命令 / 引号包裹特殊字符 / auto 模式用 shell 工具」；引号内与转义形不受影响（`log --grep 'a|b'` 合法）。
- **go/npm/lint 子命令重复剥离**：同 git 修法铺到三工具——会话 20260817-082103 中 34 次 go 调用 29 次带 `{"subcommand":"test","args":"test ./..."}` 形制，拼成 `go test test` 报 `package test is not in std` 假失败，模型误诊为 rtk proxy artifact 烧约 25 次调用后带着未跑通的全量验证提交。公共 `stripDupSubcommand`（4 处重复抽 helper）。
- **git 子命令重复剥离**：模型高频把 subcommand 重复写进 args（`{"subcommand":"add","args":"add f.txt"}`），拼成 `git add add f.txt` 后报 `pathspec 'add' did not match` / `ambiguous argument 'log'`——实测会话连续十余次重试该形制全部失败且无法自愈。现在 args 首个 token 与 subcommand 相同时静默剥离（文件名恰等于子命令名时不误伤）。
- **glob `**` 与含 `/` 模式**：原实现只按 basename 匹配（`filepath.Match(pattern, d.Name())`），含 `/` 的 pattern 永不命中、`**` 不支持——实测会话连试 6 种 pattern 全 `no matches` 后模型放弃。现在：不含 `/` 的模式保持 basename 语义（历史行为）；含 `/` 的模式按相对路径逐段匹配，`**` 段匹配任意层目录（`**/app.css`、`internal/**`）。README 同步。
- **shell 挂起**（setsid 孤儿持有管道）：`sleep 30 &` 类后台任务逃逸进程组后持有 stdout fd，`cmd.Wait` 等待 exec copier 的 EOF 直到孤儿退出（实测 500ms 超时 6s+ 才返回）。`waitOutputTimeout` 按 ctx 剩余期限+2s 有界等待，孤儿场景按时放弃（泄漏进程为已知残留，见 platform.go）。
- **非 UTF-8 输出击穿滑动窗口**：无首字节的字节流（二进制/GBK）使 `utf8FullRune` 恒 false，pending 无界增长 + 每次 Read 全量重拷（4MB 输入实测 552MB 分配）+ 全部输出变 U+FFFD。pending 封顶 4 字节，超限残片按十六进制转义呈现。另修复 cut 循环误用 `chunk[cut-1:]`（该切片恒以 chunk 末尾结束，与 cut 无关）导致的有效 UTF-8 尾部错位。
- **孤立首字节误判完整 rune**：`utf8FullRune` 对尾字节为首字节（0x80+ 且非续字节）返回 false，跨 Read 边界的 rune 不再被拆写。
- **shell 缺跨块 UTF-8 缓冲**：`drainPipe` 抽出共享（含 pending 缓冲），shell 与 git/go/npm/lint 同一保护。
- **grep 根不可达误报 "no matches"**：打错路径/无权限的搜索根与真空结果不可区分（glob 侧正确传播）。根错误现传播为真实错误。
- **`git pull --upload-pack=<cmd>` RCE**：pull deny 表漏项，且 fetch 族辅助程序与任意子命令组合均可执行（args 拼在子命令后由 git 按 pre-command 选项解释）。`--receive-pack/--upload-pack/--exec/--repo` 入全局 deny 表。
- **git 聚簇短旗标绕过**：`push -qf`/`-df`（≡`--force`/`--delete`）经簇拼写绕过短旗标 deny。无 `=` 的多字母单破折号 token 簇内含被拒布尔短旗即拒。
- **`git push origin +main`/`:x` 绕过 force/delete 拒绝**：refspec 前导 `+`（≡`--force`）与 `:`（≡`--delete`）现被拒（git-push(1) 文档语义）。
- **子目录 `.gitattributes` 绕过外部驱动守卫**：只读仓库根的属性文件，子目录声明 `filter=` 即绕过。属性源扩为祖先链 + `<gitdir>/info/attributes`（`rev-parse --git-path`）+ 树内全部 `.gitattributes`（新 `tool_gitattr.go`）。
- **`go build --o /tmp/x` 绕过写路径约束**：`checkGoWritePaths` 只匹配单横线形，go 的 flag 包双横线等价接受。`matchTokenOption` 归一化匹配；`-outputdir` 入列表（旧注释称被 -o 前缀覆盖不成立）。
- **`go build -o sub/../../X` 中间 `..` 越界**：相对分支只拒 `../` 前缀、绝对分支 `withinDir` 无 Clean。`resolveWriteFlagValue` 统一按 Join/Abs+Clean+`filepath.Rel` 判界。
- **`go fmt <abs-outside>.go` 越界改写**：fmt 实参不经写路径校验（旧注释称"无越界写风险"）。fmt 的 FILE 位置实参现走同一校验。
- **npm `-registry=URL` 单横线绕过**：手写 HasPrefix 只查双横线形。改 `optSpec` + `checkDeniedOptions`（归一化匹配）。
- **lint deny 漏项**：`--new-from-patch`/`--new-from-merge-base`/`-n` 与 `--output.json.path`（任意路径写报告）补拦。
- **rtk 吞 lint findings 退出码**：rtk 的 golangci-lint wrapper 有 findings 也 exit 0（实测 6/6），exit_code 契约（22fac84）失效。lint 改原生 exec golangci-lint。
- **write/edit/read/shell 宽松解码静默毁数据**：`{"contents":...}` typo 曾清空整个文件并报成功、`{"new_str":...}` 曾把匹配文本替换为空串。四工具统一 `decodeStrict`；write 另拒空 content（空文件不可经 write 表达）。
- **read 1MiB 截断无标记**：截断点在行中间时最后一行是碎片（据此构造 edit old_string 必失配）、行数虚报。读 maxBytes+1 检测截断，去尾部残行并追加显式标记。
- **read limit>10000 静默钳制**：显式超限现报错（钳制返回行数少于默认全读且无标记）。
- **grep 恰好 N 条误报截断**：入口预检查在还有可走查文件时即置 truncated。改为只有超出上限的真实匹配才标记。
- **runWithTimeout 宽限窗改写晚到结果**：已落盘的 edit/write 成功被重贴 timeout 标签（LLM 据此对成功操作采取恢复动作）、exec 真实退出码被丢弃。晚到结果原样透传；删除只有命令输出才能偶然命中的 "timed out" 快路径。
- **shell 后台任务超时不可见**：leader 已 exit 0 而后台任务占管道到超时（组被杀、Wait 报旧状态 nil）时静默成功。现返回超时提示。
- **go/npm/lint 字节窗口按字符上限设置**：`maxOutputChars`（runes）被直接当字节窗口 keep，CJK 输出过度逐出（66666 汉字只剩 12208）。窗口改 8 倍。
- **预执行拒绝路径 exit_code=0**：deny/校验失败/解码错误未执行命令却发 `exit_code:0`（事件层读作成功）。`denyResult` 助手统一 ExitCodeNotSet；事件层仅在 `!IsError || ExitCode>0` 时置字段。
- **decodeStrict 不拒尾部垃圾**：`{...}{...}` 双拼 payload 静默取首个。第二个 Decode 须 io.EOF。
- **splitArgs 反斜杠-换行**：POSIX 行续接被当作转义换行，产出内嵌 `\n` 的 argv token（git 报 ambiguous argument）。行续接现正确移除。
- **confineWrap 对 rename 裸奔**：包装层只解析 `path` 字段，rename 的 `{from,to}` 零校验直通（symlink 目录逃逸仅在 write 侧被拦）。from/to 现各自走写侧检查。
- **`.git` 守卫大小写敏感**：Windows/macOS 文件系统 `.GIT` 即 gitdir，精确比较可绕过。两处 `dotGitWithinRoot` 改 EqualFold（8.3 短名为已知残留）。
- **edit 锁键未归一化**：`/w/./f` 与 `f` 两拼写各持一把锁，并行编辑丢失更新。锁键经 Clean。
- **writeFileAtomic 无 fsync**：崩溃窗口内 rename 先于数据块落盘会导致目标空/半文件（与 session_rewrite 同源的崩溃持久性问题）。补 tmp.Sync + 父目录 best-effort Sync。
- **grep 二进制嗅探窗口实际 4096**：默认 bufio 缓冲限制 Peek(8192)，4096-8191 区间的 NUL 漏检。`NewReaderSize(f, 8192)`。
- **schema 测试只覆盖 4/11 工具**、**git pathspec 测试死断言**（t.Logf 不失败）：补齐/改 t.Errorf。
- **glob 畸形括号漏检（复验补充）**：双探针法对任意长度探针都被字面量前缀短路（实测 `q*a[` 对 1..255 长度 x 探针全 nil）。补结构扫描器 `globPatternMalformed`（未闭合 `[`/尾部孤立反斜杠）。
- **npm `--C` 双横线绕过（复验新发现）**：`-C` 原列 shorts 分支不认双横线形，实测 `--C /tmp` 重定向前缀成功。改列 longs。
- **shell 纯二进制输出 U+FFFD 墙（复验补充）**：无效字节逐个变 3 字节替换符，字符上限被放大 3 倍且不可读。`escapeNonUTF8` 按十六进制转义无效序列，有效文本原样保留。
- **read 截断标记分页语义**：offset/limit 已收窄视图时标记改为 "this page shows only part of..."（原 "showing the first N bytes" 对分页读不准确）。
- **`rtkWrap` 无 rtk 主机上丢 prefix**：fallback 返回 `(bin, args)` 丢弃 prefix，`git status` 实际 exec 裸 `git`（usage dump + exit 1）、`go build ./...` → `go ./...` unknown command——rtk 覆盖的 8 git 子命令与 build/test/vet 在无 rtk 环境全坏。fallback 改拼 `prefix[1:]+args` 保住完整 argv。
- **git 短旗标/长选项缩写绕过 deny 前缀**（HasPrefix 精确匹配的系统性缺口，实测复现）：`push -f`/`-d`（--force/--delete 最高频短形）、`commit --am`（≡`--amend`，实测改写 HEAD）、`push --dele` 全部放行。重构为 `optSpec` 匹配器（flagmatch.go）：短旗标全等+等号粘合、go 单破折长名全等、git 双破折唯一前缀缩写识别；新增 `--mirror`/`--receive-pack`/`--exec`/`--repo`/`--pathspec-from-file` 入禁（push --receive-pack 实测 RCE 通道）。
- **push/pull 位置参数守卫漏相对路径与 scp 风格**：`../evil.git`（无 `://` 非绝对）实测整库推出、`--mirror` 强同步全部 ref；守卫改拒含 `:`/`/`/`\`/`..` 的首位置参数。
- **.gitattributes 误杀**：`diff=java` 类 hunk-header 属性（无驱动定义）曾一票禁用全部 git 操作含只读；现仅在驱动名**已定义**于 git config（`git config -l --null` 解析 `filter.<n>.*`/`diff.<n>.*`）时拒绝。
- **pathspec 按 repo 根解析**（子目录 workdir 静默错行为）：git 曾 `git -C <repoRoot>` 执行，`add x.txt` 在 workdir=repo/sub 时命中根下同名文件或假空 diff，与系统提示「相对路径基于 workdir」矛盾；cwd 改定 workdir。
- **非零退出码误报故障**：git/go/npm/lint 曾把 `git diff --exit-code`（有差异 exit 1）、`go vet` 告警、测试失败整段标 `IsError=true` 且 ExitCode 恒 0；对齐 shell 语义——语义性非零退出为正常结果（IsError=false + ExitCode + 完整输出），event 层 `exit_code` 同步放开到 git/go/npm/golangci-lint。
- **超时丢弃已捕获输出**：`runWithTimeout` 超时分支立即返回裸文案，已累积的尾部输出（挂在哪个测试/最后进度）全部丢弃；改 2s 宽限优先收 `runLimitedOutput` 的 kill-path 结果，带 `partial output follows` 前缀返回。
- **git/go 超时杀不掉孙进程**：npm/lint/shell 均已 `setPGID` 而 git/go 漏调，`killProcessGroup` 的 `kill(-pid)` ESRCH 回退只杀直接子进程，`go test` 超时后测试二进制孤儿化；补齐。
- **`splitArgs` 未闭合引号静默吞并后续 token**：`--grep=won't --oneline` 曾产出单参数 `--grep=wont --oneline`（撇号丢弃+吞 flag）；`splitArgsStrict` 报 `unterminated ' quote` 指引修复。
- **未知 JSON 字段静默忽略**：`{"subcommand":"add","command":"x"}` typo 曾落到空 args，`git add` 无 pathspec 即整仓暂存；`decodeStrict`（DisallowUnknownFields）拒并点名未知键。
- **deny 前缀误杀合法旗标**：`commit -am`/`-m"msg"` 粘合被 `requires -m` 拒（git 最高频写法）；`go test -work`/`-overlay`/`-cache=off` 被 `-w`/`-o`/`-cache` 前缀误拦；`log -F`（fixed-strings 只读）被全局 `-F` 拦；`diff -O<orderfile>`（只读排序）被当"写文件"拦；golangci-lint `-w` 前缀无对应语义。全部按作用域/精确匹配修正，`-m`/`-am`/`-mmsg` 粘合、`-o bin/app` 树内相对路径放行。
- **`go` 工具 deny 漏项**：`test -exec <prog>`/`vet -vettool <prog>`（任意程序执行，同 -toolexec 级）、`build -C <dir>`（chdir 越过模块根 confinement）、`clean -fuzzcache`/`-i`（全局缓存/安装产物）补拦；`-coverprofile/-memprofile/-trace` 等写路径旗标改值感知（树外拒、树内放行）。
- **输出上限配置不透传**：`run.shell_timeout` 同源的 `max_shell_output_chars` 只达 shell/grep/glob，git/go/npm/lint 硬编码 100KB；经 `buildTools` 透传 `Limits.MaxShellOutputChars`。
- **UTF-8 跨块截断**：32KB pipe Read 可停在多字节字符中间，滑窗起点半个 rune 序列化成 U+FFFD；`runLimitedOutput` 加 pending 残片缓冲（`utf8FullRune` 判界）。
- **`resolveModuleRoot` 死代码**：上溯循环第二轮必 break（dir 已越 startDir 前缀），等价只查 startDir 的 go.mod；删循环留单查。
- **subcommand 前后空白**：判空用 TrimSpace 但查表用原值，`" status"` 被拒且错误列表看似一致不可自诊；统一 trim 后查表。
- **描述漂移**：go 描述漏 `fmt`、npm 漏 `version`、git 只点名 18 子命令中 9 个；allow-list 列表改 `sortedNames(map)` 同源生成，描述/错误共用，并补超时时长与非零退出语义说明。
- `go` 工具经 rtk 代理时子命令重复（`rtk go build build ./...` → `package build is not in std`）：`rtkWrap` 的 prefix 含 subcommand 而 `cmdArgs` 也以 subcommand 开头，rtk 路径改传 `fields`（不含 subcommand），与 git 一致。
- `runLimitedOutput` 移除恒为 `maxShellOutputChars` 的冗余参数（unparam）；`git_splitargs_test.go` 的 `exec.Command` 改 `exec.CommandContext`（noctx）。

## [4.6.1] - 2026-08-14

> patch：纯重构与文档对齐，无 API / 配置 / CLI 行为变化。

### Changed
- **8 个超 300 行文件拆分至限额内**（纯重构，行为零变化）：`cmd/miniagent/main.go`（拆出 `setup_run.go`）、`compaction/assemble.go`（拆出 `budget_const.go`、`compacting.go`）、`compaction/history_dedup.go`（拆出 `history_dedup_shell.go`）、`config/config.go`（拆出 `config_load.go`）、`loop.go`（拆出 `loop_extra.go`）、`looptest/llm.go`（拆出 `llm_wire.go`）、`session/session.go`（拆出 `session_rewrite.go`）。
- **verify-gate 纳入行数硬检查**：`make verify` 新增非 `_test.go` 文件 ≤300 行强制项（超标即失败），与 AGENTS.md 编码标准对齐。

### Fixed
- 文档过期事实修正：HOOKS.md 钩子调用点路径（文件拆分后 `compaction/assemble.go` → `compaction/compacting.go` 等）、`OnStep` error 契约补全；ARCHITECTURE.md 对齐拆分后核心子包结构；`loop.go` 顶层 panic 兜底注释同步。

## [4.6.0] - 2026-08-13

### Added
- **workdir 绝对路径注入 system prompt**：`assembleSystemPrompt` 在 rules 之后、subagent guidance 之前无条件追加一行 `Working directory (absolute): <wd>`（workdir 为空则跳过）。workdir 来自 `-workdir` flag（非文件来源），与已注入 configAbsPath 的 guidance 同属运行时信息；此前模型只能 `pwd` 才获知自身工作目录。实现：`appendWorkdirLine`（prompts.go）。

### Changed
- `make deploy` 不再自动构建（移除对 `build` 的隐式依赖），仅执行 `sudo install`。原先 `make deploy` 一步到位构建+安装，现须先 `make build` 再 `make deploy`。安装前用旧二进制的风险消除，部署意图更明确。

## [4.5.0] - 2026-08-12

> minor：default 模式新增两个 opt-in shell guardrail（`run.shell_allowlist`/`run.shell_confine_cd`）+ opt-in 项目规则文件（`defaults.rules_file`）；workdir 收口为 `-workdir` 单一来源（**breaking**：删 config `run.workdir`、所有模式必填且须绝对路径、去 cwd 回落）；retry 原语抽 `internal/provider/httpretry` 公共包。新增项均 opt-in、默认关；既有 openai 行为零改动。

### Added
- **default 模式 shell guardrail（opt-in）：`run.shell_allowlist` + `run.shell_confine_cd`**（`internal/miniagent/tools/tool_shell_guard.go`，`tools.GuardShell`）。两个独立开关，默认关、保持现状；仅 ModeDefault + 非空 workdir 生效（auto 仍无约束）。
  - `shell_allowlist`（`[]string`）：命令名白名单，管道/链式（`a | b && c; d`）**每段**都校验，**精确匹配**（`/usr/bin/git` 不命中 `git`，须逐字列入，避免路径混淆绕过）。词法分词器处理引号/`VAR=val` 前缀/`&&`/`;`/`|`/`&`/子 shell `()`，并把 `2>&1` 等 fd 重定向保持在词内不误切。
  - `shell_confine_cd`（bool）：词法拦 `cd`/`pushd` 越出 workdir（绝对路径、`..`、`~`、`$VAR`、裸 `cd`→HOME、`cd -`→上一目录）。
  - **性质**：best-effort **词法 guardrail，非安全边界**——可被 `eval`/`$()`/反引号/别名绕过，与既有 sudo/su 拒绝名单同框架（不变量 #13 不变）。经 `buildTools` 在 default 模式条件包装 shell 工具（镜像 `confineWrap`），不改 `ShellTool` 签名；真隔离仍靠调用方 OS 层。
- **config `defaults.rules_file`（opt-in）**：workdir 下的规则文件追加到 system prompt（base 之后、subagent guidance 之前），默认空 = 不启用（现状 config-only 不变）。须为纯 basename（拒 `..`/绝对路径/子目录，防越界读注入 prompt——default 的 workdir 约束在工具层、管不到 main 层装配）；不存在静默跳过，读失败/>64KiB stderr 警告不致命（截断）。v4.4.0 移除无条件 `.miniagent/rules.md` 后的 opt-in 回归，满足「项目规则 + 保留内置默认工作流约束」（L2 `system-prompt-config-only.md` 迁移指引点名的风险点）。

### Changed
- **retry 基础设施抽公共包 `internal/provider/httpretry`**：openai 与 anthropic 两 provider 原各自复制一份厂商无关的重试原语（常量 `MaxRetries`/`RetryBaseDelay`/`RetryMaxDelay` + `ParseRetryAfter`/`CapRetryDelay`/`SleepCtx` + 可重试状态码分类），现抽到共享包；`ShouldRetryStatus` 参数化为「公共 429/5xx 基线 + 厂商 `extra` 码」，anthropic 经此传入 529（overloaded_error），各 provider 仅私留厂商特定的 `isThinkingError`（thinking 协议本质不同，不可合并）与 `shouldRetryStatus` 薄包装。从结构上根治「复制后改一漏一」的对称维护负担（L2 incident `anthropic-provider-copy-asymmetry` 的起因），生产代码净减约 80 行。纯内部重构——对外契约（LLM 接口 / NDJSON / config / CLI）零变化。

### Removed
- **`run.workdir` 配置项删除 + `-workdir` 收口为单一来源（breaking，config + CLI 双面）**：workdir 不再可从 config `run.workdir` 取（`RunConfig.Workdir` 字段移除、`CLIOverrides.Workdir` 移除），也不再回落进程 cwd（`absWorkdir` 去 `os.Getwd()` 兜底）——唯一来源是 `-workdir` flag。同时 `-workdir` 由「仅 default 模式必填」收紧为**所有模式必填**（`-mode auto` 也不能省），且**须绝对路径**（`-workdir .` / 相对路径报错退出 1）。理由：config 取 + cwd 回落会让 session 元数据的 workdir 字段随运行环境漂移、且与 default 模式工具边界所用的 workdir 可能不一致，单源化后可预测、可审计。**迁移**：config `run.workdir` → `-workdir` flag；`-workdir .` 或相对路径 → `-workdir "$PWD"`；auto 模式脚本里省略 `-workdir` 的需显式补绝对路径。

### Fixed
- **default 模式 confineWrap 误伤只读工具的 workdir 根路径**：`checkConfine` 的「path 指向 workdir 根即拒」（防 write/edit 的 MkdirAll/Rename 覆盖整个 workdir，review P3-8）被无差别套用到 read/grep/glob，致 `grep path=<workdir>` 或 `path="."`（解析为根）在 default 模式被拒——读/列举整个 workdir 本是合法操作。`confineWrap`/`checkConfine` 加 `readOnly` 形参（显式必填，置于 `evalSymlinks` 可变参前）：read/grep/glob 传 true 跳过根覆盖检查（越界 / symlink 检查不变），write/edit 传 false 保持原拒。继 v4.4.0「空 path 直通」后再修一类只读工具误伤。

## [4.4.0] - 2026-08-11

> minor：首个非 openai 上游——`anthropic` provider（Messages API）正式落地，含流式 / prompt caching / interleaved-thinking / 签名跨轮；配套 core 层「LLM 端点 400 硬错」补丁。另含两项 breaking：内置工具 10→6（移除 codemap/todo）、`.miniagent/` persona/rules 自动加载收口（system prompt 统一 config-only）。新增 provider / 配置项均 opt-in，既有 openai 行为零改动。

### Added
- **`anthropic` provider（`internal/provider/anthropic`，Messages API）**：新增 `ProviderKind=anthropic`，`cmd` 按 Kind 字符串分派返回统一 `LLM` 接口，核心领域类型（`Message/Request/Response/Usage`）与 openai 包零改动。
  - **流式 + 中断处理**：SSE 逐事件解析 `message_start/message_delta/message_stop`，`StreamAllowUnterminated`（config `stream_allow_unterminated`）兼容非合规端点。
  - **prompt caching**：`cache_control` 落首/尾 user 消息块，按 `cache_creation/use` 统计。
  - **interleaved-thinking**：thinking 经 `provider.ThinkingMapping` 的 Map 值作 JSON 对象串下发（如 `{"type":"ephemeral"}`），启动期 `validateThinking` 前置校验。
  - **签名跨轮**：多轮 wire 层 DROP 历史 thinking，保 tool-call 一一对应。
- **config `provider.stream_allow_unterminated`**（bool，opt-in，默认 false）。

### Changed
- **core 层「LLM 端点 400 硬错」补丁（`llm_endpoint_400_hard_error`）**：`loop.go:Run` 主路径遇 `errHTTP400` 直接 `return err`，不再落入 `OnLLMError` 透明重试后 `continue`——400 是协议层坏请求（payload 不合法），重试无意义，静默继续会吞掉本次回复致后续上下文/压缩失真。`OnLLMError` 仅处理瞬时/可恢复错误（限流 / 5xx / 超时）。
- **`make deploy` 改用 `install -m 0755` 替代 `mv`**：跨文件系统 / 覆盖运行中二进制更稳，显式设权限。
- **`loopCfg` 移除空 `System` 兜底**（原 `if system=="" → defaultSystemPrompt` 删除）：dead-code 清理，生产路径由 `assembleSystemPrompt` 保证非空。

### Fixed
- **compaction tail 预算回归（`jointTailBudget` override 路径）**：custom `summarizer_prompt` 下，单一旧 `KindSummary` head 被 `SummarizerPrompt != ""` 子句双重扣减，tail 预算被错误压到 0 致小窗口压缩失败；与 default 路径对齐（`headAdj=0`），附回归测试。
- **`system_prompt` 去重**：config 已提供 `system_prompt` 后不再重复追加 `defaultSystemPrompt`。

### Removed
- **`codemap` 工具移除**：递归目录树概览与 glob+read 功能重叠，是专用工具里边际最低的一个；不符合极简默认工具集定位。模型改用 glob（结构）+ read（内容）组合感知仓库布局。
- **`todo` 工具移除（`todo_create`/`todo_update`/`todo_list`）**：进程内任务清单是纯内存脚手架（每 Run 新建、不持久、无 loop 逻辑读取它），属认知策略而非原语能力，与核心零策略/极简定位冲突。模型可在正文跟踪任务。
- **breaking（工具面契约）**：内置工具 10 → 6（`read`/`write`/`edit`/`grep`/`glob`/`shell`）。无配置迁移（此前无 per-tool 开关）；下游若依赖这两个工具，改用 glob/read 组合或在调用方自行装配等价工具经 `LoopConfig.Tools` 注入。
- **`.miniagent/persona.md`/`rules.md` 自动加载移除（breaking）**：system prompt 来源统一为 config-only（`defaults.system_prompt`，未配则内置默认 `defaultSystemPrompt`），不再启动期读 `workdir/.miniagent/` 下的 persona/rules 文件。删 `cmd/miniagent/project.go`（`loadProjectRules`/`mergeSystemPrompt`/`projectRules`/`readTrimmedFile`）+ `project_test.go`；`assembleSystemPrompt` 砍 `pr` 参数、简化为「base 兜底 + 注入 subagent guidance」。继全局 `~/.miniagent/` 层（已删）之后的第二次收口。**迁移**：原 persona 内容直进 `defaults.system_prompt`（「取代默认」语义等价）；原 rules 为「追加」语义，物进 `system_prompt` 文本时需自行保留内置默认的工作流约束（或接受其丢失）。`.miniagent/` 目录保留用于 session 存储。

### Notes
- `.agent/` 记忆已同步（L1 session + 本轮 commit）。
- 新增 Makefile `verify` 目标（聚合 AGENTS.md 五步 gate：gofmt 空 / `go build ./...` / `go vet` / `go test -race` / lint）；`release.sh` 改用 `make verify`。

## [4.3.1] - 2026-08-10

> 维护性 patch：无功能性变更，不影响 API / 配置 / CLI 行为 / NDJSON 契约。

### Changed
- 发布二进制加 `-ldflags "-s -w"`：剥离符号表与 DWARF 调试信息，产物体积减小约 30%（9.7M → 6.8M）。
- `compaction_split.go` 反向遍历改用 `slices.Backward` 迭代器（Go 1.23+），消除 `modernize` lint 告警。

## [4.3.0] - 2026-08-09

> 观测性 + 安全护栏加固 minor：新增 `OnStep` 观测缝与 `-metrics-step` 每步指标、危险工具确认门、流式中断检测；default 模式文件工具 confine 提供 opt-in 收紧；多项 compaction / 计数 / 落盘可靠性修复。全部向后兼容——新增配置项与 CLI flag 均 opt-in，既有配置与行为不变。

### Added
- **`OnStep` 观测缝 + `metrics` 子包 + `-metrics-step` CLI**：`LoopHooks` 新增第 8 个钩子 `OnStep(ctx, StepSnapshot)`——每步迭代顶部触发（先于 BeforeLLM/LLM/tools），observe-only 无 error 返回，nil 即零开销。`StepSnapshot` 携带 step/transcript_len/input_tokens/output_tokens/compacted/llm_requests/new_messages（全部取自手边状态，无额外扫描）。`internal/miniagent/metrics` 提供默认消费方 `NewStepEmitter`（NDJSON writer）；cmd 层 `-metrics-step` 接线到 stderr，把长会话行为（transcript 增长、token 消耗斜率、压缩次数、每轮请求数）变成可采集曲线。
- **危险工具确认门 `policy.ConfirmOnToolUse`**：`-confirm-destructive` 或 config `run.confirm_destructive`（config > CLI）启用——按 name/pattern 匹配拦截危险工具调用并要求人工确认，子 agent（`-mode auto` 等无 stdin 场景）default deny；`MINIAGENT_AUTO_APPROVE=1` 跳过（沙箱/容器内自动化）。
- **流式连接中断硬错检测**：`parseSSE` 在已有 content 流出但无 `[DONE]` 且无 `finish_reason` 时判为连接中断，返回 `errStreamUnterminated`（不再静默把半截回复当成功）。vLLM/Ollama 等非合规端点可经 provider 配置 `stream_allow_unterminated: true` 放宽。同步加 tool_call index 碰撞防护：同 index 不同 name 直接报错，防省略 index 的端点静默覆盖工具调用。
- **default 模式 confine opt-in 收紧**：新增 `run.confine_eval_symlinks`（路径最终 `filepath.EvalSymlinks` + 双重 root 解析再比对前缀，收窄逐段词法检查遗留的并发 symlink 交换 TOCTOU 窗口；ENOENT 竞态回落词法结果，保 create 语义）与 `run.confine_auto`（auto 模式下也给 read/write/edit/grep/glob/codemap 套 confineWrap，shell 仍自由——纵深防御，非安全边界）。默认均 false，既有语义不变。
- **`run.max_iterations` config 可配**：迭代上限从纯 CLI `-max-iterations` 扩为 config `run.max_iterations`（cli > config 裁决），默认 20 不变。
- **累计 LLM 请求计数**：`Result.LLMRequests` 统计单轮实际发给 LLM 端点的请求数（thinking 降级重试 / OnLLMError 收紧重试 / 总结步全计，不含 provider 层透明重试）；`result` 事件输出 `llm_requests` 字段；session jsonl 首行 meta 加 `llm_requests` 跨轮累加。session meta 为首行需原子重写——saveSession 对该路径统一走 `RewriteMessages`（临时文件 + rename，append-only 语义不受影响）。

### Fixed
- **损坏摘要注入防护（P0）**：摘要输出经结构校验——含 `<tool_call>` / code fence 标记、或默认模板下缺 ≥2 个章节标题行（按行精确前缀匹配，防子串绕过）判定为 garbage → 附 strict "OUTPUT PROSE ONLY" 指令重试一次 → 再失败 surface error，`FitHistory` 回落 lossy 压缩。堵死「压缩产出 tool_call 脚本被原样落盘为 KindSummary、复活后当 prompt 注入执行」向量；重试 usage 计入预算（对齐降级链语义）。消费侧零改动（`applyCompactionBarrier` 仍纯 Kind 判别）；自定义 summarizer prompt / template 时仅做 markup 拒绝。
- **`LLMRequests` 降级路径少计**：`callLLMWithDowngrade` 返回请求数（降级=2）但三处调用点恒 `+1`，降级重试被少计；改为累加返回值，补主路径 / 总结步 / OnLLMError 重试三路测试。
- **`Run` 顶层 panic 兜底**：任一用户钩子 panic 不再崩进程——顶层 recover 后 transcript 保活供 session 落盘，错误照常返回。
- **finish_reason=length 空回复防御**：`length` + 空 text + 有 reasoning（推理烧光输出预算）时，主路径自动去 thinking 重试一次；压缩路径 `summarizeMiddle` 上抛该信号由 `FitHistory` 降级到 lossy 压缩，不再产出空摘要。
- **compaction 压缩质量加固**：`compactWithSummary` 遇已有 KindSummary 先剥离再入中段（新摘要覆写旧，防陈旧摘要累积致语义稀释）；post-compaction tail 保留 reasoning（`context_keep_reasoning` 扩展到 tail 段）；`dedupReadResults` 等增强 exact/near-duplicate 检测；各层截断标记统一为英文并对齐 `policy.TrimForHistory` 的 split marker。
- **`compactionReserve` 解耦 `maxTokens`**：压缩预留改 flat 20000 buffer，不再随输出上限浮动（配大 max_tokens 时压缩预留被隐性放大）。
- **`tool_output_store` 文件数硬上限**：新增 `toolOutputMaxFiles=500`，cleanup 先按 age 过期、再按最老 mtime 淘汰，防 dense large-output runs 在 retention 窗口内无限积盘；保护 read-back 契约（模型最近收到「已保存」提示的文件优先存活）。
- **session 路径解析加固**：`resolveSessionForRun` 非交互模式报错更明确（区分不存在 / 不可读 / 非法路径）。
- **流式工具并发退出**：`runToolsParallel` 在 ctx 取消时，信号量排队中的调用直接放弃，不再阻塞等空位。
- **tool_write 新文件权限收紧**：新文件默认 0644→0600（多用户主机更安全），覆盖写保留原文件权限。
- **i18n**：全部 `.go` 文件（生产+测试）注释 / 错误消息 / 工具描述 / 提示词模板中译英；CJK 测试 fixture 保留（rune 边界 / token 估算语义）；术语统一（session/context/hook/budget/compaction/truncate/downgrade/persist）。

### Notes
- `.agent/` 三层项目记忆系统入库（L0 约束 / L1 会话 / L2 经验 + `CLAUDE.md` 工作区规则），纯文档，无代码影响。

## [4.2.1] - 2026-08-08

> 内部架构重构（续 4.1.0/4.2.0）：core 包进一步子包化——工具实现、策略钩子、配置、会话各成独立子包。CLI 行为与 NDJSON 事件契约零变更，属非破坏性 patch。

### Changed — 内部包重组
- **`internal/miniagent` 拆出 `tools` 子包**：`tool_read`/`edit`/`write`/`grep`/`glob`/`codemap`/`shell`/`todo` + `output_accum`/`tool_helpers` 从 core 迁入 `internal/miniagent/tools`，消除 core 对具体工具实现的耦合。
- **`internal/miniagent` 拆出 `policy` 子包**：`NewDefaultOnLLMError`/`NewDefaultOnBudget`/`NewDefaultShapeToolResult`（原 `loop_hooks_default`）、`EstimateTokens`/`trimHistoryForContext`（原 `history_util`）、`toolOutputStore`、`TrimForHistory` 从 core 迁入 `internal/miniagent/policy`，延续 4.2.0 的「核心策略外挂」——core 零策略，仅做工具注册 / 上下文拼接 / 调 LLM / 执行工具 / 无 tool_calls 退出。
- **`internal/miniagent` 拆出 `config` 子包**：`config.go`/`url.go`/`resolve.go`（含分层 / 二级源 resolve）从 core 迁入 `internal/miniagent/config`。
- **`internal/miniagent` 拆出 `session` 子包**：`session.go`/`validate.go` 从 core 迁入 `internal/miniagent/session`。
- **新增 `looptest` 子包**：导出版 LLM 测试桩（`FakeTransport`/`RecordingTransport`/`FakeLLM` + `TextResponse`/`ToolResponse`），供 `miniagent` 与 `policy` 外部测试包共享，破除 core → openai 测试环。core 内部保留白盒测试桩（`loop_helpers_test.go`）避免成环。

### Added — session 文件锁
- **跨进程 session 文件锁**：`session/lock_unix.go`（flock `LOCK_EX|LOCK_NB` + 5s 短轮询）与 `session/lock_windows.go`（`LockFileEx` 字节区间锁），防跨进程并发写同一 session 文件竞争。

### Notes
- 与 4.1.0/4.2.0 一脉相承的渐进式解耦。真正库化（移出 `internal/`）仍计划于 5.0.0。

## [4.2.0] - 2026-08-08

### Fixed
- **`summaryMaxTokens` 与 `summaryMaxChars` CJK 不一致修复**：原固定 `summaryMaxTokens=1024` 对 CJK 偏紧（1024 token≈1500 汉字，远低于 `summaryMaxChars=5000`），中文摘要被 `MaxTokens` 隐性截短到设计值约 30%；现默认从 `summaryMaxChars` 派生（`/2`，与 `EstimateTokens` 的 `CJK≈1token/2chars` 同口径），保证纯中文摘要填满字符上限不被 token 先截。新增纯函数 `deriveSummaryMaxTokens` 在 `NewCompaction` 装配时派生——只配 `summary_max_chars` 时 token 自动跟随，显式配 `summary_max_tokens` 仍可覆盖。
- **`compactWithSummary` tail 预算改为联合预算（方向 B）**：原 tail 按 `preserveRecentTokens` 独立选取，不考虑摘要+head 已占空间，中等 ContextWindow（约 5k）下 `head+summary+tail` 超 `CW×4/5` → `trimRecentRounds` 后仍超 → 终止 error（白烧摘要调用）。新增 `jointTailBudget`：tail 预算 = `min(CW×4/5 − 请求开销 − head − 摘要估算, preserveRecentTokens)`，使不可压缩的 summary 优先、tail 让步，从源头消除中等 CW 的压缩后终止（实测 CW=5120 由终止→不终止）。UPDATE 路径下旧 summary 不进 out 时精确不扣 head。**边界**：极小 CW（≤~4k，summary 本身装不下 `CW×4/5`）仍终止，需 `summaryMaxChars` 随窗口缩放（已由下条方向 A 解决）。
- **`summaryMaxChars` 默认随 ContextWindow 缩放（方向 A）**：原固定 5000 在小 CW（≤~4k）下 summary 本身（~2500 token）> `CW×4/5` 致压缩后终止（B 的边界）。现默认 `min(5000, context_window/5)`（summary token 占 CW ~10%），小窗口自适应避免 summary 装不下；大窗口（CW≥25000）仍取 5000。与 B 联合预算、`summaryMaxTokens` 派生三者联动构成「summary 体积自适应 CW」体系；用户显式 `summary_max_chars` 仍覆盖。实测把「压缩不终止」CW 下界从 ~5120 降到 ~1536。**硬边界**：CW<~1536 仍可能终止（请求级 overhead + head 占比过高，物理极限，非 A 范围）。
- **`windowStartOf(keepN<=0)` 语义修正**：原 `keepN<=0` 返回 0，被 `dedupReadResults`/`foldStaleReadResults`/`foldStaleWriteEditArgs`/`dedupShellCommands` 解读为「全窗口内=全保留」，与 `keepN=0` 的「不保留=全压」语义矛盾。改为返回 `len(msgs)`（全窗口外=全压）。释放摘要前 middle strip 的 dedup/fold（中段重复 read / 被后续写入取代的旧 read / 同义 shell / 被取代的 write-edit args 现被去重），并修正 `context_keep_tool_args=0` 的语义；生产默认 `context_keep_tool_args=2` 不受影响。

### Changed — 配置体系增强（breaking）
- **摘要前对 middle 全压 strip**：`compactWithSummary` 原把原始中段（含 reasoning / tool 结果 / write-edit args）直接喂给摘要 LLM，现摘要前对其全压 strip（`keepN=0`）——省摘要 input token（实测 ~56%，主为清中段历史 reasoning）+ 防 `middle + summaryMaxTokens` 超摘要模型 CW 致摘要失败回落。权衡：摘要 LLM 不再见中段历史 reasoning（要点仍在正文 / tool_calls，与 context 侧 `stripStaleReasoning` 同取舍）。注：dedup/fold（P6/P11/P8'/P9b）经 `windowStartOf(keepN=0)=0` 在 middle 全压时不生效（全窗口内），故当前主要省 reasoning；中段 tool 结果去重留待后续（需调整 windowStartOf 语义）。
- **Commit 语义改：非压缩步不替换 transcript，压缩 tail 保留原文**：`before` 原每步 `Commit=true` 把 strip 写回 transcript，致压缩轮 `RewriteMessages` 落盘 strip 版本（reasoning/args 滚动丢失），与 `stripStaleReasoning` 注释「持久化留原文」矛盾。现 `FitHistory` 加 `committed` 返回值：非压缩步 `committed=false`（strip 仅本轮 View，transcript 保留原文）；仅压缩步 `committed=true` 替换为 `[head,summary,tail]` 且 tail 不再 strip（原文 reasoning/args 保留）。LLM 所见不变（每轮 strip View），消除压缩轮持久化不对称（落盘完整近期上下文）。代价：transcript 内存略增（原文 vs strip，≈ newMsgs 量级）。`fallback`（摘要失败）仍 strip+committed=true 防循环。

- **thinking 字段名+枚举值纯钉死（breaking）**：provider.thinking 从「可选覆盖默认」改为「启用思考时必声明 {field,map}」。`validateThinking` 只查 provider.map keys（去 `standardThinkingLevels` 原样接受标准级别）；`validateConfig` 强制 `defaults.thinking≠off` 时 provider 必声明 `field≠""`+`map` 非空；wire 必经 `req.Thinking.Map[level]`（移除默认 `reasoning_effort` 与「原样传 level」）；`thinkingFieldBlacklist` 移除 `reasoning_effort`（钉死后是合法显式 field）。**迁移**：现有 openai 配置补 `thinking:{field:"reasoning_effort", map:{off→off,minimal→minimal,low→low,medium→medium,high→high,xhigh→xhigh,max→max}}`（见 config.example.json）。收益：枚举前置校验（level∉map 启动期报错而非端点 400→降级）+ 配置即文档（无隐式 reasoning_effort 假设）。降级机制（`isThinkingError`/`callLLMWithDowngrade`/`captureDowngrade`）不变。
- **模型参数三层覆盖（breaking）**：`max_tokens`/`context_window`/`thinking` level 支持 `model > provider > global` 分层；`http_timeout` 仅 `provider > global`（传输层属性，无 model 级）。`ProviderConfig.Models` 由 `[]string` 改为 `[]ModelConfig`（对象数组）。取消 `-max-tokens` CLI（max_tokens 纯 config 分层，三层全未配则不发 `max_tokens`、回落模型自身默认；原 CLI 默认 4096 移除）。`-thinking` CLI 保留（`cli > model > provider > global`）。**迁移**：config `models:["x"]` → `[{"name":"x"}]`；`-max-tokens N` → config `run.max_tokens` 或 provider/model 级 `max_tokens`。
- **提示词模板全面 config 化 + 占位符统一（breaking）**：新增 `defaults.subagent_guidance`（占位符 `{config_path}`/`{mode}`）、`summary_create_instruction`/`summary_update_instruction`（`{max_chars}`）、`summary_template`（无变量），空用内置默认。`summarizer_prompt` 占位符由 `%v` 改为 `{max_chars}`（与新增字段统一命名占位符，`strings.NewReplacer` 替换，无 `%%` 转义）。**迁移**：用户自定义 `summarizer_prompt` 含 `%v` 改 `{max_chars}`。

### Added — 配置体系（`-replay`）
- **`-replay <id>`**：离线回放指定 session——读 jsonl 重显为与运行时同构的 NDJSON 事件流（`session`/`tool_use`/`tool_result`/`text_delta`/`result`），不调 LLM、不需 key、不读 stdin、不落盘。与 `-save-session`/`-session`/`-result-only` 互斥。已知精度边界：`text_delta` 整串一次发（session 无逐块切分）、`tool_result` 双重截断且无 `exit_code`、压缩过的 session 回放压缩后快照、`result.finish` 恒 `stop`。

### Removed — 配置体系（breaking）
- **项目级脚本工具（`.miniagent/scripts.json` → `script_<name>`）**：移除该功能（删 `internal/miniagent/tool_script.go` + 测试、`cmd/miniagent/project.go` 的 `scriptDef`/scripts 加载/merge、`buildTools` 的 `scripts` 参数与注册块）。**Breaking**：已用 `scripts.json` 声明的项目失效，需改用 `shell` 工具直接跑命令（失去「固定命令 + 限制参数」语义，shell 为自由命令）。收益：工具装配收窄为 7 内置工具、`buildTools` 签名简化、收窄 flag 注入面（`shellQuote` + 拒 `-` 开头防护随之移除）。`runShellCommand` 保留（shell 工具专用）。
- **`-system` CLI + 全局 `~/.miniagent/` 规则层（breaking）**：`-system` CLI 移除——system prompt 现仅来自 config `defaults.system_prompt`（未配则内置默认）+ 项目级 `workdir/.miniagent/`。全局 `~/.miniagent/persona.md`/`rules.md` 不再读取（取消双层查找的 home 层，`loadProjectRules` 简化为单目录读取，删 `mergeProjectRules`/`personaSet`/`rulesSet`，净减 ~30 行）。**迁移**：全局 persona/rules 物进 config `defaults.system_prompt`（persona「取代默认」语义与 system_prompt 等价；rules 为追加语义，需物进 system_prompt 文本）；`-system` 改 config。

> **核心策略外挂（前期改动，CLI 与 NDJSON 事件契约零变更）**：用量估算+预算判定、LLM 失败恢复、工具结果成型+落盘从 `Run` 内联移到默认钩子工厂，`Run` 真正零策略（仅做工具注册 / 上下文拼接 / 调 LLM / 执行工具 / 无 tool_calls 退出五件事）。

### Changed — 核心策略外挂（internal）
- **核心策略从 `Run` 内联移到默认钩子工厂**：原核心内置的三项策略——零 usage 本地估算 fallback + `MaxTotalTokens` 预算判定、`ErrContextLength` 历史收紧重试、工具结果截断（`trimForHistory`）+ 超限落盘（`toolOutputStore`）——提取为 `NewDefaultOnBudget` / `NewDefaultOnLLMError` / `NewDefaultShapeToolResult` 三个导出工厂。`Run` 不再读取 `LoopConfig` 的策略字段，仅累加真实 usage、不做估算/判定/截断/落盘/错误恢复；`Run` 注释承诺的「核心不做任何上下文管理」至此与实现一致。
- **`OnBudget` 扩签以承载估算**：签名从 `func(step int, total Usage) error` 改为 `func(ctx, step int, in BudgetInput, total *Usage) error`（新增 `BudgetInput` 携带本轮 `ToSend`/`System`/`Tools`/`Resp`，`total` 改指针可累加估算）。原核心内联估算无法外挂的根因——钩子拿不到请求侧上下文——由此消除。
- **`ShapeToolResult` nil 时核心透传原文**：此前 nil 走核心内置 `defaultShapeResult`（截断+落盘）；现 nil = 零成型、透传 `ToolResult.Output`，成型完全交钩子。
- **cmd 层一行装配默认钩子**：`main.go` 用三个 `NewDefault*` 工厂替代原自写的 `OnBudget` 闭包；`store` 连同落盘逻辑整体下沉进 `NewDefaultShapeToolResult`（构造时 `cleanup` 一次，等价原 `Run` 入口行为）。

### Added — 核心策略外挂
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
