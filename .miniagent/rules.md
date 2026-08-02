## 硬约束

- 标准库优先，零三方依赖（go.mod 仅 `go 1.25`，无 require）。
- 非测试单文件 ≤300 行，超出即拆。
- 注释只写「为什么」，且仅非直观/特殊约定处写。
- 节制抽象：函数单一职责，不预建接口/基类/工厂，重复 <3 处不抽。
- 错误直接返回标准库类型，不自定义错误结构（除非语义不足）。
- 每个功能必须有标准库测试——用例即需求文档。
- 所有二进制只存 `bin/`（已 gitignore）。
- 仅做明确要求；不确定就问，不替用户决定
- 改后必跑全绿：`gofmt -s -l .`（空）/ `go build ./...` / `go vet ./...` / `go test -race ./...` / `golangci-lint run ./...`。

## 模块地图（v3.2 重构后）

- `internal/miniagent/loop.go`+`loop_tools.go`+`loop_extra.go`：ReAct 主循环（Run / handleToolCalls / runToolsParallel / callLLM*）。`callLLM` 符号被 thinking_test.go 直调，不可删。
- `internal/miniagent/context.go`：`FitHistory`+`ContextBudget` 单一上下文预算入口（合并自 compaction.go+history.go）；`summarizeMiddle` 经 `budget.Summarize` 回调解耦 client。
- `internal/miniagent/client.go`+`client_util.go`：`ChatClient`（非流式 Do/ListModels，带总 Timeout）。
- `internal/miniagent/stream.go`+`stream_parse.go`：`StreamClient`（DoStream，无 Timeout）+ SSE 解析。
- `internal/miniagent/wire.go`：OpenAI schema 序列化层（buildChatBody/parseChatResponse），跨供应商 reasoning 兼容（DeepSeek `reasoning_content` / OpenAI `reasoning`）。
- `internal/miniagent/config.go`+`resolve.go`：config 结构 + cli>config>builtin 裁决（cfg 必非 nil）。
- `internal/miniagent/session.go`：jsonl append-only 会话。
- `internal/miniagent/tool_*.go`：read/write/edit(含 edits)/grep/glob/shell(tool_shell.go) + tool_script.go(script_* 工具) + memory.go(.miniagent/memory.jsonl)。
- `cmd/miniagent/`：main.go(编排)/setup.go(requireConfig+buildLLM)/tools.go(工具注册)/project.go(.miniagent 规则发现)/sandbox.go(confineWrap)。

## 自进化

- 本目录（`.miniagent/`）即项目专属配置：persona.md（身份基线）+ rules.md（本文件）+ scripts.json（script_* 工具）+ memory.jsonl（项目记忆）。
- `read`/`write` 的保留路径 `path="memory"` 路由到 `.miniagent/memory.jsonl`：read 渲染、write 追加 `{type:"note",content}`。
