# Changelog

所有显著变更进入此文件。格式参考 [Keep a Changelog](https://keepachangelog.com/)，
版本号遵循 [Semantic Versioning](https://semver.org/)。

## [Unreleased]

### Changed
- 工具重命名：`read_file` → `read`、`write_file` → `write`、`edit_file` → `edit`
  （`shell` 不变）。工具 schema 属于外部契约，消费方需同步更新。

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
