# miniagent 规范统一实施方案（UNIFICATION_PLAN）

> 状态：草案 v1 · 2026-09-14 · 上游引用 `/home/work/Codes/tmp/GROUP-STANDARD-DRAFT.md` 与 `/tmp/wf/unification-report.md`。
> 声明：本文件是**规划文档**，不含任何已执行的改动；`🔒` 项须用户授权后才能动手。所有 `file:line`、target、脚本名、计数均为 2026-09-14 实测（仓库 HEAD `49fb003`，工作树干净）。

## 0. 现状摘要

- **module**：`github.com/justphantom/miniagent`（D1 之 b 风格；README 主打「外部可直接 import」库能力）；`go.mod` 仅 3 行，`go 1.25`（无 toolchain、零 require、无 go.sum）。
- **Makefile 范式**：**旧范式**——verify 门禁内联在 Makefile（非 scripts/ 独立脚本），`golangci-lint run ./...` 是**硬依赖**（未装即 verify FAIL，README:71 明言须预装）；无 `vet`/`help`/`build-cross` target。
- **部署派系**：`.service.tpl + envsubst` 派（GS-D 范本级：模板入库 + `grep '\${'` FATAL + `.env.example`/`.env` 双层 + 加固段），但**缺部署后探活（GS-D-6）与回滚（GS-D-7）**，unit 为 `ProtectSystem=full`（组模板为 strict）。
- **日志阵营**：`log/slog` 直用（GS-L-1 达标），级别走 CLI flag `-log-level`（非组标准的 config 键 `log.level`）。
- **配置层**：JSON + `DisallowUnknownFields` + 4MiB 上限 + O_NOFOLLOW（GS-C 强化），**缺 `${VAR}` 展开（GS-C-3）**；默认路径缺失是硬错误（GS-C-5 偏离）。
- **记忆体系**：`.agent/` 三层 + `verify-memory.sh`（76 行，D5-a 范本）。
- **卫生**：良好——bin/、`.miniagent/`、`.env` 均被 ignore；本地残留仅 `cmd/miniagent/.miniagent/sessions/`（**15 个**调试会话）与 `bin/miniagent`（8.1MB）。
- **发布**：根目录 `release.sh` 是**组发版门禁范本（GS-X-4 出处）**（annotated tag + 干净树 + `make verify`）。
- **CI**：无 `.github/`/容器（D4-a 达标）；**依赖**：零第三方（GS-DEP 范本）。

## 1. 依赖裁定清单

| D 编号 | 若按草案建议 → 本仓做什么 | 若选另一项 → 本仓做什么 | 本仓视角倾向 |
|---|---|---|---|
| D1 module 命名 | b（全 `github.com/<owner>/<name>`）：**已达标**，保持 `github.com/justphantom/miniagent` 不变 | a（全裸名）：全仓 import 重写（实测 139 个 .go 文件 / 223 处引用）+ Makefile ldflags 符号 `github.com/justphantom/miniagent/miniagent.Version`→`miniagent.Version` + README/MINISESSION_INTEGRATION 引用 | 倾向 **b**。本仓是库（外部可 import 是卖点），改动最小；选 a 是一次性大重写，且 owner（justphantom）在 b 方案下仍需定唯一 owner |
| D2 go directive | a（1.25.0）：`go.mod` `go 1.25`→`go 1.25.0`（**1 行**，本机 go1.25.13 零下载） | b（1.26.0）：需先 grep 确认未用 1.26 特性，再统一升 | 倾向 **a**（成本最低、零下载） |
| D3 「禁止改 .gitignore」红线 | b（改需用户批准+说明+CHANGELOG）：可补 `logs/`、`*.log`（🔒 待授权） | a（维持冻结）：本仓无 .gitignore 改动 | 本仓 .gitignore 41 行手写精简（非 mega-template），**无 P0 暴露**，需求弱；如组定 b 则低优先补 |
| D4 不引入 CI | a（维持无 CI）：**已达标**——本仓即纯本地 `make verify` + release.sh 门禁范本 | b（最小 CI）：需用户批准引入（不推荐，与现文化相悖） | 倾向 **a** |
| D5 记忆体系 | a（`.agent/`）：**已达标**——本仓 `.agent/` 三层是组范本；`verify-memory.sh` 即草案拟统一的两流派之一（66–76 行派） | b（`.memory/`）：迁移全仓记忆（本仓是 .agent 最成熟仓，迁移成本最高） | 倾向 **a** |
| D6 novel-editor YAML | 不影响本仓（本仓 JSON） | — | 无动作 |
| D7 lark-bridge log wrapper | 不影响本仓 | — | 无动作 |
| D8 lark-bridge IPC `/v1` | 不影响本仓 | — | 无动作 |
| D9 minidbman 用户级 systemd | 不影响本仓（本仓系统级） | — | 无动作 |

