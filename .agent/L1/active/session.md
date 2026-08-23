---
layer: L1
type: session
updated: 2026-08-23
---

# 当前会话

## 状态
- 已发布 v6.4.0（WebUI 多会话同步 + 上游 SSE 截断重试加固），tag 已 push，工作树干净，verify-gate 全绿。

## 本会话产出
- **事故根因**（用户报告"连接中断"）：上游 `autoapi` 429 TPM 限流失效前掐断 SSE 数据行 → miniagent 解析到截断 chunk `unexpected end of JSON input`（`stream_parse.go:108`）→ `OnLLMError` 只对 `ErrContextLength` 重试，此错误透传 → 整轮中止；同时客户端请求 ctx 断开使 handler 提前返回（`web_turn.go` `<-r.Context().Done()`），缓冲中的 error 行未 flush，前端只见干净 EOF → 落入"连接中断"兜底。
- **修复**：`isTransientStreamError`（`provider/openai/stream.go`）纳入 `unexpected end of JSON input`，预-delta（`deltaSent==0`）透明重试一次；新增 `TestDoStream_TruncatedChunkPreDeltaRetried`。
- **发版**：CHANGELOG `[Unreleased]` → `[6.4.0]`（Added 合并 + Fixed 补本次修复），commit `7c2d895`，tag `v6.4.0` push 后 `make build` 二进制报 `v6.4.0`。`cmd/miniagent/web_live_test.go` 双窗口 result 完整性测试一并提交。
- 事故关联日志：`/opt/llm-proxy/logs/llm-proxy.log`（429/abort）、miniagent journal（`llm call failed step=6`）、会话 `20260823-104331-467079dcd1e88fd9.jsonl`（尾部停在 step5 tool 输出，无最终 assistant/result）。

## 配置管理页面（新增 vNext）
- 后端：`config.SaveConfig`（校验+原子写 0600+O_NOFOLLOW）+ `ValidateConfig` 导出；`web_config.go` GET/PUT /api/config（secret 掩码、占位符保留、校验、need_restart 检测）
- 前端：`config.js` **双模式**——表单模式（6 分组 60+ 字段）+ **providers 卡片编辑**（增删 provider/model/header/thinking-map）+ **JSON 高级编辑器**（textarea 编辑完整配置）；`index.html` ⚙ 配置按钮 + `app.js`/`app.css` 集成；`assets.go` 新增 config.js embed
- 5 commits（ca26d61 / a4aa3e0 / f34f999 / f73e1ce / c33d824），CHANGELOG `[Unreleased]` Added 已更新；verify-gate 全绿
- 59977c4 修复：`renderKv`/`renderKvMap` 渲染时不再 setNested 创建空对象，空条目删字段（omitempty）——打开配置页不改即保存不再污染配置
- e4e78d0 后端：PUT 返回 `config` 字段（掩码后 saved config），前端保存后回填表单；前端：`clientValidate` 预校验（空 providers/重复名/必填缺失），减少无效往返
- 19c76ee 部署：配置目录 `/etc/miniagent` → `/opt/miniagent/config`（服务用户可写）。根因：`ProtectSystem=full` 把 `/etc` 挂只读 + config 目录 root 属主 0755 无目录写权限，SaveConfig 的 temp+rename 需目录写权限故失败。`deploy.sh` 改为 install 目录归服务用户 0750、README 同步
- 899c64f 文档：`WEBUI_NEXT.md` —— 对比 DSH WebUI 功能集，产出迭代路线图（P0 文件附件/轨迹视图/目录选择器/会话重命名 → P1 搜索/反馈/命令/模型切换/用量 → P2 导出/工作区/作业/目标/计划/引用 → P3 子代理/技能/交付物/国际化/插件），附 Step 1 详细设计
- 4339a5f 文档：`WEBUI_IMPLEMENTATION.md` —— 三项特性（目录选择器 GET /api/tree、Token 用量 step_usage 事件+条形图、工具轨迹 step 字段+轨迹面板）详细实现方案。核心共享支点：OnToolUse/OnToolResult 钩子签名加 step int（handleToolCalls 已透传 step，事实已验证）、LoopHooks 新增 OnStepUsage（recordStepUsage 末尾增量直得，避免 OnStep 累计值差值误差）。新文件：web_tree.go、usage.js、trajectory.js、dirpicker.js
- 30ab5d5 文档：`WEBUI_IMPLEMENTATION.md` 定稿 —— 决策清单 9 项全部定案（钩子加 step/OnStepUsage 新钩子/目录树放开只读+加固/轨迹抽屉镜像/预算取 config/最近目录 localStorage/用量双入口/禁内联样式/CSS 变量美化）；新增 §四 UI 美化设计（Token 变量扩展、header 毛玻璃、消息卡片左右对齐、用量条/轨迹抽屉/模态组件规范、动效、无障碍、CSP 合规）与 §4.10 美化实施范围表
- 0fa8ecf 文档：`WEBUI_IMPLEMENTATION.md` 补 §五 响应式布局 —— 参考 DSH（CSS Grid 三栏骨架 + position:fixed 抽屉 + 设计令牌，仅 2 档断点）；三态布局（≥800 三栏侧栏+轨迹/640-799 双栏/<640 单栏 drawer）；侧栏重构 position:fixed 修复遮挡 header、消息区居中+用户消息右对齐、composer 对齐、header 元素逐档隐藏表、轨迹抽屉/模态窄屏全屏、断点注释约定（NARROW/TABLET/DESKTOP）；决策清单扩至 12 项（#10-12 响应式策略/窄屏侧栏/消息对齐）；§八 实施步骤第 2 步扩入响应式骨架
- df028b5 文档：`WEBUI_IMPLEMENTATION.md` 规格化改写 v2.0（可直接编码给 LLM）——逐节规格化：①共享支点给出精确 Go 签名/调用方全量清单/事件 JSON；②特性 1/2/3 每个 API 给出方法/路径/参数/错误码/用例名表，前端给 DOM 模板+函数签名+状态机；③美化从"方向"升级为完整 CSS 规则块（令牌双主题全值、登录页、spinner 分离 #wait/inlineHint、会话列表 running 点位移修复、消息卡 border-left、空态、to-bottom）；④响应式给具体 @media 块与侧栏 display 冲突修复；⑤新增 §0 总览（硬约束/术语）、§7 文件改动清单（精确到函数）、§9 验收 DoD、§10 决策记录（原 12 项+新增 #13 CSSOM 赋值合法/#14 提示分离/#15 轨迹定位）

## 待办
- 无（功能主体完成；遗留为后续迭代项）。