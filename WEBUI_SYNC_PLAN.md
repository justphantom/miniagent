# WebUI 多会话并发与跨浏览器同步 · 深度评估与实现方案

- 日期：2026-08-23 · 审查对象：HEAD 0699c2e（v6.3.0，工作树干净）
- 需求：① WebUI 内多会话轮次同时进行；② 多浏览器同时访问时消息与状态同步展示；③ thinking 事件文本渲染 markdown
- 方法：全域深读（web*.go / webstatic/* / run_turn.go / session / emit / event）+ node DOM shim 实测前端渲染行为
- 状态：**决策已确认（D1-D4 全按推荐），进入实施**。决策记录见 §11。

## 1. 结论速览

| 需求 | 现状 | 差距定性 | 方案 |
|---|---|---|---|
| ① 多会话同时进行 | 服务端不同 session 已可并发；新建会话被 `__new__` 全局锁串行；前端切视图即杀在途轮次 | 前端单视图架构是主 blocker；后端一处锁 + 断连语义需改 | 预生成 session id 去 `__new__`；turn 与请求连接解耦；前端多视图（detached DOM） |
| ② 多浏览器同步 | 无任何推送通道；NDJSON 只发发起者；列表仅发起方刷新；进行中轮次磁盘无落盘无法中途观看 | 无同步基础设施 | turnRegistry 事件总线 + 全局生命周期流 + 按会话 live 流 + 列表 running 字段 |
| ③ thinking 渲染 md | **结束时已渲染**（实测：finishText 对 reasoning 与 text 同走 mdRender）；**流式阶段为纯文本**，原始 md 语法直出 | 流式阶段未渲染；thinking 恰是流式最长的可见内容（推理模型分钟级） | 节流增量重绘（300ms）+ finish 兜底 |

## 2. 现状评估（证据链）

### 2.1 多会话同时进行

服务端就绪度（高）：

- `web_turn.go:69-80`：per-session `sync.Map` + TryLock，不同 id 天然并发；同 session 409。
- 共享状态核查全部线程安全：`turnEngine.cfg` 只读；`transportCache`（setup_providers.go:26）带 mutex；`pathLocks` 每 turn 实例（tool_edit.go:69）；tools/hooks/compaction 每 turn 重建。
- 事件契约 CLI/Web 逐字节一致，无跨 turn 可变全局。

三个真实 blocker：

1. **`__new__` 全局锁**（web_turn.go:69-72）：新建会话轮次全局串行——浏览器同时开两个新会话，第二个 409。锁存在的唯一原因：session id 在 `runTurn→resolveSession` 深处才生成，handleTurn 无 key 可锁。id 为时间戳+64bit 随机，冲突 free，锁本无正确性必要（注释自认 acceptable）。
2. **前端切视图即杀轮次**（app.js:88-89、399）：`openSession`/`new-chat` 先 `state.abort.abort()` → fetch 断开 → `r.Context()` 取消 → handleTurn 的 ctx（WithTimeout(r.Context())）取消 → Run 收 context.Canceled，部分结果落盘，**轮次死亡**。一个浏览器永远只能有一个在途轮次。
3. **关标签/断网同理杀轮次**：turn 生命周期绑定在请求连接上。

### 2.2 多浏览器同步

- **无推送通道**：会话列表仅在发起轮次的浏览器 `send()` finally 里 `loadSessions()`（app.js:198）；其他浏览器永不过期，新会话/删除/状态均不可见。
- **流只发发起者**：NDJSON 写 per-request ResponseWriter；浏览器 B 无法观看 A 发起的进行中轮次。B 对 running 会话发 turn 得 409（有反馈、无进度）。
- **中途加入无数据源**：session jsonl 仅在轮次结束 `saveSession` 落盘（run_turn.go:160）——进行中轮次的事件只存在于发起者的 HTTP 流，磁盘没有。跨浏览器观看必须服务端缓冲在途事件。
- **状态不可见**：running/idle 无 API 暴露（locks 是内部实现）。

### 2.3 thinking 渲染 markdown（实测）

node DOM shim 驱动 `events.js` 实测：

- finish 后：`reasoning md html: "<h2>思考标题</h2><p><strong>重点</strong> <code>code</code></p>"`——与 text 同构，**已渲染**。CHANGELOG v6.x 自述与实现一致。
- 流式中：`streaming reasoning span text: "**streaming 阶段** 可见内容"`，无 md div——**原始语法直出**。
- 根因：`appendDelta` 用 `_span.textContent` 累积（webui-architecture 决策 #3：防 O(n²) 重解析与闪烁），mdRender 推迟到 finishText。对 text 代价小（最终答案流得快）；对 reasoning 代价大（流式主体就是 thinking，分钟级注视原始 md 语法）。

## 3. 方案总览

```
浏览器A(发起)                    浏览器B(观战)
  POST /api/turn ──┐               GET /api/sessions/{id}/live (缓冲重放+live)
                   ▼               ▼
            turnRegistry（事件总线，server 内）
            ├ runningTurn{cancel, buffer[]NDJSON, subs[], done}
            ├ 同 session 注册冲突 → 409（保持 N7 语义）
            └ 生命周期广播 ──► GET /api/events（全局流：turn_started/turn_finished/
                               session_deleted/ping）──► 所有浏览器刷新列表/状态
引擎 out 不再直连 ResponseWriter，写总线；总线扇出（订阅者死亡不影响引擎——
OnToolUse emit 错误会终止轮次，tool_handler.go:35，故总线 Write 恒 nil error）
turn ctx 脱离 r.Context()（WithTimeout(baseCtx)）：断连不杀轮次；
显式停止走 POST /api/sessions/{id}/stop
```

关键决策（详见 §11 决策点）：

- **传输用 fetch 流式 NDJSON，不用 EventSource/SSE 语义**：EventSource 无法带 `x-api-key` 头（标准限制），query 传 key 会泄漏进日志；前端已有成熟的 fetch reader 循环可复用。CSP `connect-src 'self'`（web_guard.go:35）已放行。
- **live 流缓冲重放从 event 0 开始，不做游标**：进行中轮次与磁盘 replay（仅含已结束轮次）天然无重叠，客户端零去重；单轮缓冲内存有限（见 §4.4）。
- **turn 与连接解耦是 ①② 共同地基**：解耦后「切视图不杀轮次」「关标签不杀轮次」「B 中途观战」三件事一并成立。

## 4. 后端设计

### 4.1 turnRegistry（新文件 `web_bus.go`，~220 行）

```go
type turnEntry struct {
    kind    string        // "running" | "deleting"
    cancel  context.CancelFunc
    buf     []string      // 本轮全部 NDJSON 行（扇出源 + 中途加入重放源）
    subs    map[chan string]struct{}
    done    chan struct{} // 关闭于轮次结束；err 字段随后可读
    err     error
    mu      sync.Mutex
}
type turnRegistry struct { mu sync.Mutex; m map[string]*turnEntry }
```

- `register(id) (*turnEntry, busy bool)`：存在任何 kind 条目 → busy（409）。**替代** `s.locks`（删除互斥并入：`beginDelete` 建 kind=deleting 条目挡新 turn，现有 TryLock 删除语义不变）。
- `Write(p)`（实现 io.Writer，作引擎 out）：按行切分，append buf + 非阻塞扇出（chan 满 64 → 关闭该订阅并标记 lagging，客户端重连重放即可恢复，无丢失语义破坏）；**恒返回 (len(p), nil)**。
- 订阅握手在 entry.mu 内：先拷贝 buf 快照再挂 live chan，保证不重不漏；单写者（轮次 goroutine）保证全序。
- 轮次结束：close(done)、广播 turn_finished、条目移除（订阅 chan 各自 drain 后由 GC 回收）；buf 随条目释放。

### 4.2 handleTurn 改造（web_turn.go）

1. 校验不变（content-type/prompt/workdir/上限）。
2. `saveNew` 时**预生成 id**（复用 `generateSessionID`）传入 `turnSpec.sessionID`；`resolveSession` 增参：saveNew 且预置 id 非空则用之（CLI 传 "" 走原路径）。**删除 `__new__` 锁**——注册 key 恒为真实 id。
3. `register(id)` busy → 409（语义同今日 N7）。
4. **ctx 解耦**：`turnCtx = context.WithTimeout(s.baseCtx, defaultWebMaxDuration)`（`s.baseCtx` 来自 runServe 的 ctx：服务器关闭时在途轮次走取消+保存路径）。请求方 ResponseWriter 降级为普通订阅者。
5. 请求 goroutine 只负责把订阅事件写回响应并 flush；客户端断开仅结束该订阅。轮次 goroutine 结束后 close 响应流。

### 4.3 新端点

| 端点 | 行为 |
|---|---|
| `POST /api/sessions/{id}/stop` | `entry.cancel()` → 轮次走 context.Canceled 路径（保存已执行部分，同今日停止按钮语义）；204。无在途轮次 → 404。鉴权同 /api/* |
| `GET /api/sessions/{id}/live` | 有在途轮次：重放 buf 全量 + live 续流（NDJSON；`X-Accel-Buffering: no`、逐行 flush）；轮次结束后流即关闭。无在途轮次：200 + 单行 `{"type":"live_end"}` 关闭（客户端靠全局流获知何时重开） |
| `GET /api/events` | 全局生命周期流：`turn_started {session}` / `turn_finished {session,ok,error}` / `session_deleted {session}` / 每 15s `ping`（防代理空闲断连 + 检活）。关闭：服务器 shutdown channel |

- `GET /api/sessions` 行新增 `running bool`（registry 查询）与 `turn_started_ms`；列表开销不变（map 查找）。
- 并发上限（O8 落地）：`web.max_concurrent_turns`（默认 0=不限，保持现状；溢出 429）。信号量在 register 前 acquire。

### 4.4 边界与不变量

- **缓冲上限**：`maxTurnBufferEvents = 20000` 行（内部常量，不 config 化防旋钮膨胀）。超限丢最旧并置首行 `{"type":"live_truncated","dropped":N}`——中途加入者诚实看到截断。单行尺寸已有界（tool_result 2000 字符封顶、delta 为 chunk 粒度），最坏几 MB/轮，轮次结束即释放。
- **写入失败隔离**：总线 Write 恒 nil error（依据 §3：emit 错误终止轮次，订阅者死亡不得反杀引擎）。
- **Shutdown**：live/events 长连接 select baseCtx done 即刻收尾，不阻塞 `srv.Shutdown`；在途轮次被 baseCtx 取消 → saveSession 部分保存（复用现有取消路径）。
- **409/删除互斥**：register 与 beginDelete 同 map 同锁，今日「删除 vs 在途轮次」竞态语义原样保留。
- session 文件一致性：轮次结束才 RewriteMessages（原子 rename），replay/列表的 LoadSession 读旧或读新，无撕裂（现有保证，不变）。

## 5. 前端设计

### 5.1 多视图模型（新模块 `sessions.js`；store.js/events.js 改造）

```js
const views = new Map(); // id → view
// view = { id, dom: <div class="view">, gen, live: AbortController|null,
//          status: "idle"|"running", tokens:{in,out}, workdir, scroll }
```

- `#events` 降级为视口：切换 = 换子节点；渲染进 detached DOM 天然可行。
- **events.js 视图化**（核心重构）：模块级 `curText/curReasoning/toolNodes` → 挂到 view（`view.curText/…`）；`renderEvent(ev)` → `renderEvent(view, ev)`。导出面小，改动集中。
- **M8 generation → per-view gen**：流循环按 view 归属丢弃过期事件，语义同今日、作用域缩到单视图。
- 切视图**不再 abort**（R1 核心）：后台轮次的 reader 持续写自己的 detached DOM；重挂即见全部历史。仅「停止」按钮显式调 stop API（或对本机发起的轮次 abort fetch——但断连已不杀轮次，统一走 stop API 更简单）。
- 内存：常驻 ≤8 个视图 DOM，逐出最旧 idle（running 不逐出）；被逐出视图重开时走 replay+live 重建。
- composer 绑定活动视图：workdir 逐视图保存草稿；发送目标 = 活动视图 id；`session` 事件到达时把临时视图升级为正式 id（新建会话首事件）。

### 5.2 同步接线（新模块 `live.js`）

- boot 后开一条全局 `/api/events` 流（fetch reader，复用现有 NDJSON 解析循环）：驱动列表状态点（running 呼吸点）、新会话插入、删除移除、mtime 重排（turn_finished 后刷新单行或整表）。
- 活动视图且 `running` 且非本机发起 → 开 `/api/sessions/{id}/live` 渲染到该视图；`turn_finished` 后关流。
- **409 升级**：对 running 会话发送得 409 时，自动附到该会话 live 流并提示「轮次进行中，已进入旁观」——多浏览器协作的主场景。
- 断线重连：全局流/live 流意外断开 → 2s 退避重开；live 重开重放 buf 全量，与已有 DOM 不重叠的前提是**同一轮次内只附一次**——重连前记住已渲染的轮次代（turn_started 计数），重放时丢弃旧代事件。简化替代：重连即重建该视图（replay + live），实现最省、代价是闪烁一次。**取后者**（YAGNI）。
- token 计数逐视图累计（result 事件），header 徽章显示活动视图值。

## 6. thinking 流式 markdown 渲染

- `appendDelta` 不再直写 span 文本；`_md` 累积 + **脏标记**；300ms `setInterval` 节流重绘：`md.innerHTML = mdRender(view._md)`（text/reasoning 一致处理，行为统一）。仅当视图**已挂载且可见**才重绘（后台视图跳过，finish 兜底）。
- flicker/perf：50KB thinking 每 300ms 一次 parse，桌面/移动均可行；超 64KB 停止重绘等 finish（防极端）；滚动跟随沿用 stickBottom；代码块复制按钮仍只在 finish 绑定（避免每 tick 重绑）。
- 中途态健壮性（已核实）：mdRender 对未闭合 code fence 渲染已有 body；表格分隔行流式途中短暂成段、下一 tick 自愈——可接受。
- 备选对比：① 维持现状仅 finish 渲染（不满足需求）；② 每 delta 即渲染（O(n²)+闪烁，webui-architecture 决策 #3 已否）；③ 本案节流重绘——取③。
- 用户 prompt 保持 textContent 不渲染（安全边界不动）。

## 7. 契约变更与外部消费方影响（L0 #5）

| 项 | 变更 | 影响 |
|---|---|---|
| `/api/turn` NDJSON 流 | 字节不变 | 无 |
| `/api/turn` 断连语义 | 客户端断开**不再取消**轮次 | **行为破坏性**：曾依赖「关页面=停轮次」的用法改走 stop 按钮/API。CHANGELOG breaking 单列+迁移说明 |
| `/api/sessions` | 行新增 `running`/`turn_started_ms` | 加字段，非破坏 |
| 新端点 stop/live/events | 纯增量 | 无 |
| config | `web.max_concurrent_turns`（默认 0） | 新键，decodeConfigStrict 需同步（否则旧 config 加新键被拒…新增即支持，无迁移） |
| CLI（stdout 事件） | 零变化 | 无 |

## 8. 测试计划（stdlib，用例即需求）

后端（`web_bus_test.go` 新增、`web_turn_test.go`/`web_test.go` 扩展）：

1. register 冲突 → 409；结束后再 register 成功（同 id 复跑）。
2. 双订阅者收到**逐字节相同**事件序列；中途订阅者收到 buf 全量+后续 live，无重无漏。
3. 订阅者 write 挂死/关闭 → 引擎继续、其他订阅者不受影响（总线 Write 恒 nil）。
4. 请求 ctx 取消（模拟断连）→ 轮次**继续完成**、session 文件完整落盘（fake LLM 断言收到的调用次数）。
5. stop API → 轮次取消、部分结果已保存、订阅者收到终结事件。
6. 并发两个**新建**会话轮次均成功（回归 `__new__` 移除）。
7. 并发上限：第 N+1 个 → 429；完成一个后放行。
8. 删除 running 会话 → 409；idle 删除后新 turn 不复活（沿用今日用例改走 registry）。
9. /api/events：turn_started/finished 顺序与 ok/error 字段；ping 不污染事件序。
10. shutdown：baseCtx 取消 → live/events 流即刻关闭、在途轮次保存退出。
11. 列表 running 字段随注册/结束翻转。
12. live_truncated：灌 >maxTurnBufferEvents 行后中途订阅见标记与 dropped 计数。

前端：仓库无 JS 测试基建（仅 `node --check`）；不擅自引入框架。以 §9 各阶段手工验收清单 + DOM shim 冒烟脚本（本评估已验证可行，可留 `bin/` 外临时脚本，不入库）为准。

## 9. 实施阶段（每阶段独立可交付、verify-gate 全绿）

| 阶段 | 内容 | 规模 | 价值 |
|---|---|---|---|
| P0 | thinking 节流渲染（§6） | events.js ~40 行 | 立即可见，零风险 |
| P1 | registry + 断连解耦 + stop API + 预生成 id 去 `__new__` + 并发上限 + 后端测试 | web_bus.go 新 ~220 行；web_turn/web_sessions/web.go 改 | 多会话服务端真并发；停止语义闭环 |
| P2 | /api/events + /api/live + 列表 running + 前端 live.js（旁观/409 升级/状态点） | web_live.go 新 ~150 行；live.js 新 ~120 行 | 跨浏览器观战与列表同步 |
| P3 | 前端多视图（sessions.js + events.js 视图化 + composer/停止/逐出） | 前端主重构 ~300 行改动 | 单浏览器多会话并行 |
| P4 | 文档：CHANGELOG（breaking：断连语义）、README WebUI 节、ARCHITECTURE §cmd 树、.agent/L2/decisions/webui-architecture.md 增补 | 文档 | 契约留痕 |

依赖链：P1→P2→P3；P0 独立。行数预算均 <300/文件。

## 10. 风险清单

| 风险 | 对策 |
|---|---|
| 断连不杀轮次 → 失控轮次无人停 | stop API 与解耦**同 release**；web 默认 10min 超时兜底（已存在） |
| 总线缓冲内存（长轮次+多订阅） | 行数上限+逐行截断已有界；轮末释放；不 config 化 |
| detached DOM 长驻内存 | ≤8 视图逐出策略；running 例外仅限当下 |
| 流式重绘闪烁/滚动跳动 | 300ms 节流 + stickBottom 已有 + 复制按钮延后绑定 |
| Shutdown 被长连接阻塞 | 流 select baseCtx；runServe 现有 5s 强制 Close 兜底 |
| 竞态：turn_finished 与列表刷新 | 客户端幂等（running 字段以服务端为准，事件仅触发拉取） |
| 回归：同 session 409 语义 | 用例 1/8 直接钉死 |

## 11. 决策点（2026-08-23 用户已确认，全按推荐）

- **D1 断连语义**：✅ 确认「继续执行 + stop 接管」——turn ctx 脱离 r.Context()，断连/切视图/关标签不杀轮次；停止走 `POST /api/sessions/{id}/stop`。
- **D2 并发上限默认**：✅ 确认默认 0（不限，行为零变化）；`web.max_concurrent_turns` opt-in，溢出 429。
- **D3 live 重连策略**：✅ 确认「重连即重建视图」（简单路径，不做的轮次代去重）。
- **D4 前端测试**：✅ 确认不引入 JS 测试框架；以手工验收清单为准。
