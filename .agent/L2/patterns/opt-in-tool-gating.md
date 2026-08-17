---
layer: L2
type: pattern
tags: [tools, opt-in, config-gating, registration, network, ssrf]
created: 2026-08-17
confidence: high
---

# opt-in 工具注册门：config 开关 + buildTools 条件 append

## 模式
能力超出"开发闭环默认安全面"的工具（出网、任意执行）不默认注册，经 config 布尔键 opt-in 后 `buildTools` 条件 append。误调经 dispatch 报 `unknown tool`，而非运行期拒——**注册门替代运行期过滤**。

## 决策矩阵（miniagent 实测）
| 工具 | 门控 | 理由 |
|---|---|---|
| shell | mode==auto（非 config 键） | 任意命令执行，auto 模式语义即"无约束" |
| web | `run.web_fetch`（bool，默认 false） | 任意 URL 出网，SSRF 防护是 best-effort 非安全边界 |

## 关键约束
1. **config 键纯 passthrough**：`RunConfig` 加字段，`resolve.go` 不仲裁（非三态 `*T`，因无 CLI flag 覆盖需求）；`buildTools` 收 `bool` 形参，`main.go` 经 `intoBool` 注入。
2. **注册门 ≠ 安全边界**：opt-in 后工具在全模式生效，防护靠工具内部（如 web 的 SSRF 检查）+ 调用方网络层。门控只控"是否可达"，不控"可达后做什么"。
3. **测试形参传染**：`buildTools` 加 bool 形参 → 所有调用点（含测试）须同步加 `false`，vet 立即报错兜底。
4. **文档计数加注**：工具数描述须标 "opt-in" 修饰，否则与默认注册数漂移（memory-freshness 规则）。

## 参考
- `cmd/miniagent/tools.go buildTools`（webFetch 形参 + 条件 append）
- `internal/miniagent/config/config.go RunConfig.WebFetch`、`resolve.go`（passthrough）
- 关联 `default-mode-not-security-boundary.md`（攻击面记账第 9 条 web 出网）
