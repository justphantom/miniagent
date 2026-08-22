# 安全策略

## 安全模型

miniagent 执行 LLM 产出的命令与代码，**默认不构成安全边界**——这是设计取舍，非缺陷：

- v5.0.0 起已删除 `-mode`/`confineWrap`/白名单工具/`-sandbox` 等所有进程内软约束（write/edit 无路径限制、shell 恒注册无过滤）。
- `miniagent has no mode since 5.0.0`（`config/config.go:109` 注释）。隔离责任完全在调用方：容器、沙箱、cgroup 或专用低权限 UID。
- agent 层零安全保障：任一命令可由 LLM 经 shell 工具执行（工具的本质）；文件工具可读写运行用户有权限的任何路径。

其他硬化见 [ARCHITECTURE.md §10](./ARCHITECTURE.md)：请求/响应体大小上限防 OOM/烧钱；HTTP 重定向跨域剥离 `Authorization`；insecure URL 警告；session 文件 `flock` + `O_NOFOLLOW` + `0o600`/`0o700`；shell 超时 + 进程组 `SIGKILL` 清理。

## 上报漏洞

请勿在公开 issue 披露安全漏洞。通过 GitHub Security Advisory（私有漏洞上报，Security 标签页 → Report a vulnerability）提交，或直接联系维护者。我们会在合理时限内确认并响应。

上报时请附：复现步骤、影响范围、受影响版本。

## 不属于漏洞（设计已知）

- LLM 经 shell 工具执行任意命令——工具的本质；已有超时 + 进程组清理，真正隔离依赖调用方。
- 环境变量 `MINIAGENT_API_KEY` 经 config 注入后被 shell 工具读取——机密应通过隔离环境而非进程内剥离保护（`scrubEnv` 仅 best-effort 降低概率，非边界）。
- 文件工具（write/edit/read）可访问运行用户有权限的任何路径——单模式零约束，隔离靠 OS 层（同上）。
