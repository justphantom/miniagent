# 「连接中断：流意外结束」根因分析（第三轮：订阅者 lag-close）

基线：v6.4.0-42-g08b981e（P0 已部署：cancel 解耦 + stop 终态事件）。方法：journal × session jsonl × 代码路径消除法。结论：**前两轮修复均有效且仍在生效，但漏了第三个也是最高频的根因——事件总线把"落后订阅者"当"慢消费者"掐线，而发起端前端从未实现 D3 设计时假定的重连**。

## §0 本轮实测证据

部署验证（00:28 重启，版本 08b981e ✓）后用户复测仍见「连接中断」。

| 数据源 | 内容 |
|---|---|
| journal 00:33:43-00:35:22 | turn **服务端 9 步完整跑完**，step9 `finish_reason=stop`，全程无 WARN/无截断/无 cancel |
| session `20260824-003337-69aed6877b33b81d.jsonl` | 48KB 完整落盘（含最终 assistant 回答） |
| 用户浏览器 | 「连接中断：流意外结束」错误卡 |

服务端成功 + 浏览器报流中断 → 故障只能在**浏览器↔服务器的 NDJSON 交付层**。

## §1 根因：lag-close 掐线 + 前端无自愈

### 1.1 机制链（代码实证）

```
引擎高频 emit（stream:true，reasoning/text delta 每秒可达数十行）
  → turnEntry.Write fan-out（web_bus.go:137-144）
  → 订阅者 channel 容量仅 64（turnSubchanBuffer）
  → 浏览器消费慢于生产（WiFi 抖动 / 渲染阻塞 / 标签页节流）→ handler 的网络写阻塞
  → ch 满 → fan-out 走 default 分支：close(ch)（注释自认 "client reconnects and replays"）
  → handler `case line, ok := <-ch: if !ok return`（web_turn.go:157-159）
     —— 无法区分「turn 结束关闭」与「lag 掐线关闭」，一律按结束处理 → 干净 EOF
  → 前端 reader done 且 sawTerminal=false → 「连接中断：流意外结束」
  → turn 在服务端继续跑完（journal 可证）——"会话已保存已执行部分"实为谎报（保存发生在结束时刻）
```

- `web_live.go:96-98` 同病：lag-close 也被当 turn 结束，写 `live_end`（误报结束标记）。
- **最后一击竞态**：result 事件是 runTurn 返回前最后一批写入之一，若此刻 ch 恰满，fan-out 直接 close 而非投递——**终态事件本身丢失**，即使浏览器只慢了这一瞬间。
- D3 设计注释（web_bus.go:27 "client reconnects and replays"）假定客户端会重连重放——旁观端有（409→attachSpectator），**发起端 sendTurn 从未实现**。

### 1.2 消除法佐证

浏览器侧干净 EOF（非异常）且 turn 未完，全部代码路径只有三：
1. `r.Context().Done()` → return：浏览器主动断（fetch 会 throw，不走此渲染）✗
2. `nw.WriteLine != nil` → return：连接死（前端 catch → 「请求失败」文案）✗
3. **ch 被 close（lag 或 finish）→ return**：唯一能产生「干净结束+无终态+turn 继续」的路径 ✓

测试为何没抓到：httptest syncRecorder 写内存 buffer 永不阻塞 → 测试里订阅者永不落后。真实网络（0.0.0.0:18787，跨设备 WiFi）必然出现瞬时积压。

### 1.3 与前两轮的关系

- 第一轮（上游掐流）：真实存在，v6.4.0 截断降级保内容。
- 第二轮（defer cancel 杀 turn + 无 stop 事件）：真实存在，已修复并有回归测试。
- 本轮：交付层掐线，**与前两轮正交**，且是"服务端日志全绿仍报连接中断"的唯一解释。三者叠加构成"频繁"。

## §2 修复设计

原则：保留 D3 慢消费者掐线策略（防呆死订阅者拖垮引擎），补齐它假定的客户端重连；服务端把掐线**显式告知**而非伪装成正常结束。

### F1 服务端：掐线可区分（P0）

`web_turn.go` / `web_live.go` 的 `case line, ok := <-ch: if !ok` 分支改为非阻塞查 `entry.done`：

```go
if !ok {
    select {
    case <-entry.done:
        return // turn 真结束：缓冲已排干（/live 写 live_end 后返回）
    default:
        // lag-close：写显式非终态标记后断开，客户端自愈重连
        _ = nw.WriteLine(`{"type":"stream_cut"}` + "\n")
        return
    }
}
```