## 2. Phase 0 —— 本仓自治整改（无需裁定，可立即执行）

| 编号 | 改动 | 涉及文件 | 验收命令 | 回归风险 | 回滚 |
|---|---|---|---|---|---|
| P0-1 | 清理本地调试会话 | `cmd/miniagent/.miniagent/sessions/`（15 个 jsonl，均被 `.gitignore` 覆盖） | `ls cmd/miniagent/.miniagent/sessions/ | wc -l` → 0；`git status` 仍干净 | 无（非跟踪、纯占盘） | 不可逆（本地临时数据，无需保留） |
| P0-2 | 清理本地构建产物 | `bin/miniagent`（8.1MB，被 `bin/` ignore） | `make clean` 后 `ls bin/` 空；`git status` 干净 | 无 | `make build` 重建 |
| P0-3 | 修 version 回退值 | `miniagent/version.go:6` `var Version = "v6.6.6"` → `"dev"`（GS-V-2；非 ldflags 构建现报 v6.6.6 而注释自称 dev，code/comment 双不一致） | `make verify` 全绿；`go build ./cmd/miniagent && ./bin/miniagent -version` → `dev` | 极低（仅非 Makefile 构建路径的展示值） | 改回一行 |
| P0-4 | 修 README verify 步数 | `README.md:68`「verify-gate 五步（gofmt/build/vet/test -race/lint）」→ 实为**七步**（gofmt / build / vet / test -race / lint / 行数上限 / 记忆完整性） | `make verify`；`grep -n 五步 README.md` 无命中 | 无 | 改回 |
| P0-5 | 修 release.sh 注释步数 | `release.sh:29` 注释同「AGENTS.md 五步」→ 七项 | `grep -n 五步 release.sh` 无命中 | 无（纯注释） | 改回 |
| P0-6 | 修 CONTRIBUTING verify 清单 | `CONTRIBUTING.md:8-15` verify-gate 代码块只列 5 条命令 → 补 `行数上限`、`记忆完整性` 两项（对齐 AGENTS.md:15 七项口径） | `make verify`；读块比对 AGENTS.md 七项 | 无 | 改回 |
| P0-7 | 消除 session.dir 分叉 | `config.example.json:29` `session.dir:".sessions"` → `".miniagent/sessions"`（对齐 `cmd/miniagent/session.go:24` 代码默认）；**协同**更新 `deploy/deploy.sh:71` 的 `sed` 匹配串 `.sessions`→`.miniagent/sessions` | `grep -n 'session.dir\|\.sessions' config.example.json deploy/deploy.sh`；`make verify`（不触发部署） | 中低：改脚本 sed 需同步（两处一起改则一致）；非部署本地运行将从 `.sessions` 回到代码默认目录 | git checkout 两文件 |
| P0-8 | （可选）README:38 措辞澄清 | `README.md:38`「所有核心包已移出 internal/」加注「（internal/ 仅余测试辅助包 looptest）」——CONTRIBUTING.md:35 已准确，此处仅措辞 | `make verify` | 无 | 改回 |
| 🔒P0-9 | 补 .gitignore 忽略项 | `.gitignore` 增 `logs/`、`*.log`（当前无日志文件、无暴露） | —（挂 D3） | — | — |

> 表后注：`P0-9` 涉 `.gitignore`，挂 **D3** 待授权，不并入本 Phase 0 批次；其余 P0 项均无需裁定。
> `P0-7` 影响面说明：`config.session.dir` 非空即覆盖 env 默认（main.go:121 / web_sessions.go:57，实测），故**非部署的本地运行**也会随示例 `.sessions` 回到代码默认 `.miniagent/sessions`——属预期收敛（示例对齐代码），但为用户可见行为变化，建议 CHANGELOG 记一笔。

## 3. Phase 1 —— 规范收敛

### 3.1 改动项（每项标 GS 编号 / 门控）

