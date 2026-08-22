# miniagent 评估报告（第二轮）· 整改实施审查

- 日期：2026-08-22 · 审查对象：3fa5bb6「apply second-round webui/serve remediation」+ 工作树未提交收尾（最高约束禁提交）
- 方法：逐项 diff 审读 + 边界条件推演 + verify-gate 实测 + 声明与实现一致性核对（session.md 清单 vs 代码 vs git 状态）
- 实测：`gofmt -s -l` 空 / `go build` / `go vet` / `go test -race -count=1 ./cmd/miniagent/` 全绿（全套 14 包绿）/ `golangci-lint` 0 issues / 非测试文件 ≤300 行 / 4 个 JS `node --check` 通过

## 0. 审查结论

**15 项中 14 项实施且质量合格，1 项部分实施；P3 两项（N11/N12）按报告建议留待决策，未实施——与声明一致。** 质量总体高于上轮整改：每个修复都带溯源注释（N1-N15 编号可追回本报告），N7 配了真实并发测试，N2 的焦点圈闭超出建议范围（建议只提了 role="dialog"）。

| 分组 | 项 | 状态 |
|------|-----|------|
| P0 | N1 CSP FOUC / N2 Enter 危险 | ✅ 已提交（3fa5bb6），质量优 |
| P1 | N3 Transport 缓存 / N4 有序列表 / N7 TryLock 409 | ✅ 已提交，质量优 |
| P2 | N5 空态 / N9 主题态 / N10 Names() / N13 elapsed / N8 删文件 | ✅ 已提交（N5 部分，见 §2） |
| P2 | N6 README 微漂移 / N14 workdir 回填 / N15 CSS 格式化 / CHANGELOG | 🟡 实施完成但在工作树**未提交** |
| P3 | N11 动态 Host / N12 并发上限 | ⏸ 未实施（待产品决策，符合报告建议） |

## 1. 逐项质量审查

**N1（FOUC）✅ 正确且实现优雅。** `index.html` 删内联 `style="display:none"`；`app.css:72` `#login, #app { display: none; }` 且注释明确「keep the hide last so it wins」——规则顺序经核实确实压过 `:70` 的 `#login` flex 布局规则，CSSOM 的 `style.display="flex"`（inline 优先）揭示面板。未加 `unsafe-inline`，保留了 CSP 强度（按报告推荐路径）。首次绘制只出 `<body>` 背景，零闪现。

**N2（Enter 危险）✅ 正确且超范围加固。** Enter 仅 `activeElement === btnOk` 时确认（app.js:256）；焦点初始在 btnCancel，按 Enter 走按钮原生 click → 取消——直觉方向正确。超额部分：`role="dialog"` + `aria-modal="true"` + Tab/Shift+Tab 焦点圈闭。复核了二次 close 幂等性（keyDown close + 默认 click close 并发触发 → resolve 已 settle、remove 幂等，无害）。

**N3（Transport 泄漏）✅ 正确。** `transportCache.get` nil-receiver 安全（CLI/测试传 nil 时回落每 turn 新建，不炸）；mutex 保护 map；按 provider name 键控成立（config 无热加载，启动后集合不变）。`IdleConnTimeout: 90s`（setup_transport.go:21）。CLI 路径也统一传 `&transportCache{}`（main.go:132）——单 turn 无收益但语义一致。`buildClients` 签名变更波及的测试已同步（setup_test.go）。

**N4（有序列表）✅ 正确。** `^\s*\d+\.\s+` → `<ol>`，连续行合并，li 内容走 mdInline（已转义，无 XSS 面）；`.md ol` 样式补齐。注释如实声明「不重现 markdown 重编号，按源码序号呈现」——合理的子集边界。

**N7（并发 409）✅ 正确，测试到位。** TryLock 替代 Lock（web_turn.go:74-79），失败即 409 `"a turn on this session is already in progress"`；锁获取点在 JSON 校验之后、NDJSON header 设置之前——409 走纯 JSON 响应，流契约不受污染。`TestWebTurn_SameSessionSerialized` 改写为 hold 锁 → 第二请求断言 409，语义与实现匹配。前端侧 409 落入既有 `resp.ok` false → error 事件卡路径，可辨识。

