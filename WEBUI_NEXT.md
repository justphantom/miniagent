# miniagent WebUI 下一步迭代方向
> 对比 DeepSeek Harness (DSH) WebUI 功能集，为 miniagent 定位。  
> 参考：DSH 客户端插件包名清单（`dsh-client-ui-*`），miniagent 当前能力（会话内存 + 代码）。

---

## 一、现状全景

miniagent 当前 WebUI 已完成：

| 领域 | 能力 |
|---|---|
| 会话 | 多视图并发、NDJSON 流式、断连不杀轮次、stop API |
| 跨浏览器 | 生命周期流（start/finished/deleted）、旁观模式 (live) |
| 安全 | x-api-key 鉴权、Host 白名单、CSP/XFO/nosniff、CSRF 415 |
| 渲染 | markdown 子集（表格/代码块/列表/引用/粗斜体/删除线）、XSS 实体转义、长文本折叠、代码块复制 |
| 流式 | 300ms 节流 mdRender 增量渲染、thinking 推理流 |
| 配置管理 | 全字段表单（6 分组 60+ 字段）、providers 卡片编辑、JSON 编辑器、客户端预校验、保存后回填 |
| 部署 | systemd 单元、ProtectSystem 硬化、自定义 config 路径 |

---

## 二、DSH 具备但 miniagent 缺失的能力（按优先级排序）

### P0 — 高频交互，空白直接影响可用性

| 能力 | DSH 包 | 说明 | 理由 |
|------|--------|------|------|
| **附件/文件上传** | `dsh-client-ui-attachment` | 输入框支持拖拽/粘贴/选择文件，自动读取内容或上传 | 用户最常提的需求：不用先手动 `cat` 或 `read` 文件路径 |
| **工具调用轨迹视图** | `dsh-client-ui-trajectory` | 步骤序号、每次 LLM 调用+工具结果的独立面板，支持折叠/展开、跳转 | 当前工具事件行内显示，长对话难以定位某次工具调用 |
| **工作目录选择器** | `dsh-client-ui-directory-picker-browse` | 目录树浏览/选择，替代当前纯文本输入（易错路径） | 当前 workdir 是 text input，输入痛苦且易错 |
| **会话重命名** | — | 会话列表默认显示 id，不可重命名 | 当前列表只显示 id + 最后 assistant 预览，无法标记重要会话 |

### P1 — 明显提高体验

| 能力 | DSH 包 | 说明 | 理由 |
|------|--------|------|------|
| **会话搜索/过滤** | `dsh-session-query` | 按关键词/日期/模型搜索历史会话 | 会话增多后列表难以浏览 |
| **消息反馈** | `dsh-client-ui-message-feedback` | 每条 assistant 消息旁 thumbs up/down，反馈到后端 | 模型微调/评估数据来源 |
| **命令面板** | `dsh-client-ui-commands`/`dsh-client-ui-input-trigger` | 输入框 `/` 唤起命令（如 `/compact`、`/retry`）、`@` 引用文件 | 减少鼠标操作，提高效率 |
| **模型切换** | `dsh-client-ui-model-selection` | 当前 session 内切换模型（不改 config），后续轮次用新模型 | 当前模型下拉在 composer 行，切模型是全局的 |
| **Token 用量可视化** | — | 每轮/每步 token 消耗、输入/输出占比、预算余量 | 当前 header 只显示累计 `in=N out=M`，无法逐轮分析 |

### P2 — 增强场景

| 能力 | DSH 包 | 说明 | 理由 |
|------|--------|------|------|
| **会话导出/导入** | — | 导出 session 为 JSON/NDJSON、导入恢复对话 | 备份、分享、离线分析 |
| **工作区管理** | `dsh-client-ui-workspace` | 多工作区切换、保存、最近工作区列表 | 当前 workdir 每次手动输入 |
| **后台任务/作业** | `dsh-client-ui-jobs` | 长时间运行的轮次在侧栏显示进度条/状态，可取消 | 当前 running 只有呼吸点，无进度 |
| **目标跟踪** | `dsh-client-ui-goal`/`dsh-command-goal` | 设定目标，agent 步骤向目标对齐，显示完成度 | 长任务场景，用户想知道"进行到哪了" |
| **计划展示** | `dsh-client-ui-plan` | agent 生成计划后可视化展示步骤列表，勾选完成 | 当前 agent 计划以文本输出，无结构化展示 |
| **引用/来源** | `dsh-client-ui-reference` | 文件读取/搜索结果后标注来源，可点击跳转 | 当前工具调用结果嵌入消息流，来源不突出 |

### P3 — 专业/高级