| 项 | 内容 | GS / 门控 |
|---|---|---|
| M1 | **Makefile 迁新范式**：① 抽 `scripts/check-fmt.sh`、`scripts/check-line-limits.sh`（移植 minidocman 范本；**gate 用 `test -z` 判非空，不依赖 gofmt -l 退出码**，GS-M-4 landmine）；② 增 `scripts/check-shell.sh`（POSIX 自检，herdr-bridge 范本）；③ **golangci-lint 改可选**——`if command -v golangci-lint; then run; else echo SKIP; fi`（本仓核心改动，GS-M-2）；④ 重排 verify 链顺序为**轻量检查前置**（GS-M-2：check-fmt → 行数闸 → verify-memory → go build → go vet → go test -race → golangci-lint），现为「重项在前、行数/记忆末尾」；⑤ 增 `vet`、`help` target；⑥ 增 `build-cross`（GS-M-6——实测 5 个 `*_windows.go`，`GOOS=windows go build ./...` 产物丢弃，minisession 范本）；⑦ 增 **bootstrap 跳过**（GS-M-3，实测现缺，与范式范本仓 minidocman/minidbman/herdr-bridge 不一致）；⑧ build 输出与 cross 均用 `./...` 全包编译，无需另设 binary target（minisession 因安装路径需分离才拆 binary，本仓 `bin/miniagent` 已达标 GS-N-2） | GS-M-1/2/3/4/6（无门控） |
| M2 | **行数闸加「跟踪 md ≤300」桶**（GS-M-2）：现仅非测试 Go / 测试 Go / static JS 三桶。⚠️ 本仓 `README.md` **536 行**超限——需先决策：拆分 README（Phase 2，大改）**或**登记核心文档豁免。建议：暂缓 md 桶，登记 README/CHANGELOG(855, 已豁免) 为已知超限，避免门禁一次阻塞全部提交 | GS-M-2（无门控） |
| D2-1 | go directive 对齐 `go 1.25.0`（若 D2-a 通过） | GS-META-2【D2】 |
| X1 | CLAUDE.md 单行化 `@AGENTS.md`（GS-X-1）；**将现「禁止提交」两条头并入 AGENTS.md 行为红线**，避免约束丢失 | GS-X-1（无门控） |
| DPL-1 | unit 加固对齐组模板：`ProtectSystem=full`→`strict` + `ReadWritePaths` 增 config 目录（`${MINIAGENT_CONFIG}` 所在目录，原子 rename 需目录写）+ `RestrictSUIDSGID`；保留 ProtectHome 省略注释（workdir 可位于 /home） | GS-D-2（无门控） |
| DPL-2 | 部署即验证：`cmd/miniagent/web.go` mux 增 `GET /api/health`（免鉴权，与 whoami 同级）；`deploy/deploy.sh` 末尾增 ~10s `curl /api/health` 探活，失败 dump `journalctl -n 20` 并非零退出 | GS-D-6（无门控） |
| DPL-3 | 部署回滚：deploy.sh 安装二进制前备份 `.prev`，探活失败自动恢复（吸收 llm-proxy 模式） | GS-D-7（无门控） |
| N1 | `config.example.json` 自根目录移 `deploy/config.example.json`（GS-N-1/GS-D-4），同步更新 `deploy/deploy.sh:67`（`$SCRIPT_DIR/../config.example.json`→`$SCRIPT_DIR/config.example.json`）、README.md:80/110 引用、MINISESSION_INTEGRATION.md | GS-N-1/GS-D-4（无门控） |
| N2 | 过程文档归档（可选）：`ASSESSMENT.md`、`MINISESSION_INTEGRATION.md` 移 `docs/`（GS-X-6）。二者文首已标注快照日期+基线+状态，**已满足「标注」选项**，移 docs/ 仅加强收敛 | GS-X-6（无门控） |

> **Phase 1 验收重点**：① `make verify` 装有 golangci 全绿；② 模拟无 golangci 环境（PATH 剥离）验证 SKIP 路径**不 FAIL**；③ `GOOS=windows make build-cross` 交叉编译通过（产物丢弃）；④ 新增三个 `scripts/check-*.sh` 逐个 `sh -n` 通过且首行 `#!/bin/sh`；⑤ verify 链顺序为轻量前置（check-fmt → 行数闸 → 记忆闸 → build → vet → test → lint）。

### 3.2 已达标（范本，声明不改）

