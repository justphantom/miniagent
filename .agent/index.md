---
layer: meta
updated: 2026-08-17
---

# .agent 记忆索引

L0（永久约束）每次会话加载；L1（过程上下文）仅 `active/session.md`；L2（经验教训）按需查 `L2/README.md`。检索优先精确关键词。

## L2/decisions（架构决策 ADR）

- `core-zero-policy-loophooks-decoupling` — 核心零策略 + LoopHooks 外挂 + 子包化；库化缓至 5.0.0
- `multi-provider-kind-dispatch` — 多 provider Kind 字符串分派 + wire 边界有损投影（anthropic 接入）
- `system-prompt-config-only` — system prompt 收口 config-only；opt-in `rules_file`
- `default-mode-not-security-boundary` — default 模式具体防线总账（.git 封锁/参数级收紧/攻击面记账；L0 #13 指向此）
- `default-mode-dev-tools-allowlist` — git/go/npm/lint 白名单子命令决策 + rtk 代理
- `library-defer-provider-config-decouple` — 库化暂缓；provider 包与 config 解耦（P1/P2 已做）

## L2/patterns（可复用模式）

- `compaction-system` — 压缩体系：预算自适应 CW + 7 阶裁剪 + reasoning 截断
- `config-tri-state-resolve` — config 三态裁决 + 隐性 footgun（tool_output_dir/session.dir/pickMPG）
- `memory-freshness-pointer-over-count` — 记忆反过期规则：数量引用用指针不硬编码
- `producer-contract-change-ripple` — 改动涟漪：改/删 X 必 grep 代码+注释+测试+文档
- `adversarial-workflow-review` — 对抗式 workflow 评审（finder × verify）
- `allowlist-deny-arg-prefix` — 子命令白名单 + 参数拒绝的抽象模式（optSpec 归一匹配）
- `optional-proxy-rtk-integration` — 可选外部代理（rtk）探测+回退模式

## L2/incidents（事故复盘）

- `session-jsonl-persistence` — jsonl 持久化可靠性（flock/原子写/尾行容忍）
- `streaming-sse-robustness` — SSE 中断检测/幂等重试/封顶
- `thinking-pindown-downgrade-length` — thinking 钉死+降级链+length 空回复
- `hooks-no-recover-shape-result-contract` — 钩子红线与 ShapeToolResult 契约
- `corrupted-summary-prompt-injection` — 损坏摘要注入（v4.3.0 已修，留设计依据）
- `compaction-headadj-override-stale-clause` — jointTailBudget override 误扣（消费者未同步）
- `anthropic-provider-copy-asymmetry` — 跨 provider 复制对称清单（5 bug 复盘）
- `tools-rewrite-lost-logic` — 文件重写丢逻辑 + 测试截断教训
