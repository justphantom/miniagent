# 贡献指南

感谢贡献 miniagent。动手前先读 [AGENTS.md](./AGENTS.md)（最高约束）与 [ARCHITECTURE.md](./ARCHITECTURE.md)（架构总览）。

## 开发流程

1. fork → 分支 → 改 → 提交。提交规范：subject ≤72 字符、祈使、无句号、一次一事。
2. 改后必跑 verify-gate 全绿（缺一不可）：

   ```bash
   gofmt -s -l .          # 输出为空（已格式化）
   go build ./...
   go vet ./...
   go test -race ./...
   golangci-lint run ./...
   ```

3. 测试用标准库 `testing`（不引 testify），`-race` 必跑。用例即需求文档——覆盖 error 契约、不变量、幂等、并发安全。

## 工程约束（AGENTS.md 摘要）

- 标准库优先；引入第三方依赖需说明理由及最小用法。
- 节制抽象：函数单一职责，不预建接口/基类/工厂，重复 <3 处不抽。
- 单个代码文件 ≤300 行（`_test.go` 豁免：测试按场景聚合，允许超）。
- 注释只写「为什么」，且仅非直观或特殊约定时。
- 错误直接返回标准库类型，不自定义（除非语义不足）。
- 纳入版本跟踪的文件中不可引用未纳入版本跟踪文件的任何内容。

## 扩展点

- **provider**：实现 `LLM` 接口（`Do` 非流式 + `DoStream` 流式），见 `internal/miniagent/provider_api.go`；OpenAI 兼容实现见 `internal/provider/openai/`。
- **钩子**：经 `LoopHooks` 外挂上下文管理/用量/成型/失败恢复策略，契约见 [HOOKS.md](./HOOKS.md)。
- **工具**：`Tool.Call` 函数字段，由 `cfg.Tools` 自由组装，内置工具见 `internal/miniagent/tools/tool_*.go`（read/write/edit/grep/glob/shell）。

核心库化（移出 `internal/` 开放外部导入）计划于 5.0.0；当前 `internal/` 下代码 Go 禁止外部模块导入。