- **GS-V-1/3** 版本注入 + `-s -w` strip：`Makefile:7` 已 `-ldflags "-s -w -X main.version=… -X …/miniagent.Version=…"`（双符号注入为库暴露所需，登记说明）。
- **GS-X-3** CHANGELOG：keep-a-changelog + `[Unreleased]` + SemVer（855 行，逐条可溯源）。
- **GS-T-1/2/4** 测试：纯 stdlib testing + 表驱动 + `-race`；不设覆盖率门槛。
- **GS-DEP** 零依赖、**GS-REUSE** 零跨仓 import：已达标（范本）。
- **GS-E-1/2/5** `/api` 前缀 + `{"error":string}` 信封（实测 `cmd/miniagent/web_config.go`）+ snake_case tag：已达标。
- **GS-E-4 ctx 纪律**：远程 session Client 六方法全带 `ctx`（实测）；本地文件存储非 DB，无需改造。
- **GS-X-4** 发版门禁：本仓即范本，`release.sh` 无需改（仅 P0-5 注释步数）。
- **GS-L-1/3** 日志阵营：`log/slog` 直用（实测 30 个 .go 文件 import `log/slog`，非测试代码**零 stdlib `log`**），TextHandler→stderr，无全局 logger（逐层参数注入），stdout 保留为 NDJSON 机器契约不受影响。GS-L-2 级别入 config 是 Phase 2（L1）。
- **GS-M-1 双轨制（达标）**：verify 第一步 `gofmt -s -l` 判空（检查模式，Makefile:12-13,20），独立 `fmt` 保留 `gofmt -w` 显式格式化入口——与 GS-M-1 定稿的双轨制一致（范本仓 minidocman/minidbman/herdr-bridge 同款）；不变量「verify/ci 链绝不改写工作区」本仓已满足，fmt 无需动。
- **GS-M-5**：无伪 CI target（无 `make ci` 类写副作用排除 test 的 target）。
- **GS-G-1**：`.gitignore` 41 行手写精简（非 mega-template），D3-b 若通过也无瘦身需求，仅低优先补 `logs/`、`*.log`（P0-9）。
- **GS-D-1/3**：`.service.tpl` + envsubst + `grep '\${'` FATAL（deploy.sh:55）+ `.env.example`/`.env` 双层——部署模板机制本身是范本级（缺的只是 GS-D-2 加固段 / GS-D-6 探活 / GS-D-7 回滚，见 §3.1 的 DPL-1/2/3）。
- **GS-D-5 未达标**（实测，补测项）：deploy.sh **无** `/dev/urandom` 种子密钥、无 `CHANGE_ME` 拒绝逻辑，`config.example.json` 带 `sk-xxxx…` 占位直接播种——若部署时不手改，服务将以占位 key 启动（WebUI 端由 `requireAuth` 保护，但 LLM 出站不可用）。归入 Phase 1 部署收敛，与 DPL-2 同批：探活仅验「活着」，需另做占位 key 拒绝（deploy.sh 播种时 urandom 生成或启动时校验非占位）。

## 4. Phase 2 —— 架构级改造（量化改动面 + 排期 + 前置）

| 项 | 内容 | 实测改动面 | 前置条件 / 建议排期 |
|---|---|---|---|
| L1 | **日志级别入配置**（GS-L-2）：config 增 `log.level` 四值枚举，CLI flag 优先（GS-C-2 CLI>config）；使部署 unit 免改 ExecStart 即可调级 | config 包 1–2 文件 + `cmd/miniagent/main.go` 1 处 + 校验/测试；约 10 处 | **先**用表驱动测试固化现有 `-log-level` 行为再改；建议 Phase 2 首项（日志是其余改造前置） |
| C1 | **`${VAR}` 展开**（GS-C-3）：本仓 config 层 env-free 是**设计**；secret 已由 `MINIAGENT_API_KEY`/`MINIAGENT_WEB_KEY` 等 env 注入，config `${VAR}` 基本冗余。**建议登记豁免**；若组强求，`config/config_load.go` 加 expander（空/未设 fail-fast 指名变量）+ 测试 | 1 处 + 测试（若做） | 需裁定；若做排 Phase 2 尾 |
| C2 | **首跑引导**（GS-C-5）：默认路径缺失本仓是硬错误（`setup.go` requireConfig）——对必配 provider key 的服务这是正确行为（无配置无法运行）。**建议登记豁免** | 0 | 需裁定；倾向不改 |
| R1 | **README 拆分 ≤300 行**（若 M2 采用 md 桶）：536 行拆 2–3 份（README 主 + `docs/config.md` 等） | README 1 文件拆 2–3 + 引用更新 | 需先评审文档结构；若 M2 已豁免则不必要 |
| L2 | **（可选）internal/looptest 外移**：若组强求「internal/ 全空」才做；本仓倾向保留（测试辅助包，CONTRIBUTING:35 已明示，README 声明仅指非测试核心包） | 外移 public 包 = import 路径改 1 测试文件 | 低优先，倾向不做 |

