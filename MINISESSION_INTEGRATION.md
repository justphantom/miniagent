# minisession 远程会话存储 · 全量集成方案

- 日期：2026-08-25 · 基线：HEAD e707bc1（工作树干净）
- 关联：`../minisession/ARCHITECTURE.md`（§10 客户端接入、§12 存储格式兼容）；线上实例 `http://127.0.0.1:9797`
- 验证：Client 对线上实例全流程评估 12/12 通过（认证 / CRUD / round-trip 含 Usage / 404→`os.ErrNotExist` / id 穿越拒绝），2026-08-25
- 状态：**方案待审阅，未实施**。待决策项见 §7

## 1. 结论速览

| 面 | 入口 | 现状 | 差距 | 方案 |
|---|---|---|---|---|
| CLI 轮次（新建/接续/保存） | `run_turn.go:103` | ✅ 已远程化 | 无 | — |
| WebUI 轮次 | `web_turn.go:121`（复用 turnEngine） | ✅ 已远程化 | 无 | — |
| WebUI 会话列表 | `web_sessions.go:50` | 本地 ReadDir | 列表仍扫本地目录 | 远端 ListSessions 映射（§4.1） |
| WebUI 历史回放 | `web_sessions.go:131` | 本地 LoadSession | 历史加载读不到远端会话 | 远端分支 + 404 映射（§4.2） |
| WebUI 删除 | `web_sessions.go:171` | 本地 os.Remove | 同上 | 远端 DeleteSession，守卫保留（§4.3） |
| CLI `-replay` | `replay.go:19` | 本地文件 | 回放不了远端会话 | 远端分支（§4.4） |
| 故障语义 | 全局 | 未定义降级策略 | 服务不可达行为未成文 | fail-fast、不回退本地（§4.5） |
| 存量迁移 | 运维 | 格式互通已保证 | 无操作指引 | 复制即迁移（§4.6） |

核心事实：轮次写入路径（CLI 与 WebUI）经共享 `turnEngine.runTurn` 已全部远程化；**剩余缺口全部在"读侧与列表侧"**。

## 2. 现状基线（已落地，e707bc1）

- 配置：`config.session.url/key`（config/config.go:18-24）。`url` 非空 → 远程模式，`session.dir` 整体忽略；为空 → 本地机制不变。
- 客户端：`miniagent/session/client.go` —— CreateSession / LoadSession / AppendMessages / RewriteMessages / DeleteSession / ListSessions；404 统一包装 `os.ErrNotExist`；HTTP 超时 30s；key 经 `X-Api-Key` 头。
- 保存语义：`saveSessionRemote`（session_remote.go:53）Rewrite 主路径，404 → Create+Rewrite 幂等补建；`llm_requests` 由调用方累加后随 Rewrite 全量写回。
- 新建会话不预创建：首 turn 成功前远端无该会话（与本地"jsonl 仅在首 turn 成功后落盘"对齐）。
- tool-output：远端会话无本地路径时落 `workdir/.miniagent/tool-output/{id}`（run_turn.go:145-148），保留期由既有 `run.tool_output_retention` 管。
- 测试：httptest stub 单测（client_test.go / remote_stub_test.go）+ `MINISESSION_BIN` 门控 e2e（client_e2e_test.go，自带 9799 实例）。

## 3. 不变量（实施必须保持）

1. **单一数据源**：远程模式不双写、不静默回退本地——双源必然分叉历史。切回本地（url 清空）不自动搬数据。
2. **哨兵一致**：not-found 一律 `os.ErrNotExist` 包装，web 层映射 404 的现有约定不变。
3. **落盘时机**：saveNew 首 turn 成功后才产生远端会话；失败/cancel 路径保存部分成果的行为不变。
4. **格式互通**：JSONL 行格式两端一致，存量文件复制即用；id 白名单 `[a-zA-Z0-9-]` 两端同校验。
5. **前端零改动**：WebUI 后端 handler 输出结构（sessionSummary 字段集、NDJSON 事件流形状）保持不变。

## 4. 缺口设计

### 4.1 会话列表 `GET /api/sessions`

现状逐条 ReadDir + 首行 meta + 尾部 8KB preview（web_sessions.go:57-84）。远程模式改为：

```
resolved.Session.URL != ""
    → summaries, err := s.remote.ListSessions(ctx)
    → 逐条映射进现有 sessionSummary 结构
    → err != nil → 500（与现在 ReadDir 失败同语义：配置错误不得被空列表掩盖）
```

字段映射：id/provider/model/created/size/preview 直映；modified 两端同为 `"2006-01-02 15:04"` 字符串，排序沿用字典序（=时间序）。`running` 仍由本进程 turnRegistry 判定——跨进程运行态不可见，属已知局限（见 §7-4）。

workdir 缺口：minisession 列表摘要无 workdir（仅 detail 有）。前端按 workdir 分组侧栏并在打开会话时预填输入框（sessions.js:34/132）。降级路径已存在：打开会话时 `session` 事件携带 detail meta 的 workdir 自动回填（sessions.js:153，N14 逻辑）。短期接受降级；根治需扩服务端字段（§7-1）。

