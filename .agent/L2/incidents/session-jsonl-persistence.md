---
layer: L2
type: incident
tags: [session, persistence, flock, atomic-write, concurrency, crash-recovery]
created: 2026-08-09
confidence: high
---

# session jsonl 持久化可靠性组

## 现象
append-only jsonl 落盘在崩溃 / 并发下易半写腐败。

## 背景
`miniagent/session/session.go` jsonl 首行 `type=session` metadata（model / workdir / provider / created），余 `type=message`；stdout 首条 NDJSON 即 session 事件供消费方捕获 id。

## 做法
1. `AppendMessages` 正常轮追加 `result.NewMessages`，失败轮不固化。
2. **flock 跨进程互斥**：`lock_unix.go`（`LOCK_EX|LOCK_NB` + 5s 短轮询）/ `lock_windows.go`（`LockFileEx` 字节区间锁）。
3. `info.Size()>maxSessionBytes`(50MiB) 前置拒绝 + 预序列化按 size+待写超限拒绝，防「写成功延后失败」永久卡死。
4. **`ensureTrailingNewline` 写前回扫截断崩溃半行残行**（关键修复）——否则 `O_APPEND` 盲写把新消息拼到无换行残行、下次 `LoadSession` 反噬为中段损坏，永久丢会话。
5. `RewriteMessages` 仅 `result.Compacted` 时全量重写（临时文件 → Write → Sync → `os.Rename` → 父目录 Sync 原子交换）。
6. 写盘期临时忽略 SIGINT/SIGTERM 防半写截断。

## 根因
append-only 崩溃只污染尾行。

## 容错
`LoadSession` 尾行半写容忍丢弃；中间损坏严格报错退出（不静默丢历史）。会话 id 随机段 8 字节(64bit) + rand 失败回落时间戳 + pid。

## 可复用经验
1. **append-only 崩溃只污染尾行**：写前回扫截尾 + 读侧尾行容忍，比「全程原子写」成本低。
2. **flock + size 前置拒绝** 防跨进程竞争与「写成功延后失败」卡死。
3. **写盘期忽略信号** 是防半写的最后一道闸。

## 参考
- `miniagent/session/session.go`、`lock_unix.go`、`lock_windows.go`、`validate.go`
- commit `00038cf`
