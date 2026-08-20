---
layer: L2
type: pattern
tags: [tools, rtk, proxy, optional-dep, once-value, fallback, external-binary]
created: 2026-08-15
updated: 2026-08-20
confidence: high
status: superseded
---

> **v5.0.0 已废弃**：rtk 代理 + git/go/npm/lint 白名单工具全删。所有外部命令统一走 `shell` 工具。此模式保留供参考，不再适用于本仓库。

# 可选外部代理（rtk）接入模式：探测 + 按子命令代理 + 零行为回退

## 模式
工具调用外部二进制时，若存在输出紧凑代理（rtk）可先经代理再回退原生。代理是可选依赖——未部署时行为零变化。

## 做法
1. **进程内只探测一次**：`sync.OnceValue(func() string { exec.LookPath("rtk") })` 缓存路径，热路径不重复 exec。
2. **按子命令决定代理范围**：代理只覆盖它支持的子集，不能盲目全代理。每个工具自持 `rtkXxxSubcommands` map：
   ```go
   var rtkGoSubcommands = map[string]bool{"build": true, "test": true, "vet": true}
   ```
3. **单点包装函数**：`rtkWrap(bin, prefix, args) → (bin, args)`，rtk 在则返 `("rtk", prefix+args)`，不在则原样返回。调用方一行替换，无分支：
   ```go
   bin, argv := rtkWrap("go", []string{"go", sub}, cmdArgs)
   cmd := exec.CommandContext(ctx, bin, argv...)
   ```
4. **零行为回退**：rtk 不存在时 `rtkWrap` 返回原生 bin + 原生 args，exec 路径与接入前完全一致。
5. **测试兼容**：rtk 输出格式与原生不同（如 git status → `* main...origin/main`），断言用内容关键词（`branch`/`commits`）不按精确格式串。`rtk --help` 先摸清每个子命令的覆盖范围。

## 何时用
- 外部二进制输出量大（go test、npm install）→ rtk 紧凑输出省 token
- 代理覆盖子集（非全量）→ 按子命令 map 精确路由
- 可选依赖（不强制部署）→ `OnceValue` + `rtkWrap` 零行为回退

## 参考
- commit `626819a`（rtk 代理引入）
