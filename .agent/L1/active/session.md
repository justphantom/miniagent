---
layer: L1
type: session
updated: 2026-08-24
---

# 当前会话

## 状态
- **v6.6.0 已发版**（2026-08-24）：三特性——①配置模态框漂移可见化（编辑基准改为文件内容、diverged/diff 仅字段路径、文件损坏降级运行值+警告横幅）；②设置内重载服务（runServe 重构为重启循环，POST /api/reload，坏文件 400/有 turn 409/重复 503，前端按钮轮询 whoami 穿断连窗口）；③版本号双 v 前缀修复（setVersion 不再拼接）。另：测试文件 300 行约束纳入（3 超限文件拆分）、goimports 统一 import 分组。CHANGELOG 回填 [6.6.0]、version.go v6.6.0。verify-gate 全绿。
- v6.5.0 发版记录见 git 历史（tag `v6.5.0` @ 8c03cb6）。

## 本会话任务
- **WebUI header 显示 workdir**：header 新增 `#session-workdir` span（复用 .session-id 样式，空时 CSS :empty 自动隐藏）；store.js showSessionID 渲染 view.workdir；app.js 两处采集（发送流 session 事件、loadReplay session 事件）；移动端 CSS 与 #session-id 一同隐藏。纯前端 4 文件，不动事件契约。verify-gate 全绿。

## 本会话产出（发版前完善）
- **①CHANGELOG 补漏**：v6.4.0..HEAD 44 提交原仅 6 条，补 4 条——9096515（三特性+UI 重设计+Breaking：OnToolUse/OnToolResult 加 step 首参、新钩子 OnStepUsage、`GET /api/tree`、`step_usage` 事件 web-only）、651711e（IDE 骨架）、1211853（hscroll 修复）、19c76ee（配置目录迁移）。
- **②部署复测（ROOTCAUSE3 DoD）**：服务已运行 HEAD（v6.4.0-44-g4703e7b）；turn NDJSON + `step_usage` 实测正常；**lag-closed WARN 生产实证**（01:15:19 用户浏览器旁观本会话被掐，buf=8565）且**浏览器自动恢复**（用户确认）→ stream_cut 自愈闭环端到端验证通过。环回直接复现失败（tcp_wmem autotune 4MB 吸收全部单轮流量的 ~1MB，单事件 tool_result≤2000 字符凑不够；无 sudo 不能 netns 收缩窗口）——已知限制，`TestWebTurn_LagCutSignalsStreamCut` 单测钉死语义。
- **③R4 mid-data 截断透明化**（cd4b371）：`Response.TruncatedStream`/`Result.Truncated` 领域标记（L0 #8 不进 wire）→ result 事件 `"truncated":true`（omit-if-zero，旧消费方零影响）→ WebUI「· 上游截断」徽标。区分 finish:length（max_tokens 截断）与上游断流。测试：TruncatedChunkPostDelta 断言 TruncatedStream=true；ZeroFieldsPresent 断言 truncated 键 absent。README 契约示例 + CHANGELOG。verify-gate 全绿。

## 方法论沉淀（候选 L2）
- 环回 lag-close 实测方法论：socket SO_RCVBUF 收缩无效（服务端 wmem autotune 独立扩大）；curl --limit-rate 仅节流自身读、内核双端缓冲吸收突发；真正触发需「服务端写阻塞 + 新事件持续到达 >256 channel 容量」。生产实证优于人造复现。
- 事件契约新增字段必须 omit-if-zero（区别于既有 always-present 机器契约键）。

## 待办（发版前）
- ④第四轮复审（ASSESSMENT.md 停在 248b53c 早于 v6.4.0）：重点 stream.go 降级路径、web 前端 IDE 重构、钩子签名变更面。
- ⑤README WebUI 章节：目录选择器/Token 用量/工具轨迹/IDE 布局未入文档。
- ⑥过期文档清理：WEBUI_RESPONSIVE_UX.md 基线 8e517fd 已作废（app.css IDE 体系无 #nav/#overlay，F1-F18 结论失效需重审计）；WEBUI_SESSION_HSCROLL.md 待办陈旧（1211853 已修）；13 个 WEBUI_*/SSE_* 过程文档堆积待归档（需用户决定）。
- 发版时：CHANGELOG [Unreleased] → 版本号定档（含 Breaking，主版本建议 7.0.0），不打 tag 由用户手动执行。
