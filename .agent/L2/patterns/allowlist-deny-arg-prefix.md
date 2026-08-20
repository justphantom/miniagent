---
layer: L2
type: pattern
tags: [tools, allowlist, deny, arg-prefix, optSpec, flag-matching, guardrail]
created: 2026-08-15
updated: 2026-08-20
confidence: high
status: superseded
---

> **v5.0.0 已废弃**：子命令白名单工具（git/go/npm/golangci-lint）及 optSpec 匹配器全删。所有外部命令统一走 `shell` 工具。此模式保留供参考，不再适用于本仓库。

# 子命令白名单 + 参数拒绝模式（allow-list 之上 deny 险参）

## 模式
default 模式（无 shell）下外部命令经白名单工具（git/go/npm/golangci-lint）暴露：**子命令白名单**（只放开发必需）+ **参数级拒绝**（allow-list 内子命令也拒危险参数）。当前子命令集与 deny 清单是**易变实现细节**，见 L2 `default-mode-dev-tools-allowlist.md`（决策与现行清单）与 `default-mode-not-security-boundary.md`（攻击面记账）；本条只沉淀**不随版本变化的匹配器教训**。

## 匹配器演进（前缀 → optSpec）
- **v1 `strings.Fields` + `HasPrefix` 前缀匹配**（2026-08-15）：快速失效，系统性绕过（见下）。
- **v2 `optSpec` 归一化匹配器**（2026-08-17，`miniagent/tools/flagmatch.go`）：短旗标全等+等号粘合、单破折长名全等、双破折唯一前缀缩写识别。修复：`push -f`/`-d`（--force/--delete 最高频短形）、`commit --am`（≡`--amend` 改写 HEAD）、`npm --C` 双横线、`--registry=` 单横线等全族绕过。

## 核心教训
1. **前缀匹配（HasPrefix）对旗标是错误抽象**：`-m` 前缀拒不掉 `-mmsg` 粘合与 `--am` 缩写，且误杀 `commit -am`；必须归一化到 optSpec（名/形/粘合值）再判。
2. **单双横线等价**：go flag 包 `--o` ≡ `-o`，npm `--C` ≡ `-C`——deny 表两形都要列，或匹配器归一化处理。
3. **短旗标簇**：`-qf` 内含被拒布尔短旗即拒（无 `=` 的多字母单破折 token）。
4. **误杀检查与漏洞对称**：每次收紧后跑合法高频写法（`commit -am`、`-m"msg"`、`go test -work`）确认不误杀；guardrail 拒绝语义「保守方向：误报代价一次手动编辑，漏报是代码执行」。
5. **拒绝信息可自诊**：deny 文案列出「为什么拒 + 合法替代」（如「use the git tool instead」），模型能据此改道而非盲试。

## 关联
- `default-mode-dev-tools-allowlist.md`（L2/decisions，已 superseded）——历史决策依据
- `default-mode-not-security-boundary.md`（L2/decisions，已 superseded）——攻击面记账
- `optional-proxy-rtk-integration.md`（L2/patterns，已 superseded）——rtk 代理层

## 参考
- CHANGELOG v4.7.0 Fixed「git 短旗标/长选项缩写绕过」等条目（实测复现记录）