### 4.2 历史回放 `GET /api/sessions/{id}`

远程分支：`remote.LoadSession(id)` → `errors.Is(err, os.ErrNotExist)` 映射 404（替换现 `meta.Type==""` 检查）、其他错误 500。尾部 200 条 cap（maxHistoryMessages）不变。NDJSON 事件流形状不变（`user_prompt` 等 replay-only 事件照常产出）。

### 4.3 删除 `DELETE /api/sessions/{id}`

`beginDelete` 在途轮次守卫保留（防本进程 writer 复活会话）；删除改调 `remote.DeleteSession(id)`，ErrNotExist→404。`session_deleted` life 广播不变。差异点：本地模式的 `<path>.tool-output` 清理在远程模式跳过（tool-output 在各 workdir 下，服务端不知位置；由 retention 兜底）——若要彻底清理需先取 detail 拿 meta.Workdir，多一次往返，见 §7-2。

### 4.4 CLI `-replay`

`runReplay(out, sessionDir, id, maxBytes)` 增加 remote 参数（nil=本地）：main.go:109-119 组装处按 `resolved.Session.URL` 分支构造 `session.NewClient`。远端无 50MB 上限问题（服务端自管），maxBytes 仅本地路径使用。

### 4.5 故障语义

| 场景 | 行为 | 理由 |
|---|---|---|
| 接续时服务不可达（非 404） | turn 立即失败，错误含 cause | fail-fast；静默回退本地 = 双源分叉 |
| 新建时服务不可达 | 同上（saveNew 本就不预创建，失败发生在首 turn 后的保存） | — |
| 运行中保存失败（saveErr） | 结果事件已流出；saveErr 走 stderr 警告并使 turn 以错误收尾（run_turn.go:190/196/203-205 现状保持） | 数据丢失窗口仅限本 turn；下次接续从上次成功状态续 |
| 请求超时 | Client 30s 超时，按上述错误路径走 | 不加自动重试；Rewrite 幂等可安全手动重试 |
| 认证失败（401） | 归入非 404 错误 → turn 失败 | key 配置错误应显式暴露 |

可选增强（默认不做）：启动时探测 `/api/health` 提前给出可读诊断；保存失败单次重试。

### 4.6 存量迁移

格式互通（minisession ARCHITECTURE §12.2），无需转换工具：

```sh
cp .sessions/*.jsonl {minisession-data-dir}/   # 默认 .minisession/data
```

- 文件名即 id，两端白名单一致；
- `.sessions/*.tool-output/` 为本地产物不随迁，继续留在原机器由 retention 清理；
- 迁移后原目录建议改名留存（如 `.sessions.migrated`）防误双跑；文档只给指引，不做迁移命令。

### 4.7 配置示例与密钥

- `config.example.json` 补 `session.url/key` 注释示例（key 明文入库与 `provider.key` 先例一致）。
- 可选：`MINIAGENT_SESSION_KEY` 环境变量覆盖（对齐 `$MINIAGENT_WEB_KEY` 先例），默认不纳入本期（§7-3）。

### 4.8 测试策略

- 复用 remote_stub_test.go 的 httptest stub 模式新增：列表映射与 500 语义、回放 404/500 映射、删除守卫+404、replay 远端分支。
- e2e 维持 `MINISESSION_BIN` 门控，不进常规 CI 路径。
- 每步实施 `make verify` 全绿（gofmt/build/vet/test -race/lint/行数/记忆完整性）。

## 5. 实施顺序（每步独立 commit，全绿后再下一步）

| 步骤 | 内容 | 预估 |
|---|---|---|
| P1 | `-replay` 远端分支（面最小，先打通读侧） | ~40 行 + 单测 |
| P2 | WebUI 三 handler 远端分支（列表/回放/删除） | ~90 行 + 单测 |
| P3 | config.example.json 示例 + README 配置节 + CHANGELOG | 纯文档 |
| P4（可选，待 §7 决策） | env key / 服务端 summary 扩 workdir / 启动健康探测 | 视决策 |

## 6. 明确不做

- 双写/混合模式、静默本地降级（违反 §3-1）
- 服务发现、多 minisession 实例路由
- 远端会话的运行态广播（running 跨进程可见）——需 minisession 侧事件通道，另立项
- CI/容器/监控类配套（AGENTS.md 红线）

## 7. 待决策项

| # | 问题 | 选项 | 建议 |
|---|---|---|---|
| 1 | 列表 workdir 分组缺口 | A. 接受降级（打开时回填已有） B. 扩 minisession summary 加 workdir（跨仓小改） | 先 A，B 随下个 minisession 迭代 |
| 2 | 远程删除是否清理本地 tool-output | A. 跳过（retention 兜底） B. 删除前取 detail 拿 workdir 再清 | A |
| 3 | `MINIAGENT_SESSION_KEY` env | A. 本期做 B. 后续 | B（YAGNI，需要多机分发配置时再加） |
| 4 | running 标记跨进程不可见的 UI 表述 | A. 不处理 B. 列表行提示"以服务端为准" | A（本期） |
