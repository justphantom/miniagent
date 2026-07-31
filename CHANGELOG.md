# Changelog

所有显著变更进入此文件。格式参考 [Keep a Changelog](https://keepachangelog.com/)，
版本号遵循 [Semantic Versioning](https://semver.org/)。

## [Unreleased]

### Added
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
