---
layer: meta
updated: 2026-08-24
---

# .agent 记忆索引

L0（永久约束）每次会话加载；L1（过程上下文）仅 `active/session.md` + `active/carryover.md`；L2（经验教训）按需查 `L2/README.md`、`L2/schema.md`。检索反馈闭环见 `AGENTS.md` 路由节。

## L2 Schema
- `schema.md` — L2 条目 YAML frontmatter 必填/可选字段定义、tags 约定、confidence 语义、生命周期

## L2/decisions（架构决策 ADR）

### 有效
- `core-zero-policy-loophooks-decoupling` — 核心零策略 + LoopHooks 外挂 + 子包化；库化缓至 5.0.0
- `system-prompt-config-only` — system prompt 收口 config-only；opt-in `rules_file`
- `library-defer-provider-config-decouple` — 库化暂缓；provider 包与 config 解耦（P1/P2 已做）
- `compaction-review-fence-and-constants` — 压缩审查结论：代码围栏判定收紧（P2-1）+ 估算常量单源守护（P3-2）
- `webui-architecture` — WebUI 前端架构（ES Module 零构建 / 多会话同步 / 流式 Markdown 渲染）

### 已废弃（历史档案，勿作现行依据）
- `multi-provider-kind-dispatch` — 多 provider Kind 分派（anthropic 已删）
- `default-mode-not-security-boundary` — default 模式防线总账（v5.0.0 全删）
- `default-mode-dev-tools-allowlist` — git/go/npm 白名单子命令（v5.0.0 已删）

## L2/patterns（可复用模式）

### 有效
- `config-tri-state-resolve` — config 三态裁决（新增 config 键/缺省值优先级时查）+ 隐性 footgun（tool_output_dir/session.dir/pickMPG）
- `compaction-system` — 压缩体系：预算自适应 CW + 7 阶裁剪 + reasoning 截断（改压缩逻辑时查）
- `memory-freshness-pointer-over-count` — 记忆反过期：数量引用用指针不硬编码（设计记忆类机制时查）
- `producer-contract-change-ripple` — 改动涟漪：改/删 X 必 grep 代码+注释+测试+文档（改共享结构体/NDJSON 事件时查）
- `adversarial-workflow-review` — 对抗式 workflow 评审（finder × verify）（大改动评审时查）
- `opt-in-tool-gating` — 工具注册门选型（mode/config/无门；web 撤门教训：防护内置则门控多余）（加新工具默认开关时查）
- `release-checklist` — 发版检查单：版本定级先例/新工具五处文档同步/动作序列
- `webui-ux-audit-baseline` — WebUI UX 审计基线（IDE 骨架 576989f 起）：布局大重构后旧 file:line 审计作废须重写

### 已废弃（历史档案，勿作现行依据）
- `allowlist-deny-arg-prefix` — 子命令白名单 + 参数拒绝的抽象模式（v5.0.0 已删工具）
- `optional-proxy-rtk-integration` — 可选外部代理（rtk）探测+回退模式（v5.0.0 已删）

## L2/incidents（事故复盘）

### 有效
- `session-jsonl-persistence` — jsonl 持久化可靠性（flock/原子写/尾行容忍）（改 session 落盘时查）
- `streaming-sse-robustness` — SSE 中断检测/幂等重试/封顶（改流式处理时查）
- `thinking-pindown-downgrade-length` — thinking 钉死+降级链+length 空回复（改 thinking 协议时查）
- `hooks-no-recover-shape-result-contract` — 钩子红线与 ShapeToolResult 契约（写自定义钩子时查）
- `corrupted-summary-prompt-injection` — 损坏摘要注入（v4.3.0 已修，留设计依据）（改摘要生成时查）
- `compaction-headadj-override-stale-clause` — jointTailBudget override 误扣（消费者未同步）（改压缩预算字段时查消费者）
- `tools-rewrite-lost-logic` — 文件重写丢逻辑 + 测试截断教训（改 write/edit 工具时查）

### 已废弃（历史档案，勿作现行依据）
- `anthropic-provider-copy-asymmetry` — 跨 provider 复制对称清单（5 bug 复盘）（anthropic 已删）