- `stream_cut` 为**非终态**信息事件：语义 = "此流被服务端掐断，turn 仍在运行（或已结束），请经 /live 或会话回放重建"。
- /live 同型：lag-close 时写 `stream_cut`（**不写** live_end——轮次未完，语义不撒谎）。
- README 事件契约补 `stream_cut` 行。

### F2 前端：sendTurn 自愈重连（P0）

`app.js` sendTurn 的 `if (!sawTerminal)` 分支重写：

```
v.id 为空（session 事件未达，极早断）→ 保留原「连接中断」错误卡（兜底）
v.id 存在 → healAfterCut(v)：
  1. 渲染轻提示卡「流被中断，重连中…」（muted 样式）
  2. rebuildView(v)：v.gen++ 防旧流；v.dom 清空；重置 view 状态
     （tokens/metrics/usage.steps/trajectory/curStep/curText/curReasoning/toolNodes）
  3. runningKnown.has(v.id)（生命周期流实时状态）为真 → attachSpectator(v, false)
     —— /live 从事件 0 重放，完整重建并跟到 live_end
  4. 否则 → loadReplay(v)（GET /api/sessions/{id}，turn 已结束 jsonl 已落盘，全量回放）
  5. 自愈失败（/live 无事件且 loadReplay 空/失败）→ 渲染原「连接中断」错误卡
```

收到 `stream_cut` 事件时同样触发 healAfterCut（服务端主动告知路径）；EOF 无终态且无 stream_cut（网络层断）也触发——双保险。

复用面：attachSpectator / loadReplay / renderEvent 全部现成，新增仅 rebuildView 与分支逻辑。

### F3 总线可观测（P1）

- `turnEntry` 加 `id` 字段（register 时填），fan-out `close(ch)` 处记 WARN：
  `"subscriber lag-closed" session=<id> buf=<len> dropped=<n>`——本轮根因在日志中零痕迹，此行是未来频发度与修复效果的唯一观测点。
- `turnSubchanBuffer` 64 → 256（约 50KB/订阅者，可忽略）：delta 突发（尤其终局 result 批次）容错提升，减少自愈触发频率。F1/F2 就位后这纯为降 churn。

## §3 文件改动清单

| 文件 | 改动 |
|---|---|
| `cmd/miniagent/web_turn.go` | F1：!ok 分支查 done，lag-close 写 stream_cut |
| `cmd/miniagent/web_live.go` | F1：同型；lag-close 不写 live_end |
| `cmd/miniagent/web_bus.go` | F3：entry.id + lag-close WARN + buffer 256 |
| `cmd/miniagent/webstatic/static/app.js` | F2：healAfterCut + rebuildView + stream_cut 处理 |
| `README.md` | 事件契约补 `stream_cut`（非终态） |
| `CHANGELOG.md` | Unreleased·Fixed |

## §4 测试

| 测试 | 断言 |
|---|---|
| bus 单测：`TestTurnEntry_LagClose` | 订阅者停止 drain，写 >buffer 行后其 ch 关闭、脱离 subs（钉死语义） |
| handler 集成：`TestWebTurn_LagCutSignalsStreamCut` | 阻塞型 ResponseWriter（Write 挂起）+ fake LLM 灌 300+ delta → 响应尾含 `stream_cut`；turn 服务端照常跑完；随后 GET /live 重放含 `result`（自愈材料完整） |
| handler 集成（/live 同型） | lag-close → 收 stream_cut 且**无** live_end |
| 既有测试全绿 | finish 路径语义不变（drain 后正常 EOF / live_end） |

## §5 验收 DoD

1. 人为慢客户端（curl 限速 `--limit-rate 1k` 直连 /api/turn 流式长回答）：响应尾出现 `stream_cut`；浏览器实测：短暂「重连中…」后视图完整重建至 result，**不再出现「连接中断」**。
2. turn 进行中刷新页面：重连后完整显示（D1 + 本轮双保障）。
3. 服务重启（turn 丢失）：仍走「连接中断」兜底（诚实报错，不假装自愈）。
4. journal 可见 `subscriber lag-closed` WARN，频度可统计。
5. verify-gate 全绿。

## §6 提交计划（1 commit）

`web: self-heal subscriber streams cut on lag`

## §7 明确不做

- 不做无界订阅者缓冲（内存风险，违背 D3 慢消费者策略）。
- 不做服务端自动 resubscribe（/live 重放从事件 0 起，同连接续推会重复事件，需前端去重——复杂度大于客户端重建）。
- 不改 maxTurnBufferEvents（20000 上限不变）。
- 不为 stream_cut 加重试计数/退避（attachSpectator/loadReplay 失败即兜底错误卡，防止无限重连风暴）。