> **Phase 2 前置校验**：① L1 改前先以表驱动测试锁定 `-log-level` 现有四值行为（debug/info/warn/error，非法 exit 1）为回归锚；② C1 若做 expander，必须先配「空/未设 fail-fast 指名变量」测试；③ R1 拆分前先评审 README 结构，确认 536 行中哪些章节可独立成 `docs/*.md` 且不破坏入门引导；④ 全部 Phase 2 改动逐项 `make verify`，L1 单独提交（日志是其余改造前置）。
>
> **范围底线**：Phase 2 本仓不做的事——不引入 CI/容器/监控/APM（D4-a 红线）、不改 `.gitignore`（D3 待授权）、不设覆盖率门槛（GS-T-4）、不建公共日志/配置 wrapper（GS-L-1/GS-C-1 直用）。

## 5. 实施顺序与提交切分

原则：**每类一次提交、每次只改一类**，逐类跑 `make verify` 全绿后提交（遵守 AGENTS 红线「每行改动可溯源」）。

1. **P0 文档/卫生批次**（单提交或按需分 2–3 个）：P0-1/2（清理，无代码）→ P0-3（version 值）→ P0-4/5/6（步数文档）→ P0-7（session.dir 协同）。每步后 `make verify`（P0-1/2 清理类无需 verify，仅 `git status` 干净检查）。
2. **P0-7 前置**：改 `config.example.json` 与 `deploy/deploy.sh:71` 须**同一提交**（两处协同，破坏性若拆开则运行分叉）。验证：`grep` 双文件一致 + `make verify`。
3. **M1 Makefile 新范式**（单类大提交，独立评审）：抽脚本→golangci 可选→增 target。验证：`make verify`；`command -v golangci-lint` 时 lint 跑、缺时 SKIP 不 FAIL。
4. **部署收敛**（同一部署类提交组）：DPL-1 加固 + DPL-2 `/api/health`+探活 + GS-D-5 种子密钥/占位拒绝 + DPL-3 回滚；N1 移 config.example。验证：`make verify`（不含实际部署；unit/脚本改动建议在有部署环境时 dry-run `sh -n`）。
5. **D2-1 / X1**（各自单类小提交，D2 待裁定）。
6. **N2 归档**（若做）。
7. **Phase 2**：L1（日志，先测试固化）→ C1/C2/R1/L2 按裁定排期。

验证命令（本仓现有）：`make verify`（gofmt 空 / build / vet / test -race / lint / 行数 / 记忆完整性）；`make test`；文档类改动用 `grep -n` 定向核对；脚本类加 `sh -n <file>`；交叉编译 `GOOS=windows go build ./...`。

提交信息遵循 AGENTS.md 编码标准：subject ≤72 字符、祈使句、无句号、一次一事；每类提交前给 diff 摘要待审阅（行为红线「提交前必须给 diff 摘要待审阅」）。

## 6. 验收清单

