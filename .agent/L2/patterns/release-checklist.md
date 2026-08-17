---
layer: L2
type: pattern
tags: [release, checklist, docs-sync, versioning, workflow]
created: 2026-08-17
confidence: high
---

# 发版检查单（v4.7.0 实测沉淀）

## 适用场景
若干 commit 积压未发版、或含新工具/新配置面变更，准备定版 tag。

## 1. 发版前评估（半小时内可完成）
- 基线核验：工作区 clean、与 origin 同步、`git describe --tags` 看积压量、verify-gate 全绿（build/vet/test -race/lint）。
- CHANGELOG `[Unreleased]` 存量盘点：**同 section 多段交替出现 = 多轮修补未合并**，定版前必须同类合并（Added/Changed/Removed/Fixed 各一段）。
- 版本号定级看两个先例：
  - v4.4.0 / v4.7.0：**工具面/CLI 行为 breaking 归 minor + 显式迁移说明**（工具是交互契约，非库 API；库 API breaking 才 major）。
  - `core-zero-policy-loophooks-decoupling`：库化（internal/ 外迁）预留 5.0.0。

## 2. 新工具接入的文档同步（v4.7.0 踩过：`ast` 工具全缺）
新增内置工具后必查五处，漏一处即文档与注册表漂移：
1. README 特性行（工具列举 + 计数）
2. README「工具清单」段首（计数 + confine 说明）+ 对应 `### <tool>` 条目
3. ARCHITECTURE 目录注释（tools/ 文件列举）
4. ARCHITECTURE「工具系统」表（每工具一行）+ default 模式计数
5. CHANGELOG `[Unreleased]` Added

## 3. 发版动作序列
```
commit 文档定版 → git tag -a vX.Y.Z -m "..." → push main → push tag → make build
```
- tag 后必须重编：`Makefile` 的 version 注入用 `git describe`，tag 前后 describe 不同。
- `bin/` 不入版本跟踪；**勿 `go build -o 根目录`**（会在仓库根落裸二进制，违反 bin-only 约定）。

## 4. 记忆闭环（同任务内）
- 改工具/防线后同步 `.agent/` 相关计数（见 `memory-freshness-pointer-over-count`）。
- session.md 清历史流水账，有沉淀价值的进 L2。
- index.md 登记 + `.agent/README.md` 约定兜底。

## 参考
- commits `6788942`（v4.7.0 文档定版）、`f2aa79d`（.agent 精炼）；tag `v4.7.0`
- `producer-contract-change-ripple`——文档同步本质是其 checklist 的发版特化