**N9/N10/N13 ✅。** N9 applyTheme 统一 ◐/◑ + 方向化 title；N10 `webstatic.Names()` 经 `fs.Glob` 派生路由名单，embed 指令成为唯一事实源；N13 result 后 `turnStartTs=0`，回放多轮各自计时（与 send() 的重置点一致）。

**N6/N14/N15/CHANGELOG 🟡 已实施未提交。** 内容本身合格：README ◐/◑、20→24 行与 `LONG_TEXT_LINES=24` 对齐；N14 的 `workdirFilled` 守卫正确防住 sessionMeta 命中后的重复回填，replay 首事件回填受 generation 守卫保护；CHANGELOG Unreleased 分 Added/Fixed/Changed/Removed，逐条挂 N 编号，与实码一一对应（抽查 N3/N7/N8 属实）。**建议尽快提交**：当前 HEAD 的 README 与实码有一处已知失配（◐/◑），工作树才是全量正确态。

**N8 ✅ 已删除但在工作树。** `.agent/L1/assessment.md` D 状态未提交——同上，随收尾一并提交。

## 2. 发现的实施偏差（1 项）——已修复

**N5 失败态缺失。** session.md 声称「三态占位（加载中/空态/失败）」，实际只有两态：`loadSessions` 开头 `box.innerHTML=""` + 「加载中…」，catch 块是 `/* keep old list */`——但旧列表已被开头的 innerHTML 清空，**API 失败后侧栏永远停留在「加载中…」**，既非旧列表也无错误提示。
**修复（本轮）**：catch 改为 `加载失败：<原因>（点击重试）` 占位，hint 挂 click 重试。三态齐。

## 3. 新观察（非缺陷）——两条已处置

- **N14 行为扩展**：旧逻辑仅在 sessionMeta 命中时覆盖 workdir 输入；新逻辑在 meta 缺失时由 replay 事件回填——若用户先手填 workdir=X 再点开 meta 无 workdir 的会话，X 会被会话值覆盖。**已收紧（本轮）**：回填条件补 `!$("workdir").value.trim()`，显式输入恒胜隐式回填。
- **N10 边界**：`fs.Glob("static/*")` 未来若嵌入子目录会返回目录名，注册的路由 Read 失败回 500。当前全部平铺文件，无影响。**已钉死（本轮）**：新增 `webstatic/assets_test.go` `TestNames` 断言 6 文件精确集合 + 每项可 Read，未来嵌入子目录即测试红。

## 4. 遗留清单

| 项 | 内容 | 状态 |
|----|------|------|
| 提交 | 工作树 8 文件（N5 修补/N6/N8/N14 收紧/N15/CHANGELOG/README/审查报告/session.md/assets_test.go） | 待用户提交（最高约束禁提交） |
| N11/N12 | 动态 Host / 并发上限 | 维持待决策；若采纳，N12 轻量路径是 turn 并发信号量 |
| N5/N14/Names 测试 | 本轮处置 | ✅ 清零 |
| 上轮遗留 | 无 | 上轮 45 项已全部核验清零 |

## 5. 结论

本轮整改**实施率 14/15（93%）→ 审查后追加修补，现为 15/15 全清**；已实施项全部通过审查。修复质量是三轮整改中最高的一轮：溯源注释、超范围加固（N2）、真实并发测试（N7）三者齐备。verify-gate 全绿（含新增 webstatic 包测试，15 包 ok）。

剩余动作唯一项：**用户提交工作树**（HEAD 的 README ◐/◑ 失配在提交后消除）。P3 两项维持待决策。

综合评价 **8.7 → 8.8**（N5 偏差清零、N14 收紧、Names 契约钉死；扣分项仅剩未提交状态与 P3 两项待决策）。
