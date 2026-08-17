---
layer: L2
type: pattern
tags: [tools, registration-gating, config-gating, mode-gating, network, ssrf]
created: 2026-08-17
updated: 2026-08-17
confidence: high
---

# 工具注册门：mode 门 / config 门 / 无门 的选型

## 模式
工具超出"开发闭环默认面"时用**注册门**（不注册 → dispatch 报 `unknown tool`），而非运行期过滤。三种门控选型：

| 门控 | 形态 | 适用 |
|---|---|---|
| mode 门 | `mode == ModeAuto` 才 append | 能力即"无约束模式"语义（shell：任意执行） |
| config 门 | `run.<key> *bool` opt-in | 能力风险高但合法场景明确（保留给未来工具） |
| 无门 | 始终注册 | 风险可用内置防护收敛到 guardrail 水位（web：SSRF 检查） |

## miniagent 决策矩阵（2026-08-17 定型）
- `shell`：mode 门（auto-only）——任意命令执行无 guardrail 可言。
- `web`：**无门**（default+auto 均默认注册）——SSRF 防护（私网/环回/链路本地/组播/v4-mapped 拒 + 重定向每跳重查）+ 仅 GET + content-type 白名单 + 双截断，把风险收敛到 guardrail 水位；**曾用 config 门（`run.web_fetch`，commit 913a6b5），当日即撤**（4d7d78d 后）——默认不可用让多数用户永远发现不了该能力，而防护已内置，门控收益 < 可发现性损失。

## 教训（config 门 → 撤门）
1. **opt-in 键默认关 = 能力隐形**：用户不读文档就永远不知道工具存在；工具本身有防护时，门控是多余的摩擦。
2. **门控的真正适用点**是"防护不可内置"的能力（任意执行类），不是"有防护但有残余风险"的能力（后者攻击面记账 + guardrail 定位即可）。
3. 撤门是 breaking（工具清单变化）：须同步工具计数文档五处 + .agent 记忆三处（memory-freshness）+ config 键删除（残留键被忽略但示例文件须清）。

## 参考
- `cmd/miniagent/tools.go buildTools`（mode 门 + 无条件 append 并存）
- L2 `default-mode-not-security-boundary.md` 攻击面记账第 9 条（web 出网残余）
- 反例存档：commit `913a6b5`（web 以 opt-in 引入）→ 后续撤门；keep the lesson, not the flag
