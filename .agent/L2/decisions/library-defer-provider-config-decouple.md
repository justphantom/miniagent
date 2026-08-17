---
layer: L2
type: decision
tags: [architecture, library, provider, config, coupling]
created: 2026-08-17
updated: 2026-08-17
confidence: high
---

# 库化暂缓 + provider→config 解耦改善（P1/P2）

## 背景
评估库化可行性：架构就绪度高（零第三方依赖、单向无环、core 纯净、LoopHooks 缝即库 API），但 L2 既定决策库化缓至 5.0.0。本轮先做模块解耦评估并执行低成本清障。

## 解耦评估结论（B+）
- ✅ 无环、单向、core→子包零依赖，L0 #14 全满足。
- ⚠️ `provider/openai|anthropic → config` 是最大耦合点：
  1. `config.ValidateURL` 是纯 URL 校验，语义不属于 config（provider 实现被迫拖走 CLI 配置层）。
  2. `openai.ListAllModels` 签名吃 `[]config.ProviderConfig`，把 provider 实现与配置层焊死。
- ⚠️ `event → session`：NDJSON 序列化共享契约（sessionEventType 同值），轻度，留待库化时处理。
- ✅ `compaction → policy`（EstimateTokens）合理；`compaction → session` 仅压缩屏障 Kind 判断，可留。

## 决策
1. **库化暂缓**，维持 5.0.0 breaking 槽位；此前不动 internal API 承诺。
2. **P1**：`ValidateURL` 挪出 config → provider 共享层（消除 provider→config 的工具依赖）。
3. **P2**：`ListAllModels` 去 config 化——签名改吃中间结构（name/chat_url/models_url/kind/key），config 层做映射；provider 包不再 import config。
4. **明确不做**：拆 EstimateTokens、动 event→session、动 compaction→session（收益 < 改动面）。

## 理由
- P1/P2 是库化的唯一实质障碍（provider 拖 config）；提前消除，5.0.0 时 provider 包可整体外迁零改动。
- 中间结构是纯数据投影，非抽象（节制抽象对齐）；config→provider 方向本来就存在（cmd 装配层），不新增环。

## 参考
- `internal/miniagent/config/url.go`（ValidateURL 原址）
- `internal/provider/openai/models.go`（ListAllModels 签名）
- 关联 `core-zero-policy-loophooks-decoupling`（库化槽位 5.0.0 出处）
