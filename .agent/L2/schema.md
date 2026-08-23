---
layer: L2
type: reference
tags: [schema, frontmatter, tags, memory, index]
created: 2026-08-24
---

# L2 条目 Schema 参考

本文件约束所有 L2 条目的 YAML frontmatter 字段，保证索引精度与自动校验可执行。

## 必填字段

| 字段 | 取值 | 说明 |
|------|------|------|
| `layer` | `L2` | 恒为 L2 |
| `type` | `pattern` / `decision` / `incident` | 决定存放子目录 |
| `tags` | YAML 数组 `[a, b, c]` | 精确关键词，禁自由文本逗号分隔 |
| `created` | `YYYY-MM-DD` | 首次创建 |
| `confidence` | `high` / `medium` / `low` / `evolving` | 可信度，见下 |

## 可选字段

| 字段 | 取值 | 说明 |
|------|------|------|
| `updated` | `YYYY-MM-DD` | 内容变更时更新 |
| `status` | `superseded` | 条目已过期，索引优先级降级 |
| `superseded_by` | 条目名 | 指向替代条目 |

## tags 约定

- 全部小写，连字符分隔（`kebab-case`），如 `context-window`。
- 每个 tag 单个词（禁空格）。
- 覆盖主题、实现对象、关键机制三个维度。
- 可引用代码对象名（如 `loop.go`、`loop_api.go`）。

## confidence 语义

| 值 | 含义 | 处置 |
|----|------|------|
| `high` | 已多次验证，可信 | 直接采用 |
| `medium` | 单次验证或推断 | 采用时向用户提示置信度 |
| `low` | 推断，未验证 | 呈现候选 + 请求确认 |
| `evolving` | 新条目待验证 | 一次采用后更新为 high/medium/low |

## 生命周期

1. 新建默认 `confidence: evolving`。
2. 首次检索采用后，更新为实际验证置信度。
3. 内容过期 → 更新条目并记 `updated`；被替代 → 标 `status: superseded` + `superseded_by`。
4. verify-gate 校验：frontmatter 必填字段齐全、tags 为 YAML 数组、superseded 条目代码引用有效。