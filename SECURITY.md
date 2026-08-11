# 安全策略

## 安全模型

miniagent 执行 LLM 产出的命令与代码，**默认不构成安全边界**——这是设计取舍，非缺陷：

- **`default` 模式**是薄软约束：写工具（write/edit）限 workdir 子树、shell 拒 sudo/su。但 shell 可经 `cd`/绝对路径越界，写工具可经符号链接逃逸 workdir。
- **`auto` 模式**无任何约束。
- **隔离责任在调用方**：沙箱、容器、cgroup 或专用低权限账号。default 模式的越界是已知行为，不视为漏洞。

其他硬化见 [ARCHITECTURE.md §10](./ARCHITECTURE.md)：请求/响应体大小上限防 OOM/烧钱；HTTP 重定向跨域剥离 `Authorization`；insecure URL 警告；session 文件 `flock` + `O_NOFOLLOW` + `0o600`/`0o700`；shell 超时 + 进程组 `SIGKILL` 清理。

## 上报漏洞

请勿在公开 issue 披露安全漏洞。通过 GitHub Security Advisory（私有漏洞上报，Security 标签页 → Report a vulnerability）提交，或直接联系维护者。我们会在合理时限内确认并响应。

上报时请附：复现步骤、影响范围、受影响版本。

## 不属于漏洞（设计已知）

- `default` 模式下 shell 经 `cd`/绝对路径访问 workdir 外路径——README/ARCHITECTURE 明示，约束非安全边界。
- LLM 经 shell 工具执行任意命令——工具的本质；已有超时 + 进程组清理，真正隔离依赖调用方。
- 环境变量 `MINIAGENT_API_KEY` 经 config 注入后被 shell 工具读取——机密应通过隔离环境而非进程内剥离保护（`scrubEnv` 仅 best-effort 降低概率，非边界）。
