---
layer: L2
type: pattern
tags: [webui, roadmap, iteration, ux, dsh]
created: 2026-08-25
confidence: evolving
---

# WebUI 迭代路线图（沉淀自 docs/WEBUI_NEXT.md，2026-08-25）

> 来源：对比 DeepSeek Harness (DSH) WebUI 功能集（`dsh-client-ui-*` 插件包名清单）为 miniagent 定位差距。DSH 清单为外部参照，不可独立验证，仅作优先级输入。原过程文档已删，本条目为唯一存续版本。

## 现状（截至 2026-08-25，v6.6.6）

已落地：多视图并发 / NDJSON 流式 / 断连不杀轮次 / stop API；生命周期流 + 旁观模式；x-api-key + Host 白名单 + CSP/XFO/nosniff + CSRF 415；markdown 子集渲染 + XSS 转义；300ms 节流流式渲染 + thinking 流；全字段配置表单（6 分组 60+ 字段，secret 掩码，原子写回）；systemd 部署。**原 P0/P1 项中已实现**：工具轨迹面板（step 分组卡片 + `step_usage` 明细）、目录选择器（`GET /api/tree`）、token 用量可视化（状态栏 per-session 累计 + 每轮进度条）。

## 剩余缺口（按优先级）

### P0 — 高频交互空白

| 能力 | 说明 | 理由 |
|---|---|---|
| 附件/文件上传 | 输入框拖拽/粘贴/选择文件，读取内容为消息附件 | 免手动 `cat`/`read`，最常被提的需求 |
| 会话重命名 | 列表仅显示 id + 预览，无法标记重要会话 | 需后端 rename 端点 |

### P1 — 明显提升体验

| 能力 | 说明 |
|---|---|
| 会话搜索/过滤 | 侧栏按关键词过滤已有列表（零后端） |
| 命令面板 | `/` 唤起命令（`/compact`、`/retry`）、`@` 引用文件 |
| 会话内模型切换 | 当前下拉为全局；改 per-session 需设计 meta 切换策略 |
| 消息反馈 | thumbs up/down 回传后端（评估数据来源，价值待定） |

### P2/P3 — 增强与专业场景

会话导出/导入（NDJSON 下载 + multipart 导入）、后台任务进度、目标跟踪、计划展示、引用/来源标注、子代理面板、技能管理、交付物面板、权限预设、国际化、插件管理。**均不推荐当前投入**。

## 架构兼容性（剩余项）

| 能力 | 后端改动 | 前端量 | 可行性 |
|---|---|---|---|
| 文件附件 | `POST /api/upload` 或 base64 inline（零后端） | 中 | ✅ |
| 会话重命名 | `POST /api/sessions/{id}/rename`（jsonl append-only 不可就地改首行 meta，需 `.meta` 旁挂文件或 rewrite） | 小 | ✅ |
| 会话搜索 | 无 | 小 | ✅ |
| 会话内模型切换 | session meta 存储改动 | 中 | ⚠️ 需设计 |
| 导出/导入 | 新端点 ×2 | 中 | ✅ |
| 子代理 UI | 新端点 | 大 | ⚠️ 需评估 |

## 推荐首步（附件 + 目录增强）设计要点

- 优先 inline：前端 `FileReader.readAsText` 嵌入用户 prompt（零后端）；大文件再考虑上传端点。
- 交互面：`#composer` dragover/drop + `#prompt` 粘贴检查 `clipboardData.files` + 文件选择按钮 + 附件行内列表（名/大小/删除）。
- 安全：前端 10MB 限制、后端复用 `max_read_file_bytes`；二进制跳过（同工具层 read 的 NUL 检测）。

## 待决策清单

| 问题 | 倾向 |
|---|---|
| 附件 inline 还是上传引用 | inline 起步（零后端），大文件后补上传 |
| 轨迹面板展开状态持久化 | localStorage per-session |
| 重命名存储位置 | `.meta` 旁挂文件（不动 append-only 语义） |
