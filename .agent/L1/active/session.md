---
layer: L1
type: session
updated: 2026-08-21
---

# 当前会话

## 状态
三轮待提交改动：
1. 发版完善（config.example web 段/CHANGELOG [5.1.0] 回填/release-checklist/README/webui-architecture.md）
2. systemd 部署（deploy/ + Makefile deploy + README 章节）
3. 统一 UA miniagent/{version}：miniagent/version.go 单源（Version+UserAgent()），provider 三请求点（chat/models/stream）+ web 工具统一 UA；Makefile -X 注入 miniagent.Version；main.version 改别名 miniagent.Version；Headers 允许覆盖 UA（网关兼容）。测试：tools TestWeb_UserAgent、openai TestUserAgent_AllRequests。verify-gate 全绿。

## 待办
- 用户审阅后提交。发版：git tag -a v6.1.0 + make build + push。

## 备注
- verify-gate 全绿；非 _test.go Go 文件均 ≤300 行。