- [ ] `cmd/miniagent/.miniagent/sessions/` 空、`bin/` 无残留，`git status` 干净。
- [ ] `make verify` 全绿（本仓现可离线通过，golangci 本机已装）。
- [ ] `miniagent/version.go` 回退值为 `"dev"`；非 ldflags 构建 `-version` 报 `dev`。
- [ ] `README.md`、`release.sh`、`CONTRIBUTING.md` 的 verify 口径均为**七项**，与 AGENTS.md:15 一致。
- [ ] `config.example.json` 的 `session.dir` 与代码默认 `cmd/miniagent/session.go:24` 一致，`deploy/deploy.sh` sed 同步。
- [ ] Makefile 为标准 target 集 + golangci **可选**（缺装 SKIP 不 FAIL）+ `vet`/`help`/`build-cross` 齐备。
- [ ] deploy 收敛项（加固段 / `/api/health` 探活 / `.prev` 回滚）落地并有脚本语法/逻辑自检。
- [ ] `CLAUDE.md` 单行 `@AGENTS.md`；「禁止提交」约束已并入 AGENTS.md 红线。
- [ ] 涉 `.gitignore` 的改动（P0-9）已获 D3 授权并在 CHANGELOG 记录。
- [ ] verify 链顺序为轻量前置；无 golangci 环境（PATH 剥离模拟）verify 不 FAIL（SKIP）。
- [ ] `make build-cross` 交叉编译通过（5 个 `*_windows.go`）；新增 `scripts/check-*.sh` 均 `#!/bin/sh` 且 `sh -n` 通过。
- [ ] 部署首装不再以占位 key 播种：`config.example.json` 占位被替换或启动被拒（GS-D-5）。
- [ ] `internal/looptest` 处置已裁定（保留登记或外移），README:38 措辞与 CONTRIBUTING:35 口径一致。

## 7. 调查 quirks 覆盖对照（`/tmp/wf/miniagent.md` §7）

| 存档 quirk | 处置 | 本方案条目 |
|---|---|---|
| 1) 零 CI，门禁纯本地 | 已达标（D4-a）；不引入 CI | §3.2 |
| 2) git-ignored 产物（bin / sessions / .env） | sessions 15 个、bin 8.1MB 本地清理；`.env` 为本机覆盖保留（非跟踪无泄漏风险） | P0-1/P0-2 |
| 3) deploy/.env 缺 MINIAGENT_SESSION_DIR 字段、分层漂移风险 | `.env.example` 已兜底（实测含该键），功能正常；漂移风险随 N1 收敛 | P0-7 / N1 |
| 4) README「五步」vs 实为七步 | 修 README；**并补 release.sh:29、CONTRIBUTING.md:8-15 两处同源过时** | P0-4/5/6 |
| 5) config.example session.dir 与代码默认分叉（deploy sed 暴露） | 对齐示例 + 协同改 sed；**实测优先级：config.session.dir 非空时覆盖 env 默认**（main.go:121、web_sessions.go:57），故非部署本地运行也受影响（比「仅部署暴露」更广） | P0-7 |
| 6) version.go 报 v6.6.6 而非 dev | 回退值改 `"dev"`（GS-V-2 同列项） | P0-3 |
| 7) internal/ 仅剩 looptest 尾巴 | CONTRIBUTING:35 已准确说明（第四轮已修）；仅 README:38 措辞澄清；外移为可选（倾向保留） | P0-8 / L2 |
| 8) 注释中英混杂 | **不处理**——非门禁项、组不统一语言；登记为已知风格差异 | — |
| 9) deploy.sh 单向、无卸载/回滚 | 回滚经 `.prev`+探活（DPL-3）；卸载脚本非必要（可选增） | DPL-3 |
| 10) .gitignore 缺 logs/、*.log、config.json | 当前无日志文件、config 不入仓，**无暴露**；补忽略项挂 D3 待授权 | 🔒P0-9 |

### 7.1 本次实测对调查存档的修正（survey_corrections）

- `.miniagent/sessions` 实测 **15 个**（存档 §7 写 16；组报告 §五 已写 15，以此为准）。
- `bin/miniagent` 实测 **8.1MB / 8,147,236 B**（存档 §7 quirk 2 写 7.8MB——为发版后重建的产物，尺寸随构建波动，仅记录无定性影响）。
- 「五步」过时措辞不止 README:68——`release.sh:29` 注释与 `CONTRIBUTING.md:8-15` verify 块同样只列 5 步（CONTRIBUTING 缺「行数上限/记忆完整性」）。
- `internal/looptest` 与 README「核心包移出」**并非实质矛盾**：CONTRIBUTING.md:35 已明示「internal/ 仅剩测试辅助包 looptest」，README:38 指非测试核心包；仅可加注措辞，非残留尾巴。
- session.dir 分叉的运行时影响比存档描述的「deploy.sh sed 暴露」更大：`config.session.dir` 非空即覆盖 env 默认（main.go:121、web_sessions.go:57），非部署本地运行也会启用示例的 `.sessions`。
- 存档 §4 只报 README「五步」；本方案将同源过时扩至 release.sh:29 与 CONTRIBUTING 两处（P0-4/5/6），与组报告 §五 miniagent 行一致。
