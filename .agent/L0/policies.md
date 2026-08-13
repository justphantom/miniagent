---
layer: L0
version: 1
updated: 2026-08-13
---

# 流程策略

1. **verify-gate**：编码标准见根目录 `AGENTS.md`（gofmt 空 / build / vet / test -race / lint 五步，缺一不可、`-race` 必跑），本层不重复。
2. **config-first**：config 必须存在（v4.2.0 删裸模式）；裁决优先级 `cli > config > builtin`。新能力默认 config 化，CLI flag 仅便捷覆盖；参数膨胀优先扩 config 而非加 flag。
3. **钩子红线**：`LoopHooks` / `CompactingHook` 调用点无核心 recover——自定义 / 第三方钩子须自行 `defer recover()` 或恒不 panic；钩子经返回值（`StepOutput` / `CompactingOutput`）表达意图，禁直接改入参 `msgs` 元素（直接改不生效）。
4. **CHANGELOG 纪律**：改 API / 配置 / CLI 行为后同步更新 `CHANGELOG.md`（Keep a Changelog + SemVer）；breaking 单列并附迁移说明。
5. **改动落点检查**：改 compaction 逻辑查 `internal/miniagent/compaction` 及调用方；改 session 查 `internal/miniagent/session`；改 NDJSON 事件契约须评估外部消费方。
6. 不确定实现路径时，先给方案与利弊，待用户确认再写代码。
7. **记忆闭环**：每轮交互后更新 `.agent/L1/active/session.md`（L1 单文件）；任务结束评估是否沉淀到 `L2/`。