| 能力 | DSH 包 | 说明 | 理由 |
|------|--------|------|------|
| **子代理面板** | `dsh-client-ui-subagent` | 子代理（fork）独立面板，显示其思考过程与结果 | 当前 miniagent 支持 subagent fork 但无 UI |
| **技能管理** | `dsh-client-ui-skill` | 技能（预设 prompt/工具组合）的管理、导入导出 | 当前 tools 全在 config 或代码，无 UI |
| **交付物面板** | `dsh-client-ui-deliverables` | agent 产出的文件列表（写入的文件），预览/下载 | 当前写入的文件只有 LLM 文本提及，无结构化列表 |
| **权限预设** | `dsh-client-ui-permission-presets` | 按场景预设工具权限组（只读/沙箱/全开） | 当前全有或无，无细粒度 |
| **国际化** | `dsh-client-locale` | UI 多语言支持 | 当前全中文，国际化用户需要 |
| **插件管理** | `dsh-client-ui-settings-plugins` | 插件列表/增删/配置 | miniagent 当前无插件系统 |

---

## 三、推荐迭代路线图

### Step 1 (P0) — 文件附件 + 目录选择器
- `#composer` 增加文件拖拽/粘贴/选择按钮
- 读取文件内容作为消息附件（标记来源文件名）
- 目录选择器替换 workdir 文本输入，可选：原生 `<input type="file" webkitdirectory>` 或 server 端目录树 API

### Step 2 (P0) — 工具轨迹视图
- 在会话视图内增加「步骤」面板：每次 LLM 调用 + 工具执行结果作为独立卡片
- 点击展开/折叠，跨步骤时保留上下文
- 后端已有 `step` 字段在 `text_delta`/`tool_use`/`tool_result` 事件中

### Step 3 (P1) — 会话搜索 + 重命名
- 搜索框在侧栏顶部，实时过滤 `GET /api/sessions` 列表
- 会话重命名：`POST /api/sessions/{id}/rename`（后端新增，写入 session meta 或新增 `.meta` 文件）
- 重命名后侧栏显示自定义名称而非 id

### Step 4 (P1) — 模型切换（会话内）
- 当前 model 下拉从全局改为每会话可切换
- 切换后后续轮次用新模型，记录在 session meta 中

### Step 5 (P2) — 会话导出/导入
- 导出：`GET /api/sessions/{id}/export` → NDJSON 下载
- 导入：`POST /api/sessions/import` → multipart upload → 创建新会话

### Step 6 (P3) — 子代理/技能/交付物
- 按需开发，不推荐当前投入

---

## 四、架构兼容性检查

miniagent 当前架构对上述迭代的支撑情况：

| 能力 | 需要的后端改动 | 前端改动量 | 可行性 |
|------|---------------|-----------|--------|
| 文件附件 | 新增 `POST /api/upload`（或 base64 inline） | 中等（attachments.js + 输入框改造） | ✅ 无冲突 |
| 轨迹视图 | 无（step 已在 NDJSON 事件中） | 中等（events.js 按 step 分组） | ✅ 零后端 |
| 目录选择器 | 新增 `GET /api/tree`（目录列举） | 小（dir-picker component） | ✅ |
| 会话搜索 | 无（前端过滤已有列表） | 小（filter input） | ✅ 零后端 |
| 会话重命名 | 新增 `POST /api/sessions/{id}/rename` | 小 | ✅ |
| 会话内模型切换 | 修改 session meta 存储 | 中等 | ⚠️ 需设计切换策略 |
| 会话导出/导入 | 新增导出/导入端点 | 中等 | ✅ |
| 子代理 UI | 新增子代理端点 | 大 | ⚠️ 需评估 |
| 国际化 | 无 | 大（全部 .js 文案提取） | ✅ 独立 |

---

## 五、推荐第一步（Step 1）详细设计

### 能力：文件附件 + 目录选择器

**后端**：
- `POST /api/upload` — 接收 multipart 文件，写入临时目录，返回 `{path, name, size}`，内容作为用户消息附加上下文
- 或更简单：前端读取文件内容（`FileReader`）作为 `data:` URI 或文本内容嵌入用户 prompt

**前端**（`attachment.js` 新文件）：
- 拖拽事件：`#composer` 区 `dragover`/`drop`，提取 `FileList`
- 粘贴事件：`#prompt` 粘贴时检查 `clipboardData.files`（截图、文件）
- 选择按钮：`#composer` 行新增文件选择按钮，`<input type="file" multiple>`
- 附件列表：选中的文件行内显示（文件名 + 大小 + 删除按钮）
- 上传/读取：点击发送时，附件内容作为系统消息或用户消息上下文注入

**安全**：
- 文件大小限制：前端 10MB、后端复用 `max_read_file_bytes`
- 二进制文件跳过（同工具层 `read` 的 NUL 字节检测）

---

## 六、决策清单

| 问题 | 选项 |
|------|------|
| 文件附件是读取内容 inline 还是上传到服务器再引用？ | inline 更简单且零后端（`FileReader.readAsText`）；大文件需上传 |
| 目录选择器是纯前端 `<input webkitdirectory>` 还是后端 `GET /api/tree`？ | 混合：`webkitdirectory` 快速实现（但仅 Chrome），后端 `GET /api/tree` 通用 |
| 工具轨迹视图的展开/折叠状态是否持久化到 localStorage？ | 推荐：localStorage 记忆 per-session 展开状态 |
| 会话重命名存储在哪里？ | 推荐：新增 `.meta` 文件（jsonl 首行已有 meta，但 append-only 不可就地修改），或独立 `meta.json` |