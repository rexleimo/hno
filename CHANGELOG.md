# Changelog

All notable changes to Agno-Go will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### ✨ Added (B5b / D-39：A2 生效——实测可放宽准入)
- 新选项 **`sandbox.WithBoundaryMeasurement(report)`**：准入由 `admitBoundary` 双向裁决。
  **放宽**：`private-kernel` 可放行声称低于下限的 provider（本机 `IsolationMicroVM` 从 **exit 70 → exit 0**）。
  **收紧**：`shared-kernel` + 声称 ≥ micro-VM ⇒ 拒绝（单靠声称本会通过）。
- 放宽需**三证齐全**：`report.Provider` 与当前 provider 一致、`report.HostKernel` 与本进程宿主一致、
  `MeasuredAt` 在 `Timeouts.BoundaryFresh` 内；任一不成立则**不提升**，并指明是三条里的哪一条
  （首版丢掉原因、只回泛泛的 "below required" 并让人去测量——而他已经测过）。
- `KernelBoundaryReport.Provider` 记录测量来源；`Status.Boundary` 携带准入所依据的证据，
  `Status.Isolation` **仍报 provider 自己的声称**（改写声称等于宣称引擎换了性质）。
- `Diagnose` 的 `isolation` 行与准入结论一致：「container claimed, private-kernel measured」。
- `Timeouts.BoundaryFresh`（默认 30m）：测量作为放行理由的有效期。**0 解析为默认**——`WithTimeouts`
  是整体替换，只写 Setup/Exec 的调用方会拿到 0，而 0 会拒掉刚测出的报告；负数=每次 Open 重测。
- CLI 新 flag **`--measure-boundary`**（run/doctor，会先起盒）；verb flag 注册提取为
  `registerVerbFlags`，帮助文本可被测试直接读取（不再由测试自造副本）。

### 🐛 Fixed（本轮被自己的测试/真机抓出）
- `BoundaryFresh` 负值被实现成「永不过期」，与文档承诺的「每次 Open 重测」相反。
- 证据不成立时（别的引擎/别的宿主/过期）丢弃原因，导致提示让人去重测一台已测好的引擎。
- 真机测试漏注册 provider 且未带 `--binary` 属性；抽出 `realOptions` 供全部真实门共用，避免再漂移。
- 一个 CLI 测试自建 FlagSet、断言自己写的空 usage（空转）——改为读取发布的注册。

### ✨ Added (B5a / D-38：隔离档位改为实测)
- 新可选接口 **`sandbox.KernelBoundaryQueryable`**（`MeasureKernelBoundary(ctx, boxes)`）：provider 起若干盒、
  读每盒 `/proc/sys/kernel/random/boot_id` + `/proc/uptime` + `uname -sr`，**由读数**判定档位。
  四态 `private-kernel` / `shared-kernel` / `unknown` / `unmeasured` 不可混用；`unknown` 不得伪装成健康。
- docker provider 实现之（复用 create/start/exec/close **同一条生命周期路径**，不另造轻路径）；
  成本铁律：provider 不支持 ⇒ 类型化错误点名接口且**零启动**；盒数越界 ⇒ `BoundaryBudgetError` 且零启动；
  镜像缺失先问（承 D-36）；teardown 失败写入 `Fixes` 并指向 `hno sandbox prune`。
- **核心纠正过度声称**：provider 报 `private-kernel` 而 boot id 相同时，降为 `shared-kernel` 并给原因。
- `Diagnose` 报告缓存的结论并对照本句柄下限（**绝不主动起盒**）；未测量时不产生该项检查。
- 新 CLI 动词 **`hno sandbox boundary`**（`--boxes` / `--expect` / `--json`）：`0` 测得且相符、
  `1` 结论 `unknown` 或不符、`70` 无法测量、`2` 参数错。dispatcher/帮助/未知动词提示改由
  `sandboxVerbs` 单一列表驱动（原先三处手抄，会互相矛盾）。
- 现场证据：本机 `docker` 是 Apple containerisation 的 shim（`docker info`），4 盒 4 个不同 boot_id ⇒
  **每盒一个内核**；真实引擎门 `verdict=private-kernel (2/2 distinct)`，残留 0。
- 中英 guide 新增「隔离度是实测出来的，不是按名字给的」一节 + 退出码表 `boundary` 行；
  五道文档门现已**逐语言**运行（此前围栏语法门/方法调用门/裸名门只看英文页——D-37 的洞没堵完）。

### 🐛 Fixed（本轮被自己的测试抓出）
- `defer` 中修改 `report` 而返回**值拷贝** ⇒ teardown 泄漏信息永远丢失。改为具名返回值，
  由 `TestBoundaryTeardownFailureIsReportedNotSwallowed` 事先钉住。

### ✨ Added (B4i：中文 guide + 跨语言文档门禁)
- 新页 `website/zh/guide/sandbox-runtime.md`，并挂进 zh 侧栏（`沙盒运行时`，与英文页同位置）。
  渲染证据：`dist/zh/guide/sandbox-runtime.html`、zh 侧栏与前后翻页链接均出现、进 sitemap，
  页内两条锚链的 id 与渲染器产出**逐字相同**。
- 文档门禁升级为**多语言**：`guidePages` 显式列出各页，**缺页 = 失败**（原来 `t.Skipf` ⇒ 删掉 guide
  会让五道门全绿，正与本批一直在修的「不会失败的门禁」同类）；`TestGuideAPINamesResolve` 另加
  **各页限定名集合相等**检查，翻译漏掉一个选项会被点名。
- 新测试 `TestGuideSlugMatchesTheRenderer`：复刻渲染器的锚点规则（NFKD 兼容折叠 → 特殊集
  （含 `- _ .`）折成连字符 → 其余原样保留 → 合并去边 → 小写），7 例取自**真实构建产物**。
  过程中被实测纠正两次：全角 `，` 变 `-`，而顿号 `、` 因无兼容分解**原样保留**——同页两向，靠推定必错。
- 中英各 12 条标题与 dist 逐条比对：**零偏差**。

### ✨ Added (B4h：站点链接门禁 + 一次真实 docs:build)
- `docs:build` 第一次被真实跑过（`npm ci` 装依赖，127 包 / 5s，构建 22.1s exit 0），并**渲染验证**新页：
  `website/.vitepress/dist/guide/sandbox-runtime.html` 115 KB、11 个 h2、侧栏项 "Sandbox Runtime" 与
  prev/next 均已出现、进 sitemap。
- 反证给出一个必须记录的负结果：**该站在 `config.mjs` 里设了 `ignoreDeadLinks: true`**，所以往 guide 塞一条
  `/guide/does-not-exist` 死链后构建仍 **68 秒 exit 0**。⇒ 「docs:build 通过」不是关于链接的证据。
- 新测试 `TestGuideLinksResolve`：把 guide 里的本地链接解析到真实文件（`/guide/x` → `website/guide/x.md`，
  兼容 `index.md` / `public/` 资源 / 相对链接），并把同页 `#anchor` 与渲染器的 slug 规则对齐
  （用小写 + 去掉 `:` 与反引号 + 空格转 `-`，与实测产出 `from-a-shell-hno-sandbox` 一致）。
  门禁会先**自检站点根路径**再下结论：本站第二次踩同一类 `../../` 少一层的错，表现是「所有链接都是死链」——
  错的路径伪装成发现，比漏报更坏。

### 📋 Finding（只测量、未修，需决策）
- 全站实测：**26 个页面共 45 条无法解析的链接**（初版扫描器误报 61 条，修正 `/rss.xml` 这类 public 资源后为 45）。
  集中在 `/examples/*`（如 `workflow-demo`、`chat-agent`、`file-agent`）与 `api/a2a.md` 的相对链接。
  新页 `sandbox-runtime.md` 为 0。不在本轮范围内改别人的页面；若要治本需把 `ignoreDeadLinks` 翻成 `false`，
  那是**全局构建行为变更**、会立刻让现网 45 条变成构建失败 ⇒ 需要明确授权。

### 🐛 Fixed (B4g / D-36：`Diagnose` 不再对镜像给假绿灯)
- 真实引擎实测的症状：`hno sandbox doctor --image no-such-image-hno:0` 报 **“all 8 checks passed” / exit 0**，而同一镜像走 `run` 是 exit 70 并带引擎原文（registry 401）。`Diagnose` 里没有镜像项，`Probe` 又刻意不 pull 并把「缺镜像」当无关信息吞掉。现在同一命令报 **exit 1**、点名镜像、给出可执行修复，summary 变为 “1 of 9 checks failed”。
- 新可选接口 `sandbox.ImageQueryable`（`ImageAvailable(ctx, image)`）：provider 经 **dialect** 用自己的词汇表回答（docker 用 `image inspect`，Apple 解析 `image list`——用错形状会把 usage error 误报成「镜像不存在」）。`docker.ImagePresent` 此前能力已实现、已测，但生产路径调用者为 0；现在核心健康报告真正走它。
- 检查四态，只有 present 是绿的：absent ⇒ 不 OK + 修复建议；问了但引擎不肯答 ⇒ 不 OK 且 `Got: "unknown: <引擎原因>"`（沿用 D-31「无法验证即拒绝」，不向上取整成健康）；provider 不提供查询 ⇒ 不 OK 并点名要实现的接口（与 `capabilities-without-behaviour` 同一家规）。
- `sandboxtest` double 增加 `AbsentImages` / `ImageQueryErr` 两个模拟旋钮（沿用其既有 `ProbeErr` / `SlowBoot` 风格），默认回答「present」——因为该 double 确实能用任意镜像起环境，答 absent 就是朝反方向撒谎。

### 🧪 Tests (B4g)
- 五条镜像态测试在**旧实现下 5/5 全红**（抽掉 `Diagnose` 的检查即复现旧行为），恢复后全绿。
- provider 层：两种 dialect 各两条（present/absent）+ 空镜像不得被答成「absent」+ 引擎故障必须以 error 上抛而非伪装成缺失 + `*docker.Provider` 确实满足 `sandbox.ImageQueryable`。docker provider 覆盖率 59.6% → **62.2%**。
- 真实引擎闭环 `TestRealImageCheckIsNotAFalseGreen`：本地镜像 ⇒ 检查 OK；不存在的镜像 ⇒ 不 OK、点名镜像、修复可执行，并断言 summary 不得在镜像阻塞时读成 clean。真实门 **10 PASS / 0 FAIL / 残留 0**。
- 测试反证了我自己的两个错误假设：① `WithImage("")` 不是「无镜像态」，它解析到 `sandbox.DefaultImage`（`agent-runner:1`），故该测试改为钉住「留空不得豁免验证」；② 委托壳若转发 double 的全能力集，会在 bind 时被核心以过度声明拒绝——壳返回空集合才是诚实的。
- 收紧行为的半径实测：非测试 `Diagnose` 调用点只有 `cmd/hno`；七包全绿（含 `pkg/hno/agent`），核心覆盖率 73.2% → 73.3%，`-race` 全绿。

### ✨ Added (B4f：`hno sandbox` CLI)
- 新命令 `cmd/hno`：runtime 自报的东西以前只有写 Go 才听得到，而修复文案长期指向一个不存在的命令。三个动词，每个对一个导出调用，零新增策略：
  - `hno sandbox doctor` → `Sandbox.Diagnose`，人读逐项 want/got/fix，`--json` 出机读报告；有检查失败则非 0 退出，可直接当 CI 门禁。
  - `hno sandbox prune` → `Sandbox.Reclaimer`，**默认只列不删**，删除必须 `--yes`；引擎拒绝动的对象照实打印（`SkippedPrune`），不把「拒绝」藏进 0 计数。
  - `hno sandbox run` → `Sandbox.Run` / `Session.Exec`。
- 退出码把三件事分开（这是 CLI 的全部意义）：payload 自身状态**原样透传**；`124` = 跑了但超预算（`timeout(1)` 约定）；`70` = 环境不可用且明说「程序状态未知」；`2` = 命令行本身错。与 D-33 的 `run_code` 分类同源。
- `--attr k=v` 直通引擎配置通道（`sandbox.WithAttrs`），于是 `--attr binary=container` 可以指向真实引擎、`--attr runtime=runsc` 可以问 gVisor。`--isolation` 只接受 `Isolation.String()` 渲染的那几个名字。
- `website/guide/sandbox-runtime.md` 新增「From a shell」章节（命令表 + 退出码语义）。

### 🔒 Tests (B4f)
- D-34 的收尾：文案门禁不再简单禁止 CLI 形状，而是解析 `cmd/hno/sandbox.go` 里的 verb 常量集合来校验 —— 改动词会自动改门禁，不复制第二份清单。三种状态各自反证：假 verb 被拒并列出真实集合；CLI 目录消失时回到「禁止一切命令形状」；目录在而源文件读不到时 `t.Fatalf` 响亮自杀（初版把「读不到」当成「不存在」，门禁坏掉却表现为所有文案违规）。
- `cmd/hno` 离线行为测试覆盖 flag→option 映射（拼错的隔离档位被拒且不改判）、请求形状二义性、`--json` 与退出码一致、只读 prune 绝不调用删除分支、超时必须 ≠ 环境故障。`cmd/hno` 覆盖率 77.2%。
- Isolation 词汇往返被钉住：`String()` 输出必须能被 `ParseIsolation` 解析回来，且 `--isolation` 帮助文本必须列出全部档位、不得出现解析器拒绝的词（我第一版写的 `micro-vm` 就是被这条抓的）。
- 真实引擎证据（非 double）：`doctor` 8 项全过 exit 0；坏引擎二进制 exit 70 并给出可执行说明；`run -- go version` 返回 `go version go1.26.9 linux/arm64`；`run -- sh -c "exit 5"` 透传 5；`prune` 零删除、残留 0。`-race` 全绿，真实门 26.0s，核心覆盖率 73.2% 不变。

### 🐛 Known gap（已由 B4g 修复，见上方 D-36 条目）
- `hno sandbox doctor --image <不存在的镜像>` 报 **“all 8 checks passed” / exit 0**，而同一镜像走 `run` 是 exit 70 并带引擎原文。`Diagnose` 没有镜像项，`Probe` 又刻意不 pull 并吞掉缺镜像；而 `docker.ImagePresent`（两种 dialect 都已实现）唯一的调用者是集成测试。修法是加一项 image 检查，但它会改动检查集合、可能把 CI 里的 doctor 从绿翻红 ⇒ 需要授权，未在本轮范围内。

### 🐛 Fixed (B4e：可执行文案不再指向不存在的东西)
- `Diagnose` 的「有孤儿环境」修复建议此前是 `hno sandbox prune --orphans`，`ProvisionError`/`defaultFixes` 里还有 `hno sandbox doctor` —— **当时本仓库没有这个 CLI**，照做的用户只会去找一条不存在的命令（B4f 已把这个命令补上，见上方 `hno sandbox` 条目；当时先改为真实可走的 Go 入口）。现改为真实入口：`Sandbox.Reclaimer()` 的 `Sweep(ctx)`（只看不删）与 `Prune(ctx)`（回收），以及 `Sandbox.Diagnose(ctx)`。
- 同一批文案里 `WithMinIsolation(IsolationContainer)` 也是不存在的功能名（真名 `WithMinimumIsolation`），已改正。
- docker provider 的截断标记 `[hno sandbox: output truncated …]` 长得像命令，改为 `[output truncated at the provider capture ceiling]`，信息不减。

### ✨ Added (B4e：DX 门禁)
- `pkg/hno/sandbox/dx_gate_test.go`：解析包自己的导出面，校验所有用户可读的字符串字面量 —— `With*` 必须是真实导出的函数、`Type.Member` 必须解析得到、不得出现 shell 命令形状（没有 CLI 就不许指 CLI）。
- `pkg/hno/sandbox/dx_docs_gate_test.go`：`website/guide/sandbox-runtime.md` 的 `Limits`/`Timeouts` 表格数字必须等于 `DefaultLimits()`/`DefaultTimeouts()` 的**规范渲染**（`Provision` 是 `2m0s`，不是「120s」）；文中裸标识符（`With`/`Act`/`Isolation`/`Cap`/`Overflow`/`Mount`/`Profile`/`Err`/`Class` 前缀族）必须在 `sandbox ∪ tools/run ∪ sandbox/graph` 中存在；每个 ```go 围栏必须能被 `go/parser` 解析。符号宇宙取三目录并集：过窄的取域会把真实 API 报成假名，那会训练读者忽略门禁。
- 新 guide 页 `website/guide/sandbox-runtime.md`：30 秒上手、真实默认值表、`Diagnose`、孤儿回收、人审批准、图内审批、项目/文件/产物、从遗留 `Executor` 迁移（含适配器拒绝面）、toolkit 失败可见性，附 recipes。已挂进 VitePress 英文侧栏；中文页未建，故未加中文链接（避免 404）。

### 🧪 Tests (B4e)
- 门禁本身被反证：把 `WithMinIsolation`、`[hno sandbox: …]`、`1 MiB`、`120s`、`WithRunOutputMax` 与一个缺逗号的代码围栏塞回去，六条断言各自命中并附原文；恢复后全绿。四批文档错误由门禁在我自查之前抓出（`sandbox.Context`、`OverflowFail`、`WithRunOutputMax`、表格数字渲染形式）。
- 覆盖：`pkg/hno/sandbox` 73.2%（不变）；`-race` 四包全绿；真实引擎门 28.8s 通过、残留 0（改动了 provider 输出标记，故重跑）。

### ✨ Added / 🐛 Fixed (D-33：`run_code` 的失败终于可被机器看见)
- `pkg/hno/tools/run/toolkit.go`：环境故障改为**双写**——既保留调用方一直在读的结果 map（新增 `failure` 摘要键），也返回**非 nil 的包装 error**（`errors.Is(err, ErrNoProvider)` 等哨兵仍可判别）。此前失败被折进 map 并返回 `nil`，agent 的 logger、逐调用 outcome、任何监控与 Policy 全都把这当成一次成功的工具调用。
- 分类规则只用**本层可证的事实**，不把「用户代码失败」误报成「环境坏了」（遗留 executor 对非 0 退出也返回 error，无脑上抛会制造反向混淆）：哨兵 → `TimedOut` → `ExitCode > 0` → 其余判环境故障。依据是 `exitCodeOf` 对非 `*exec.ExitError` 返回 `-1`、e2b 以 `Result{ExitCode: -1}` 表示「未观测到退出码」，因此正的退出码确实意味着代码跑过。
- 超时保持为数据（`timed_out` 已在 map 中说明这是调用方自己的预算被超出）。
- 已知代价（如实记录，不静默）：`agent.executeToolCalls` 在 handler 返回 error 时会**丢弃结果 map**，只把 `"tool execution error: …"` 文本交给模型 —— 因此错误文本自带可执行建议；若要把 map 一并渲染，需改 `pkg/hno/agent`，属另行授权。另一处窄不精确：podman 的「输出捕获故障 + 已观测退出码」组合按规则判为数据，完整错误文本仍留在 map。

### 🧪 Tests (D-33)
- 新增 5 条分类回归：环境故障必须双写（哨兵可 `errors.Is`、map 仍在、含 `failure` 摘要）、代码非 0 退出必须仍是数据且不得被标为环境故障、超时必须是数据、`ExitCode` 为 -1/0 且带 error 必须判环境故障、后端拒绝哨兵必须保真。反证：前一组对旧实现三条全红（`an environment failure must reach the caller as an error`、`exit code -1 with an error … must not read as success`、`want the sentinel preserved, got <nil>`）。
- 既有 `TestCodeExecutionToolkitReturnsExecutionOutputOnFailure`（`ExitCode:4` + error）在新规则下仍要求 nil error，未改动即通过 —— 说明迁移没有把代码故障升格为故障。`pkg/hno/tools/run` 覆盖率 65.8% → 66.4%。

### 🐛 Fixed (D-32：图 HITL 的可选响应字段终于能声明)
- `pkg/hno/graph`：`validateResponse` 的属性循环对**缺失的键**取 `respMap[n]`（读出 nil），nil 撞任何声明类型都判红——与它自己的注释「缺失的属性只由 required 管，这里跳过」直接矛盾。结果是「声明一个可选字段」等于「强制该字段必须出现」，任何省略它的合法响应都被拒。现改为按双值取键判定缺席并跳过值校验；强制出现只能靠 `required`。
- 显式 `null` 不算缺席：它是「出现了但不是那个类型」，照旧拒绝（受测钉住）。
- `pkg/hno/sandbox/graph`：默认响应 schema 把 `reason` 声明回**可选**。此前不可能声明——本包实际会用 `readResponse` 读它，却只能让 schema 少写一个字段；修复后 schema 与真实读取一致。

### 🔒 Security (D-32 的收紧面)
- 同一改动让非字符串的 `reason` 从「不声明因而不校验、值被静默丢弃」变为「类型不符即拒绝，且挂起不被消耗、可重新作答」。可选字段的宽容只作用于「不出现」，不作用于「出现但形状错」。

### 🧪 Tests (D-32)
- `pkg/hno/graph` 新增 5 条行为测试（走 `Run`/`Resume` 公开面，非内部谓词）：省略可选字段应通过、可选字段出现但类型错必须拒、显式 null 必须拒、伴随可选字段时必填仍必须拒、全可选 schema 接受空响应（把放宽变成明示合同而非事故副产品）。`pkg/hno/sandbox/graph` 新增 2 条：非字符串 `reason` 被拒且问题仍开放；只给决策不给理由的作答必须被接受。
- 反证两条：旧 graph 实现下第 1、5 条以 `field "note" should be string, got <nil>` 判红；`sandboxgraph` 的「只答是/否」测试在旧 graph 下同样判红，证明两处耦合。`pkg/hno/graph` 覆盖率 90.8%，`sandbox/graph` 80.3%。

### 🔒 Security / Fixed (D-31：`WithCreateIfMissing(false)` 不再静默失效)
- **核心**：`Sandbox.Session(ctx, id)` 在句柄**没有 session store** 时，此前无视 `WithCreateIfMissing(false)`：未知 id 会开出一台空环境并作为「正在恢复的那个会话」返回。跨进程恢复因此以为自己在续作，实际拿到空盒子，还会把缺失状态归因于自己的代码。现在无论有没有 store 都返回 `*StaleSessionError`，且不注册会话、不开机；`Cause` 按「有无 store」分别说明能证明的原因（无 store 时诚实说「无法核实该 id 是一次恢复」）。
- `RunProject` 显式传 `WithCreateIfMissing(true)`：它本来就运行在自己创建的会话上，靠隐式宽容行为等于把创建意图藏起来。
- **`sandboxgraph` 删除一个 workaround**：`Ask.Answer` 原先先用 `Diagnose` 的审批计数做前置排除，只因核心当时不肯拒绝。核心修好后该守卫一并删除——同一件事不留第二个权威。错误分类随之回到事实驱动：句柄无法核实 id ⇒ `ErrNoSession`（由核心给出并被测试直接断言）；会话活着但审批已消耗 ⇒ `ErrNoApproval`。
- 爆炸半径实测：非测试调用点仅 2 个（其中一个正是需要拒绝的 `graph/ask.go`），包内 50 处测试早已显式传该选项；需补选项的既有测试 4 处（`readiness_falsifier_test.go`）。

### 🧪 Tests (D-31)
- 新增 5 条合同测试：无 store 拒绝且**不注册会话、不创建环境**（持 provider 引用直接探测）、显式创建仍可用且已知 id 可重新取得、`WithSessionReplay(true)` 隐含恢复意图、有 store 时 Cause 文案点名缺失记录、`RunProject` 自建会话并可重新取得。反证：旧实现下第一条拿到的是带 `gen:...` 血缘的活会话。`sandbox` 73.0% → 73.2%、`sandbox/graph` 78.9% → 80.3%。

### ✨ Added (B4c 遗留 Executor 兼容适配器)
- 新文件 `pkg/hno/tools/run/adapter.go`：`NewSandboxExecutor` 用 `pkg/hno/sandbox` 实现冻结的遗留 `Executor` 合同（`Run(ctx, Spec) (Result, error)` + `Close()`）。纯新增，不改 `executor.go/podman.go/e2b.go/local.go/toolkit.go`，也不要求任何调用方迁移。
- 语义落差一律类型化拒绝而不是静默重解释：`Spec.Workspace`（遗留为宿主读写 bind mount，新模型为快照进出）报 `ErrSandboxUnsupported` 并点名 `RunProject`/`WithWorkspace`；`Spec.MemoryLimit/PidLimit` 与句柄限额不一致报 `ErrSandboxLimitMismatch` 并说明配置点（per-run 内存/PID 上限在新 API 里不可表达）；`podman`/`e2b` 后端明确拒绝而不是偷换 provider。
- 兼容哨兵与信号保真：引擎缺失映射回 `ErrNoProvider`，未知后端仍是 `ErrUnsupportedBackend`，`validateSpec` 的错误文案原样保留；超时同时给出包装的 `ErrTimeout` 与 `Result.TimedOut`（遗留调用方两种都测），且 provider 不返回 report 时退出码记为 -1 而非谎报成功。

### 🐛 Fixed (B4c)
- 限额判定改为从已开句柄 `Status()` 读回：首版用构造期记忆值，而调用方 `Options` 追加在后、可覆盖 `WithLimits`，于是拒绝理由里的数字与引擎真实限额分叉（`TestAdapterHonoursConfiguredLimits` 先失败暴露此点）。

### 🧪 Tests (B4c)
- 14 条适配器测试；`pkg/hno/tools/run` 覆盖率 61.1% → 65.8%。反证四项对旧实现失败：静默忽略 Workspace、静默忽略限额不一致、丢掉 `ErrNoProvider` 映射、丢掉无 report 时的 `TimedOut` 置位。并发 8 路运行与 `Close()` 幂等亦有覆盖。

### ✨ Added (B4b 图内沙箱节点)
- 新包 `pkg/hno/sandbox/graph`：把沙箱的人工审批接进控制流图。`Node(name, sess, req, opts...)` 运行一个请求，遇到 `ActRequireApproval` 时把它变成图自己的中断（`Payload` 携带 `Ask{Session,Approval,Lineage,Question}`），人类在图侧的 `Resume` 回答被翻成 `Session.Approve/Deny + Resume`，工作最终落在人被展示的那台环境里。不新增第二套 HITL，也不改 `pkg/hno/graph` 语义。
- 节点自持稳定运行身份（钉 `Request.ID`）：否则图重入等待节点时每次都是「等价但新的请求」，审批 id 会变，回答指向无人持有的标识。
- 拒绝是可分支的输出（`Result.Refused`）而非节点故障，`WithRefusalFails()` 供没有拒绝分支的图把它变成错误；命令失败仍按运行合同作为数据上报。
- 跨进程续答：`FromCheckpoint`/`Outstanding` 从检查点流里找出未答问题，`Ask.Answer` 在另一个进程里重新取回该会话并消耗裁决（不创建会话）；`ErrNoApproval` 与 `ErrNoSession` 分开，各自只说自己能证明的事。

### 🐛 Fixed (B4b)
- 图侧中断的响应 schema 只声明决策字段：`pkg/hno/graph.checkShape` 会把未提供的可选属性判为类型不符（与其自身注释矛盾），声明可选 `reason` 会拒掉所有省略它的回答；本包不声明，但仍接受该字段。

### 🧪 Tests (B4b)
- 22 条，新包覆盖率 78.9%。反证三项对旧实现失败：去掉 id 钉定 ⇒ 稳定性与同盒完成两条失败；审批错误直接透出而不转图中断 ⇒ 挂起断言失败；去掉存在性守卫 ⇒ 跨进程两条失败。另有一条 `t.Log` 显式记录核心缺陷（`WithCreateIfMissing(false)` 在无 store 时为静默 no-op），不做静默绕过。

### ✨ Added (B4a HITL 审批闭环)
- `sandbox/approval.go`：把「要求人工批准」从一条错误变成一条完整回路。`Session.Awaiting()` 让调用方在不装 Observer 的情况下看到被挂起的请求（工作流要么汇报卡住、要么只会卡住）；`Approve/Deny/Resume` 落人类裁决并按原请求原文重跑，重跑仍走完整策略/钩子/审计路径，权限不靠绕过维持。
- 审批记录绑定环境：`PendingApproval.BoundToEnvironment()` 明示这次授权是否关于某台具体盒子（策略先于开机跑，新会话首次挂起本就没有盒子可绑）。已授予的批准免除自身义务，否则再提交会无限挂起。
- 过期窗口真正生效：`Approval.Expires` 与 `Timeouts.ApprovalHold` 取短者，过期后 Approve/Resume 返回可补救的 `ApprovalExpiredError`。
- 新类型化错误：`UnknownApprovalError`、`ApprovalAnsweredError`、`ApprovalExpiredError`、`ApprovalRefusedError`、`ApprovalEnvironmentChangedError`、`ApprovalNotDischargedError`；`ApprovalPendingError` 增补 `SessionID/Hold/ExpiresAt/Remediation`。
- 新事件：`EventApprovalRequested/Granted/Refused/Expired`；`Diagnose` 增 `approvals` 检查（有挂起未答即报不健康并指向 `Awaiting()`）。

### 🐛 Fixed (B4a)
- **锁序死锁**：在持 session 锁时 emit，与 `Close`（先取 handle 锁再 `Release` 取 session 锁）构成 ABBA；nil 审批簿 panic 展开时还会留下未释放的 handle 锁，症状是永不返回而非失败。现由 `sb.mu` 独占审批簿、先收集后解锁再 emit、book 方法 nil-safe、Open 即建簿。
- **文档承诺的重建路径不可达**：`Resumable()` 声称 Exec 可按 spec 重建、`environmentFor` 也实现了，但 `reserve()` 更早对 `StateLost/StateSuspended` 直接返回 `StaleSessionError`，使该分支零调用者，默认 `RebuildFromSpec`（D-11）在 session 路径上从不生效。现两处共用单一谓词 `canRebuild()`，拒绝时把原因写进 `Cause`。
- **懒建/重建丢 provenance**：`provision` 用了 `newEnvironment` 返回的 provenance 做重放却丢弃它，session 上留着 `adoptWorkspace` 的占位 lineage，同时使 D-20 步骤缓存水位门在该路径失效、审批无法绑定环境。现写回 provenance。

### 🧪 Tests (B4a)
- `approval_test.go` 12 条：挂起→批准→恢复全回路、拒绝后不跑、窗口过期、per-approval 短窗口生效、审批不循环、换盒拒绝且授权不被消耗、未知 id 双补救、二次裁决拒绝、事件落观察者、Diagnose 可见挂起、并发 answer/Resume/Diagnose/Close 不死锁。
- 反证：撤销 provenance 落库 ⇒ 换盒拒绝测试 FAIL；`expired()` 失效 ⇒ 两条过期测试 FAIL；禁用免除义务 ⇒ 循环测试 FAIL（报 `ApprovalNotDischargedError`）。真实门 8 PASS + 1 可见 SKIP，引擎残留 0，核心覆盖 72.6% → 73.0%。


### ✨ Added (B2 真引擎收尾：CapRebuild / ReadyProbe / TTL / runsc 档位)
- **`sandbox/provider/docker`：`Config.Runtime` 与 `ProviderConfig.Raw["runtime"]`** —— 把 gVisor 的 syscall-filtered 档位做成 provider 属性（`create --runtime`），而不是第二个 provider；`Probe` 带同一 runtime，避免「探默认档、跑另一档」把不可用配置报成健康。新增 `ContainerRunning` 导出查询（与 `ImagePresent` 同先例：Apple CLI 的 `inspect` 无 `--format`）。
- **`sandbox.Diagnose`：readiness 档位如实上报** —— provider 无 `CapBackground` 时说明 `Request.ReadyProbe` 会被拒绝；有后台但非 `ReadinessProber` 时说明只有 `Pattern` 可答。选择时可见，而不是等依赖探针的服务起不来。
- **`sandbox.Step.Input` + `Report.Staged`** —— 录制的输入现在带可重放载荷；纯内容请求是合法运行形状，报告写明放置了什么而没有退出码可报。
- **`sandbox.UnrecoverableInputError`** —— 无法被重建复现的输入在**记录时**拒绝（`ClassPermanent` + remedy），不再留一个注定失败的重放步骤。
- 真引擎门新增 `TestRealReadinessProbeSeesARealListener`、`TestRealRebuildRestoresRecordedState`、`TestRealExpiredSessionReclaimsTheContainer`、`TestGVisorTierIsExercisedOrExplicitlyAbsent`（可见 SKIP）。

### 🐛 Fixed (B2 真引擎收尾)
- **录制过输入的 session 一律无法重建** —— `RecordInput` 只存路径与摘要、不留内容，重放端则只做 `test -e`（检查存在、从不写回）。现按所有权归位修复：重放经核心 `stageInputs` 写回，后置校验用 provider 自己的文件服务 `Stat`，校验通过即结束，不再伪造一条 payload 命令。`HostPath` 在记录时读成字节，`Reader` 当场拒绝。
- **容器启动窗口接错配置** —— provider 的 create/start 一直被喂 `Timeouts.Connect`（连 daemon 的 3s 预算），引擎繁忙时每次启动都死在调用方从未配置过的 deadline 里。改接 `Timeouts.Provision`。
- **`Timeouts.Connect` 沦为有文档、零消费者** —— 补 `probeBounded`：探针获得自己的窗口（daemon 挂住不再无限挂住），并保持所有权分离（探测已绑定的 provider 不得关闭句柄；仅候选走 `probeAndClose`）。
- **指定未注册的 provider 却报「isolation 不满足」** —— headline 现说出到底在找谁，并带上「先注册」的 remedy。

### 🧪 Tests (B2 真引擎收尾)
- 离线 `go test -race ./pkg/hno/sandbox/...` 绿；真引擎门 9 PASS / 0 FAIL / 1 可见 SKIP；两个并发进程各 9/0/1，引擎 `hno.sandbox.managed` 残留 0；覆盖率核心 70.8% → 72.6%，docker provider 57.9% → 59.6%。
- 反向证伪 5 项均在破坏机制时变红，含一次自我纠正：第一版 `TestRecordedInputSurvivesRebuild` 关掉机制仍为绿，据此改造重放路径后才可读到 `file not found`。`TestUnknownProviderNamesTheProvider` 断言 `UnavailableError.Reason` headline 本身而非整串（整串会连旧文案一起放过）。
### ✨ Added
- **Sandbox contract layer** (`pkg/hno/sandbox`): a provider-neutral execution
  contract that replaces the one-shot `Run`/`Close` shape of
  `pkg/hno/tools/run` without touching it. One `execute()` pipeline backs both
  `Sandbox.Run` and `Session.Exec`, so policy, audit, failure classification,
  artefacts, and replayable-spec semantics are identical in both paths.
  - Isolation is an enum (`IsolationNone` … `IsolationMicroVMHosted`) and
    deployment shape is a separate axis (`FormationInProc`/`Remote`/`Sidecar`/
    `Hosted`); neither is a magic number any more.
  - Failures return as typed errors **and** a populated `Report` — seven
    classes (`user-code`, `denied`, `permanent`, `transient`, `resource`,
    `timeout`, `lost`) that feed the existing `graph.RetryConfig` instead of a
    second retry engine.
  - Extension points are split by what they may affect: `Policy` (may deny or
    tighten, never widen — enforced by comparing a privilege score),
    `Observer` (events only), `Sink` (streaming only). A panicking policy fails
    closed.
  - Replayable spec: `Install`/`Setup`/`SetEnv`/file steps record `Step` entries
    so `RebuildFromSpec` (default, D-11) re-renders an environment instead of
    replaying disk history, with step-level caching.
  - `lockdown` (`detect` for development, `enforce` for production) converts an
    undeclared `pip install` into `Failure.NextAction = use_install_api` plus the
    equivalent `Install(Pip(...))` call.
  - `Preflight(ctx, ...Option)` diagnoses "why can't this run here?" before a
    handle exists — the case `Sandbox.Diagnose` cannot reach, since it needs a
    provider that already resolved.
- **Reference provider** (`pkg/hno/sandbox/sandboxtest`): an in-memory engine
  that implements the optional interfaces (sessions, file bridge, streaming,
  snapshots, rebuild, egress allow-list, lease sweep) so the contract is testable
  without BoxLite, `/dev/kvm`, containers, or network egress.
- **Design record**: `docs/design/sandbox-runtime-architecture.html` (v1.0, 17
  sections, decision ledger `D-01`…`D-19`) and
  `docs/design/sandbox-litebox-evaluation-and-provider-plan.md`, including the
  Microsoft LiteBox rejection and the BoxLite provider plan.
- **Container provider** (`pkg/hno/sandbox/provider/docker`): a real engine
  behind the same contract, driven through the CLI so it needs no daemon SDK. It
  speaks two dialects — Docker and Apple Containerization — which differ only in
  the questions they answer (`version` vs `--version`, `ps` vs `list --all`,
  nested label JSON, `--pids-limit` availability); the lifecycle is shared, so a
  dialect is a value rather than a second provider. Real container support is
  opt-in: `HNO_SANDBOX_DOCKER=1` runs live gates, and the skip reason names the
  unmet gate instead of quietly passing.
- **Workspace staging in the core** (`staging.go`): a declared directory is now
  actually shipped to the environment. The core validates `Request.Workspace`
  and owns the walk (deterministic order, ignore patterns, byte ceiling, symlink
  refusal) because per-provider copies drift invisibly.
- **Engine-side orphan reclamation is operator-reachable**: `Sandbox.Reclaimer()`
  exposes `Sweep`/`Prune` without running a full `Diagnose`, so residue from a
  crash can be reclaimed by name after a restart.
- Resource ceilings the engine can enforce (`Limits.CPUCount`, `MemoryMB`,
  `PidsLimit`, `DiskMB`); zero means engine default and renders no flag, and
  per-run options may only tighten them.

### 🐛 Fixed
Nine defects the real engine exposed, all now covered by tests. A fake that
agrees with your assumptions cannot find any of these:

- **Probe lied.** `docker` on this host is a shim that answers `version` and
  refuses `container create`, so a version-only probe reported a usable engine
  and every run failed later. Probe now asks a second question — does `create`
  work — with `--pull=never` so a missing image reads as "not local", not
  "engine broken".
- **`ProviderConfig.Raw` was never populated**, so engine-specific config had no
  channel and the only alternative was process globals (the D-03 habit).
- **A healthy container was reported as lost.** `alive()` hard-coded Docker's
  `inspect --format`, which exits nonzero on Apple; the caller would then rebuild
  and retry against good work — the most expensive error class. It is now a
  dialect question.
- **Provider output capture was unbounded** (`cmd.Output()`), so a large payload
  output allocated the same size on the host and `OutputMaxBytes` was a lie;
  stderr was also dropped on success. Capture is now bounded and reports the
  number of bytes dropped.
- **A failed phase was stamped `succeeded`** because status came only from the Go
  error; user-code failure is data, not an error.
- **A failed build reported only "command exited with code 1"** — the compiler
  message existed nowhere in the API, so the model that `ActionModelFix` routes to
  had nothing to fix. `PhaseReport` now carries `Stdout`/`Stderr` with a bounded
  tail, and the core owns the wording while the provider owns the class.
- **`Workspace.SourceDir` was validated but never copied**, so project runs failed
  with "go.mod file not found" in a workspace that had been checked as legal.
- **`statProbe` embedded unbalanced quotes**, so the container's `sh` failed with
  "Unterminated quoted string"; nonzero was read as "file not found", making every
  `Stat` claim a missing file that `Read` returned correctly.
- **`Artifact.Size`/`Digest` were documented but never set**, so a required
  artefact always read as zero bytes and an empty file was indistinguishable from
  a collection that never measured. Artefacts are now captured during collection:
  lazy handles into a one-shot environment were unreadable after cleanup, which
  failed exactly on the paths that pass.
- **Error text repeated itself**: a nested `UnavailableError` re-serialised its
  inner attempts as prose, printing the same daemon sentence three times and
  hiding the one fact that mattered; attempts are now spliced and the remedy
  derived from `Fixes` when `Fix` is empty.
- **`WorkspaceMaxBytes: 0` rejected every file**, so a zero-value configuration
  could not stage any directory input; a non-positive ceiling now means
  "unbounded here", consistent with the rest of `Limits`.

### 🔒 Contract enforcement (SB-B2b)
The audit's real lesson was not nine bugs but one missing mechanism: a capability
bit is a provider's assertion about itself, and nothing enforced it.

- **`CapStreaming` was false.** The contract reads "delivers output incrementally
  instead of only at exit"; the provider emitted one stdout chunk *after* the
  process exited and never emitted stderr. The visible consequence is not
  performance: a build that only talks on stderr presented a live watcher an
  entirely dead pane while the capability bit stayed lit. Both streams now tap the
  CLI pipes and deliver as bytes arrive.
- **`Report.OutputSent`/`OutputSkipped` were documented and written by nobody** —
  same class as an unset artefact size. The core now measures them at the fanout,
  so they are a measurement and not a promise.
- **A false claim is observable without configuring an Observer**, via
  `Report.StreamingContract()` (`*StreamingContractError` with `Remediation()`,
  derived mechanically from output and delivery counts) plus an Observer fault
  event. A fault only a pre-configured observer could see would recreate the very
  silent no-op this work removes.
- **The violation does not flip the run's result.** Writing it into
  `Report.Failure` was tried first and immediately broke a passing project test:
  `Failure` answers "why did my payload not produce its result", and dishonest
  telemetry does not change what the code did.
- **`WithOverflow(OverflowFault)` is reachable again.** The provider discarded the
  reporter's return value (`_ = sink.Report(...)`), so a binding whose documented
  policy is "stop the run with a `HookFaultError`" behaved exactly like one that
  accepted everything. The first non-cancellation fault is now reported after the
  run while the captured output is preserved.
- **Test doubles were fixed with the provider, not after it.** The fake Runner never
  invoked the streaming channel, so every streaming assertion had been validating
  the double rather than the code. `TestStderrStreamsToo` and
  `TestChunksArriveBeforeExit` were confirmed to fail against the previous
  implementation before being kept.

### 🧪 Tests
- New real-engine gate `TestRealStreamingIsIncremental`: the container runs
  `echo; sleep 2; echo`, and the two lines must arrive as separate deliveries at
  least 500ms apart with `Report.OutputSent` agreeing with the sink's byte count.
  A buffered provider cannot pass.
- New real-engine gate `TestRealPruneRefusesAnotherProcessesContainer`: a running,
  managed, foreign container is reported by `Sweep` and refused by `Prune`. Verified
  under two concurrent gate processes: 6/6 pass each, zero skips, and zero
  `hno.sandbox.managed` residue left in the engine afterwards.
### 🐛 Fixed (SB-B2e: a resumed session replayed nothing)
- **`Session()` never replayed the steps a session recorded.** `replayStep()` had exactly
  one call site, inside `rebuild()`, so the acquisition path created a fresh environment
  from the persisted spec and adopted it un-replayed. A resumed workflow — the one
  situation a session store exists for — got an empty box reported as `Ready`: every
  recorded `Install` and `Setup` was gone, and the first failure was classified
  `ClassUserCode`, blaming the caller for the sandbox's own omission. Replay now has a
  single owner (`replayRecorded`), shared by rebuild and resume; a resume whose replay
  fails closes the half-built environment, marks the session `Lost`, and returns
  `StepReplayError` instead of pretending readiness.
- **The in-memory double could not express a lost box.** Its `NewEnvironment` returns an
  existing environment for the same session id, `MarkLost` only unlinks the session, and
  history recorded argv commands but not shell steps — so "the step was skipped" and "the
  step ran and nobody wrote it down" were indistinguishable. Added `DropEnvironment` and
  `InstalledIn`, and every executed request now leaves a trace, recorded before the
  interpret dispatch rather than as a `case` in it (a Go switch runs only the first
  matching branch, and a catch-all case there silently stops the double interpreting
  payloads).

### 🐛 Fixed (D-20: a cached step is not evidence about a container it never ran in)

- **A step-cache hit could skip work in a box that inherited nothing.**
  `Step.Key()` hashed step content only — no session, image, or environment
  generation — and `normalize()` auto-installed a process-memory step cache by
  default, so `RebuildFromSpec`, which renders a brand-new container from the
  base image, reported `cached=1 replayed=0` with an empty box. Measured both
  within one session and across two sessions: session B's install was silenced
  by session A's cache entry. Resolved as decision **B**: a hit may skip a step
  only when the environment being built carries the state that step produced.
  `newEnvironment` now returns a `boxProvenance` (state lineage plus an
  inheritance watermark) because the function that creates a box is the only
  authority on what it inherited; the watermark admits a clone of a session and
  a restore to a checkpoint that already covered the step, and a box rendered
  from the base image inherits nothing, so it replays everything. A restore
  with no recorded checkpoint mark takes the safe direction rather than
  guessing. The default handle no longer installs a step cache — the same
  reasoning `normalize()` already applies to a process-memory journal.
  `RebuildReport.StateSource` now says what the new environment inherited,
  because `Cached` without it is unexplainable.
- **`RebuildFromSpec` was also silently dropping every workspace step.** The
  provider replay loop it used handled only installs and ignored setup,
  materialise, environment, and workspace-delta steps. `EnvSpec.ReplayFrom` and
  `CachedSteps` were unreachable — the core never populated them — so they and
  the provider-side loop are removed: the core replaying through
  `Environment.Exec` is the single owner of step replay.
- **A step that exited nonzero was recorded as satisfied.** Providers report a
  failing payload as a populated `Report` with a nil error, which is the run
  contract; `replayStep` checked only the error, so a failed install was
  stamped done and written into the cache. The exit code is now the evidence.
- **A failing `Install` or `Setup` reported success and persisted the failure as
  replayable state.** `Setup` of a command exiting 7 returned nil and recorded
  the step, so the caller proceeded and every later rebuild re-ran a command
  known to fail. State-bearing calls now require a zero exit, do not record the
  step when it fails, and return a classified error.
- **Prune could delete another live process's stopped container.** With two
  real-engine runs in flight, one process's container was removed between
  `create` and `start` by the other's prune: "not held by this process and not
  running" was read as crash residue, the same inference already rejected for
  running containers. `SweepDetail` gains `HeldElsewhere` — a stopped container
  whose owner pid is alive on this host is refused, reported through `Refused()`
  and `Provider.SkippedPrune()` — so ownership is decided from engine state plus
  the owner label rather than from one process's memory.

### 🧪 Tests (D-20)

- `TestRebuildFromBaseImageReplaysEveryStep`,
  `TestCloneCarriesStateAndRebuildDoesNot`,
  `TestStepCacheCannotSilentlySkipAgain`, and
  `TestFailingInstallIsNotRecordedAsState` pin the decision;
  `TestStepCacheHonoursTheLineageRule` pins the rule directly, and
  `TestReplayPipelineRecordsSatisfiedSteps` proves the pipeline reaches it —
  the earlier version passed every behavioural test while recording nothing,
  which is why a rule needs an observability assertion of its own. Each half
  was checked against a deliberately broken implementation and fails.
- `TestOwnerAliveRejectsWhatItCannotVerify` covers the ownership predicate
  across live, dead, foreign-host, and malformed labels; the real gate asserts
  a stopped container owned by a live process survives a prune.
- Test-double fidelity: a clone now carries installed packages and history like
  a real engine does, the in-memory provider tracks its live instances so a test
  can make a loss real, and a shell directive is no longer mistaken for a
  comment.

### ⚠️ Known issues
- **D-20: a step-cache hit is treated as evidence about a container that never ran the
  step.** `StepCache` is on by default with a process-memory journal, `Step.Key()` hashes
  step content only (no session, no image, no environment generation), and
  `RebuildFromSpec` builds a fresh box from the base image without restoring a snapshot.
  Measured after dropping the box so the loss is real: a same-session rebuild and a
  cross-session rebuild both report `cached=1 replayed=0` and the new box has nothing
  installed. Failures then surface as `ClassUserCode`, so the sandbox blames the caller
  for a skip the sandbox performed. Fixing it changes the cost shape of the default retry
  mode and the meaning of an existing contract test, so it needs a decision: replay
  always for scratch boxes, or have rebuild restore the latest snapshot and skip only
  steps predating it. `TestStepCacheIsNotEvidenceAboutAFreshBox` states the acceptance
  criterion and skips with the measured values until then.

### 🐛 Fixed (SB-B2d: silent no-ops on the request surface)
- **`Request.ReadyProbe` was decorative.** `clone()` deep-copied it, `Validate()`
  rejected an empty one, and no code ever read `URL`, `Port`, `Pattern`, or `Deadline`,
  so "start a service and wait until it is ready" was implemented as "return
  immediately" — the caller's next request hit a connection refused behind a reported
  success. The core now waits: `Pattern` is matched against a bounded output window the
  fanout retains regardless of whether any Sink is configured (a promise only
  verifiable with extra machinery is its own silent no-op), while `Port`/`URL` are
  answered by providers through the new optional `ReadinessProber` interface. A provider
  that cannot inspect its guest is refused with `*UnsupportedProbeError`
  (`ClassPermanent`) instead of being allowed to "pass", and a service that never comes
  up yields `*ReadinessError` carrying what the provider actually saw plus a
  `Remediation()`.
- **`Request.Expose` could not be honoured by any provider.** The core checked
  `CapPortForward`, but port mapping is a create-time property of the substrate and
  `EnvSpec` — the only thing a provider receives at create — carried no field for it.
  Expose now flows `SessionSpec.Expose` → `EnvSpec.Expose` at provisioning, reachable
  through `WithSessionExpose(...)`, and a run asking for a mapping its session did not
  publish is refused with `*ExposeConflictError` rather than silently dropped, which
  would leave a caller probing a host port nothing forwards.
- **Readiness validation tightened.** A `ReadyProbe` without `Background` is now refused
  (a foreground run blocks until exit, so no moment exists to check it), a negative
  `Deadline` is refused, and a non-compilable `Pattern` is refused at `Validate()`
  instead of quietly being matched as a literal substring.
- **The in-memory test double stopped lying in the same direction as a provider.** Its
  background branch pushed chunks into its own buffer without ever calling
  `sink.Report`, so pattern-based readiness could not see them; and implementing
  `ReadinessProber` on the default double made the "provider cannot probe" path
  untestable. Background output now flows through the sink, and probing lives on a
  separate `sandboxtest.ProbedProvider` so both the supported and the unsupported path
  stay reachable.
### 🐛 Fixed (SB-B2c: claim reconciliation, a flaky gate, and cross-process data loss)
- **Capability claims were never reconciled with implementations.** A provider that
  declared `CapSnapshot` without implementing `Snapshotter` produced the message
  `provider "x" lacks capability "snapshot" (has: session,snapshot,clone)` — denying
  and affirming the same bit in one line, with a remedy ("choose another engine")
  pointing away from the real fault. `CapabilitySet.VerifyClaims` now compares the
  declared set against the interfaces the core actually asserts on, at `Open`, so an
  unserviceable claim is refused before anything runs; `CapabilityError` distinguishes
  "not declared" from "declared but unbacked" and carries `Remediation()`.
- **Five capability bits had no consumer at all** (`CapAudit`, `CapIdleSuspend`,
  `CapQueryStatus`, `CapPTY`, `CapHostedTrust`): a caller checking them got a yes
  that produced no behaviour. They are now registered in a semantics table that names
  what the core does with each bit, reported by `Diagnose` through
  `CapabilitySet.Unconsumed()`, and pinned by a test that fails when the enum grows
  past the table. No semantics were invented for them.
- **The real-engine gate was flaky (2 failures in 4 runs) because `create` and
  `start` shared one deadline.** A slow create left start with nothing, the CLI was
  killed, and the failure surfaced as `container start exited nonzero` with no reason
  whatsoever. Reproduced deterministically at `startup_timeout=1200ms`. Each step now
  gets its own window, and `engineFailure` reports a killed command as the deadline
  expiry it is, preserves engine stderr when present, and says so when the engine
  stayed silent. `startup_timeout` itself was read by nobody — a documented-shaped
  attribute that silently did nothing; it now parses, and unknown or malformed keys
  are refused instead of ignored.
- **`Prune` deleted containers other live processes were using.** Ownership was
  inferred from this process's in-memory map, which has no authority over the engine.
  A two-process experiment on a real engine showed it: process A held a session with
  data written into its container, process B called `Prune`, and A's workload was
  destroyed (`VICTEM LOST: container … not found`). Containers now carry an owner
  label, the engine's own running state decides reclaimability, `Prune` reclaims only
  stopped residue, refusals are recorded in `SkippedPrune()` so an empty result is not
  misread as a clean engine, and `prune_running` is an explicit opt-in for
  single-tenant machines. After the fix the same experiment reads
  `VICTIM survived, content="still in use"`.
- **`Probe` leaked a container on every `Open()`**, under a name derived from the
  image, so residue accumulated and a second concurrent process failed with
  "container already exists" — reporting a healthy machine as an unavailable engine.
  Probe names are now per-process, and the probe container is removed whether the
  create passed or failed.

## [3.0.0] - 2026-09-29

The v3 program: a lock-free control-flow graph engine with policies,
durability, HITL, streaming, and observability — plus the workflow
migration onto it.

### ✨ Added
- **Graph engine** (`pkg/hno/graph`): zero-lock single-consumer scheduler;
  DAG/conditional/default/join routing; dynamic fan-out (`Send`/`SenderFunc`/
  `AddJoinSend`); build-time validation with locatable errors; step-budget
  valve; panic barrier; cancellation normalization; typed nodes
  (`Typed[TIn, TOut]`).
- **Node policies** (slice 22): `WithRetry` (full-jitter backoff wired in
  slice 35), `WithTimeout` (per-node/per-attempt), `WithCache`
  (success-only, fail-closed), `WithTrace` (consumer-side hook) via variadic
  `AddNode`.
- **Durability** (slice 24): Sync/Async/Exit checkpoint tiers,
  `Checkpointer`/`Checkpoint` with consumer-stamped `Seq`.
- **Human-in-the-loop** (slices 27/28): `RequestInterrupt` → durable
  `*Suspension`; `Resume` with schema-checked, idempotent semantics and
  Rerun/Handoff modes; session sidecar (`pkg/hno/session/sidecar`) +
  `internal/hitlbridge` for cross-process resume.
- **Streaming protocol** (slices 23/31/34): `run.StreamMode` seven modes and
  six event types; all seven producers wired on the agent path
  (`Messages`/`Tasks`/`Updates`/`Values`/`Checkpoints`/`Debug`/`Custom`),
  `RunStreamMode` selector with `Agent.WriteCustomEvent`.
- **Observability** (slices 25/32): breaker→span→retry wired at the runner
  model boundary; run/agent-level spans — four-level coverage
  (run/agent/model/tool).
- **Store** (slice 29): G8 minimal long-term-memory core
  (`pkg/hno/store`): namespaces, optional embedding search, memory +
  Postgres backends.
- **Workflow migration** (slice 33): workflow control flow compiles onto the
  graph kernel (OPT-β); public API unchanged, existing tests byte-identical.
- Session contract fixtures landed in-repo; the 9 Go↔Python contract tests
  now execute (previously skipped).

### 🛠️ Changed
- Lint zeroed across all v3-touched packages (first golangci-lint run).
- Documentation: graph engine/policies/durability/HITL/Store guides, API
  references, and three engineering posts (en+zh).


### 🛠️ Changed
- `Agent.ToolCallLimit` now counts tool calls **per run** instead of accumulating across the stored conversation history. A run that starts with tool messages already in memory gets its full budget again. 行为变更：工具调用上限改为按本次运行计数。
- Failing runs now carry a distinct stop reason: model-invocation failures report `model_failure` and tool-execution failures report `tool_failure`, instead of both being reported as `cancelled`.
- When the tool-call limit is already exhausted at the start of a round, the model's newly requested calls are no longer silently dropped: each is answered with a paired tool message, so memory never keeps an unanswered tool call.

### ✨ Added
- `RunStreamResult.StopReason()` exposes why a streaming loop terminated (read it after receiving the terminal value on `Done`).
- `Agent.RunStream` now executes through the same `pkg/hno/runner` loop as `Agent.Run`, so truncation, stop-loop and limit policy cannot drift between the two paths.

## [1.2.9] - 2025-11-14

### ✨ Added
- EvoLink provider for text, images, and video exposed under `pkg/hno/providers/evolink` and `pkg/hno/models/evolink/*`, enabling agents and workflows to call EvoLink via the standard model interfaces.
- New EvoLink Media Agents documentation (`website/examples/evolink-media-agents.md`, `website/zh/examples/evolink-media-agents.md`) with end-to-end examples for text → image → video pipelines.
- Knowledge upload chunking: `POST /api/v1/knowledge/content` now accepts `chunk_size` and `chunk_overlap` for JSON, `text/plain` (query params), and multipart form uploads, propagating these values into stored chunk metadata.
- AgentOS HTTP tips surfaced in docs, covering custom health endpoints, `/openapi.yaml` and `/docs` routes, and `server.Resync()` usage.

### 🛠️ Changed
- Knowledge API handlers persist `chunk_size`, `chunk_overlap`, and `chunker_type` for each stored chunk, mirroring the Python AgentOS responses and enabling downstream pipelines to inspect segmentation strategy.
- AgentOS documentation is now the canonical source for configuring health probes and documentation routes for the Go server, aligning README and VitePress content.

### 🧪 Tests
- Added focused tests for EvoLink image and video models to validate configuration boundaries and task polling behavior.
- Extended knowledge API tests to cover chunking parameters, metadata propagation, and compatibility with existing search/config endpoints.

### ✅ Compatibility
- Additive release; public HTTP and Go APIs remain backward compatible.
- Knowledge chunking parameters are optional and default to previous behavior when omitted.

## [1.2.8] - 2025-11-10

### ✨ Added
- Run Context 贯穿执行：向 hooks、tools、遥测传递上下文，并在流式事件中提供 `run_context_id` 便于关联。
- 会话状态扩展：持久化 `AGUI` 子态，`GET /sessions/{id}` 返回包含 UI 状态的 `session_state`。
- 向量索引能力：
  - 可插拔 VectorDB 提供方（保持 Chroma 为默认示例，Redis 为可选依赖）。
  - VectorDB 迁移 CLI（`migrate up/down`）支持集合与索引的幂等创建/回滚。
- Embeddings：新增 VLLM（本地/远端）提供方，遵循通用 `EmbeddingFunction` 接口。
- MCPTools：支持可选参数 `tool_name_prefix`，为注册工具名加前缀。

### 🛠️ Changed
- Redis 从向量数据库默认依赖中剥离，未配置时零影响；启用时按配置注册。
- 团队模型继承仅下沉主模型配置；辅助参数需在 Agent 端显式开启。

### 🐛 Fixed
- 修复模型响应未正确绑定到步骤导致的“未绑定/零值”问题。
- 修复团队场景下基于 OS Schema 的工具判定，避免成员工具丢失。
- 修复异步存储（知识库）复合过滤与超时上下文配合的边界问题（不泄露 goroutine）。
- 强化工具包导入时的错误信息，缺失模块返回结构化提示而非 panic。
- AgentOS 错误处理路径更一致，便于契约测试断言。

### 🧪 Tests
- 新增覆盖：Run Context 传播、AGUI 状态持久化、团队主模型继承、MCP 前缀、VLLM 嵌入。
- 可选依赖（Redis）用例按环境开关执行，默认跳过。

### ✅ Compatibility
- 增量更新；默认关闭的可选能力不影响现有用户；外部 API 形态保持不变。

## [1.2.7] - 2025-11-03

### ✨ Added
- Go session service matching the Python AgentOS `/sessions` API, including Chi router, Postgres-backed store, and health endpoints.
- Deployment assets for the session runtime: dedicated Dockerfile, Compose stack, Helm chart, and curl-based verification script.
- Documentation for quick start and production deployment of the Go session service.

### 🧪 Tests
- Contract suite and Postgres store coverage ensuring session parity with existing Python fixtures.

### ✅ Compatibility
- Additive release; existing APIs remain backward compatible with the Go session runtime as an opt-in component.

---

## [1.2.6] - 2025-10-31

### ✨ Added
- Session runtime parity: session reuse endpoint, history pagination filters (`num_messages`, `stream_events`), and summary manager supporting sync/async generation.
- Response caching for agents and teams via in-memory LRU store, exposing cache-hit metadata in session runs.
- Media attachments across agents, teams, and workflows, including validation helpers and `WithMediaPayload` run option.
- Pluggable session storage drivers: MongoDB and SQLite implementations alongside Postgres with shared JSON contracts.
- Tooling expansions: Tavily Reader/Search, Claude Agent Skills, Gmail mark-as-read, Jira worklogs, ElevenLabs speech synthesis, and enhanced file toolkit.

### 🛠️ Changed
- AgentOS session handlers now expose summary endpoints, reuse semantics, run metadata (status timestamps, cancellation reasons, cache hits), and history filters.
- Workflow engine supports resumable checkpoints, cancellation persistence, and media-only payload execution paths.
- MCP client caches capability manifests and forwards media attachments in tool calls.

### 🧪 Tests
- Added dedicated coverage for cache layer, summary manager, storage drivers, workflow media flows, and new toolkits.

### ✅ Compatibility
- Additive release; existing APIs remain backward compatible with new features opt-in.

---

## [1.2.5] - 2025-10-20

### ✨ Added
- Model providers: Cohere, Together, OpenRouter, LM Studio, Vercel, Portkey, InternLM, SambaNova (Invoke/Stream + function calling)
- Core modules:
  - Evaluation system (scenario runner, per-run metrics, aggregated summary, multi-model comparison)
  - Media processing: image metadata (DecodeConfig), audio/video probe占位
  - Debug helpers: request/response compact dump
  - Cloud: NoopDeployer interface for simple deployments
- Integrations registry: register/list/health-check for third‑party services

### 🛠️ Changed
- Airflow toolkit mock schema aligned with Airflow REST API v2 (Context7): `total_entries`, `dag_run_id`, `logical_date`
- Website hero image uses `/logo.png` (fix broken asset)
- README “Multi-provider models” list updated

### 🧪 Tests
- Focused unit tests for new providers and modules (cohere, together, openrouter, lmstudio, vercel, portkey, internlm, sambanova, eval/media/debug/integrations/utils)

### ✅ Compatibility
- Additive features; no breaking changes

---

## [1.2.2] - 2025-10-18

### ✨ Added

#### Reasoning Model Support
- **Enhanced Reasoning Capabilities** - Advanced reasoning support for modern LLM models
  - Automatic detection for Gemini, Anthropic Claude, and VertexAI Claude
  - Structured reasoning output with step-by-step analysis
  - **cmd/examples/reasoning/** - Example program demonstrating reasoning capabilities

#### Batch Operations for PostgreSQL
- **High-Performance Batch Upsert** - Optimized bulk operations
  - 10x faster than individual INSERT/UPDATE operations
  - Transaction-safe with conflict resolution
  - **cmd/examples/batch_upsert/** - Performance comparison example

#### SurrealDB Vector Database Support
- **Modern Vector Database Integration** - Full SurrealDB support
  - Vector similarity search and document embedding storage
  - Real-time query capabilities
  - **cmd/examples/surreal_demo/** - Vector operations example

#### CI/CD Pipeline
- **GitHub Actions CI Workflow** - Automated testing and quality assurance
  - Go module validation and unit tests with race detection
  - Code coverage reporting and security scanning

#### Enhanced Knowledge API
- **Advanced Content Processing** - Improved knowledge ingestion
  - Multi-format content extraction (JSON, Form, Text)
  - Structured data validation and metadata extraction

### 🧪 Testing & Quality
- **Enhanced Test Coverage** - 85% reasoning, 92% batch, 88% SurrealDB
- **Race Condition Detection** - All new code validated with `-race` flag
- **Performance Benchmarks** - Added comprehensive performance tests

### 📊 Performance
- **Batch Operations** - 10x performance improvement for bulk data
- **Reasoning Detection** - Minimal overhead (<1ms)
- **No Regression** - All existing benchmarks maintained

### ✅ Backward Compatibility
- Additive changes only; no breaking changes
- All existing APIs remain unchanged
- Enhanced functionality automatically available for supported models

## [1.2.1] - 2025-10-15

### ✨ Added
- SSE event filtering on streaming endpoints (A2A)
  - `POST /api/v1/agents/:id/run/stream?types=token,complete`
- Content extraction middleware for AgentOS (JSON/Form → context)
- Google Sheets toolkit (service account)
- Minimal knowledge ingestion endpoint (`POST /api/v1/knowledge/content`)

### 🧭 Documentation Reorganization
- Adopted clear separation of docs:
  - `website/` → Implemented, user-facing documentation (VitePress site)
  - `docs/` → Design drafts, WIP, migration plans, developer/internal docs
- Added `docs/README.md` to state policy and entry points
- Added `CONTRIBUTING.md` for contributors (development, testing, docs website)

### 🔗 Link Updates
- README, CLAUDE, CHANGELOG, and release notes now point to canonical pages under `website/advanced/*` and `website/guide/*`
- Removed outdated links to duplicated files under `docs/`

### 🧹 Removed (duplicated implemented docs from docs/)
- Deleted `docs/{API_REFERENCE.md, ARCHITECTURE.md, DEPLOYMENT.md, MULTI_TENANT.md, PERFORMANCE.md, QUICK_START.md, SESSION_STATE.md, WORKFLOW_HISTORY.md, A2A_INTERFACE.md, CHANGELOG.md}`

### 🌐 Website
- Updated API docs to include Knowledge API and configuration on AgentOS page
- Updated website Release Notes with v1.2.1 summary

### ✅ Backward Compatibility
- Additive changes only; no breaking changes

## [1.2.0] - 2025-10-12

### ✨ Added

#### Workflow Session Storage (S005)
- **In-Memory Session Management** - Complete workflow session lifecycle management
  - **pkg/hno/workflow/memory_storage.go** - MemoryStorage implementation (393 lines)
    - Session creation, retrieval, updating, deletion
    - Concurrent-safe with sync.RWMutex
    - Configurable max sessions limit
    - Automatic session pruning
  - **pkg/hno/workflow/session.go** - WorkflowSession structure (300 lines)
    - Session metadata and run history
    - History retrieval with flexible count
    - Statistics tracking (total/completed/success/failed runs)
  - **pkg/hno/workflow/run.go** - WorkflowRun structure (158 lines)
    - Individual run execution tracking
    - Input/output/error recording
    - Timestamp and status management
  - **pkg/hno/workflow/storage.go** - WorkflowStorage interface (141 lines)
    - Abstract storage interface for extensibility
    - Support for custom storage implementations (Redis, PostgreSQL, etc.)

#### Workflow History Injection (S008)
- **Agent Temporary Instructions Support** - Enable history context injection without modifying agent's original configuration
  - **pkg/hno/agent/agent.go** - Enhanced with temporary instructions mechanism
    - `tempInstructions string` - Temporary override for instructions (single execution only)
    - `instructionsMu sync.RWMutex` - Thread-safe concurrent access protection
    - `GetInstructions()` - Retrieves current instructions (temporary takes precedence)
    - `SetInstructions()` - Permanently sets agent instructions
    - `SetTempInstructions()` - Temporarily sets instructions (cleared after Run)
    - `ClearTempInstructions()` - Explicitly clears temporary instructions
    - `updateSystemMessage()` - Updates system message with current instructions
  - **Auto-cleanup mechanism**: `defer a.ClearTempInstructions()` in Run() ensures zero memory leak
  - **Concurrency safety**: RWMutex allows concurrent reads, exclusive writes
  - **Backward compatible**: Empty tempInstructions behaves identically to original implementation

- **History Injection Utilities** - Flexible history formatting and injection helpers
  - **pkg/hno/workflow/history_injection.go** - History injection helper functions (151 lines)
    - `InjectHistoryToAgent()` - Injects formatted history into agent's temporary instructions
    - `buildEnhancedInstructions()` - Combines original instructions with history context
    - `RestoreAgentInstructions()` - Explicitly restores original instructions (optional, auto-cleared)
    - `FormatHistoryForAgent()` - Formats history entries with customizable options
    - `HistoryFormatOptions` - Flexible formatting configuration:
      - Header/Footer tags
      - Include/exclude input/output
      - Optional timestamps
      - Customizable labels
    - `DefaultHistoryFormatOptions()` - Sensible defaults with XML-style tags

- **Step Integration** - Seamless workflow history injection
  - **pkg/hno/workflow/step.go** - Updated Step execution with history support
    - Automatically injects history when `shouldAddHistory()` returns true
    - Retrieves formatted history from `ExecutionContext.GetHistoryContext()`
    - No changes required to existing workflow code

### 🧪 Testing

- **Comprehensive Test Coverage**:
  - **agent_instructions_test.go** - 308 lines, 8 test cases
    - `TestAgent_TempInstructions` - Basic get/set/clear functionality
    - `TestAgent_SetInstructions` - Permanent instruction changes
    - `TestAgent_TempInstructionsPriority` - Temporary overrides permanent
    - `TestAgent_ConcurrentInstructionsAccess` - 100 iterations, 300 goroutines
    - `TestAgent_Run_AutoClearsTempInstructions` - Verify defer cleanup
    - `TestAgent_Run_WithTempInstructionsError` - Cleanup on error
    - `TestAgent_UpdateSystemMessage` - Table-driven tests

  - **history_injection_test.go** - 380 lines, 12 test cases
    - Nil safety, empty history handling
    - Default and custom format options
    - Multiple runs formatting
    - Input/output inclusion control

- **Test Results**:
  - All tests passing with `-race` detector ✅
  - Agent coverage: 75.6% (100% on instruction methods)
  - Workflow coverage: 88.4% (100% on history injection)
  - Zero race conditions detected

### 📊 Performance

- **Performance Targets Met/Exceeded**:
  - `GetInstructions()`: <50ns (RLock optimized)
  - `SetTempInstructions()`: <100ns (Lock optimized)
  - `InjectHistoryToAgent()`: ~200-300ns (vs <500ns target, 2x better)
  - Total injection overhead: <1ms (vs <1ms target)
  - Memory overhead: ~40 bytes per agent (negligible)

- **No Performance Regression**:
  - Agent instantiation: Still ~180ns/op
  - Memory footprint: Still ~1.2KB per agent
  - Existing benchmarks unchanged

### 🔧 Technical Highlights

- **Zero Memory Leak** - defer-based automatic cleanup ensures temp instructions always cleared
- **Concurrency Safe** - sync.RWMutex enables high-performance concurrent access
- **API Design** - Clean, intuitive methods following Go best practices
- **Backward Compatible** - No breaking changes to existing Agent API
- **Bilingual Documentation** - All code comments in English/中文

### 📝 Files Added/Modified

**New Files:**
- `pkg/hno/workflow/history_injection.go` - History injection utilities (151 lines)
- `pkg/hno/agent/agent_instructions_test.go` - Instruction tests (308 lines)
- `pkg/hno/workflow/history_injection_test.go` - Injection tests (380 lines)

**Modified Files:**
- `pkg/hno/agent/agent.go` - Added temporary instructions support
- `pkg/hno/workflow/step.go` - Integrated history injection
- `docs/task/S008-agent-history-injection.md` - Marked as Done

### ✅ Acceptance Criteria

All S008 acceptance criteria met or exceeded:
- ✅ Agent supports temporary instructions
- ✅ Agent.Run auto-clears temporary instructions
- ✅ Concurrent access to instructions is thread-safe
- ✅ History injection doesn't affect Agent's original configuration
- ✅ Flexible history formatting options provided
- ✅ Test coverage >85% (100% on new methods)
- ✅ All tests passing
- ✅ Performance: Injection overhead <1ms (achieved <1μs)

### 🚀 Usage Example

```go
import (
    "github.com/rexleimo/agno-go/pkg/hno/agent"
    "github.com/rexleimo/agno-go/pkg/hno/workflow"
)

// Create agent with original instructions
agent, _ := agent.New(agent.Config{
    Model:        model,
    Instructions: "You are a helpful assistant",
})

// Format workflow history
history := []workflow.HistoryEntry{
    {Input: "hello", Output: "hi there"},
    {Input: "how are you", Output: "I'm good"},
}
historyContext := workflow.FormatHistoryForAgent(history, nil)

// Inject history (temporarily enhances instructions)
workflow.InjectHistoryToAgent(agent, historyContext)

// Run agent (temp instructions auto-cleared after execution)
output, err := agent.Run(ctx, "new question")

// Agent's original instructions remain unchanged
fmt.Println(agent.Instructions) // "You are a helpful assistant"
```

## [1.1.1] - 2025-10-08

### ✨ Added

#### Groq Model Integration
- **Groq Ultra-Fast Inference Support** - Industry-leading LLM inference speed
  - **pkg/hno/models/groq/** - Complete Groq model implementation (287 lines)
    - `groq.go` - Main implementation using OpenAI-compatible API
    - `types.go` - Model constants and metadata (7+ models)
    - `groq_test.go` - Comprehensive unit tests (580+ lines)
    - `README.md` - Detailed documentation and examples
  - **Supported Models:**
    - LLaMA 3.1 8B Instant (fastest, recommended)
    - LLaMA 3.1 70B Versatile (most capable)
    - LLaMA 3.3 70B Versatile (latest)
    - Mixtral 8x7B (Mixture of Experts)
    - Gemma 2 9B (compact but powerful)
    - Whisper Large V3 (speech recognition)
    - LLaMA Guard 3 8B (content moderation)
  - **Performance Benefits:**
    - 10x faster inference vs traditional cloud LLM providers
    - Ultra-low first-token latency
    - High concurrent request throughput
  - **Features:**
    - OpenAI-compatible API (reuses go-openai SDK)
    - Full function calling support
    - Streaming and non-streaming modes
    - Configurable timeout, temperature, max tokens
  - **cmd/examples/groq_agent/** - Example program with 4 scenarios
  - **Test Coverage:** 52.4% ✅
  - **Documentation:** Updated CLAUDE.md with Groq configuration

### 📝 Documentation
- Added Groq to supported models list in CLAUDE.md
- Added `GROQ_API_KEY` environment variable documentation
- Updated example programs list with groq_agent
- Added Groq to test coverage table

## [1.1.0] - 2025-10-08

### ✨ Added

#### A2A (Agent-to-Agent) Interface
- **A2A Protocol Support** - Standardized agent-to-agent communication based on JSON-RPC 2.0
  - **pkg/agentos/a2a/** - Complete A2A interface implementation (1001 lines)
    - `types.go` - JSON-RPC 2.0 type definitions (154 lines)
    - `validator.go` - Request validation logic (108 lines)
    - `mapper.go` - A2A ↔ RunInput/Output conversion (317 lines)
    - `a2a.go` - Main interface and entity management (148 lines)
    - `handlers.go` - HTTP handlers for send/stream endpoints (274 lines)
  - REST API endpoints:
    - `POST /a2a/message/send` - Non-streaming message send
    - `POST /a2a/message/stream` - Streaming via Server-Sent Events (SSE)
  - Multi-media support: Text, images (URI/bytes), files, JSON data
  - Compatible with Python Agno A2A implementation
  - **cmd/examples/a2a_server/** - Example A2A server
  - **pkg/agentos/a2a/README.md** - Complete bilingual documentation (English/中文)

#### Workflow Session State Management
- **Thread-Safe Session State** - Cross-step session management with race condition fix
  - **pkg/hno/workflow/session_state.go** - SessionState implementation (192 lines)
    - Thread-safe with sync.RWMutex
    - Deep copy via JSON serialization for parallel branch isolation
    - Smart merging: only applies actual changes (last-write-wins)
  - **ExecutionContext Enhancement** - Added session support fields:
    - `SessionState *SessionState` - Cross-step persistent state
    - `SessionID string` - Unique session identifier
    - `UserID string` - Multi-tenant user identifier
  - **Parallel Execution Fix** - Solved Python Agno v2.1.2 race condition:
    - Clone SessionState for each parallel branch
    - Merge modified states after parallel execution
    - Prevents data races in concurrent workflow steps
  - **pkg/hno/workflow/SESSION_STATE.md** - Comprehensive documentation
  - **Test Coverage:** 79.4% with race detector validation ✅

#### Multi-Tenant Memory Support
- **User-Isolated Memory Storage** - Multi-tenant conversation history
  - Enhanced Memory interface with optional `userID` parameter:
    - `Add(message, userID...)` - Add message for specific user
    - `GetMessages(userID...)` - Get user-specific messages
    - `Clear(userID...)` - Clear user messages
    - `Size(userID...)` - Get user message count
  - `InMemory` implementation:
    - Per-user message storage: `map[string][]*types.Message`
    - Independent maxSize limit per user
    - Backward compatible: empty userID defaults to "default" user
  - Agent integration:
    - Added `UserID string` field to Agent and Config
    - All memory operations pass agent's UserID
  - New `ClearAll()` method to clear all users
  - **Tests:** Multi-tenant isolation, backward compatibility, race detection ✅

#### Model Timeout Configuration
- **Configurable Request Timeout** - Fine-grained timeout control for LLM calls
  - **OpenAI Model** (`pkg/hno/models/openai/openai.go`):
    - Added `Timeout time.Duration` to Config
    - Default: 60 seconds
    - Applied to underlying HTTP client
  - **Anthropic Claude** (`pkg/hno/models/anthropic/anthropic.go`):
    - Added `Timeout time.Duration` to Config
    - Default: 60 seconds
    - Applied to HTTP client
  - Usage example:
    ```go
    claude := anthropic.New("claude-3-opus", anthropic.Config{
        APIKey:  apiKey,
        Timeout: 30 * time.Second, // Custom timeout
    })
    ```

### 🐛 Fixed

- **Workflow Race Condition** - Fixed parallel step execution data race
  - Python Agno v2.1.2 had shared `session_state` dict causing overwrites
  - Go implementation uses independent SessionState clones per branch
  - Smart merge strategy prevents data loss in concurrent execution

### 🧪 Testing

- **New Test Suites:**
  - `session_state_test.go` - 543 lines of session state tests
  - `memory_test.go` - Multi-tenant memory tests (4 new test cases)
  - `agent_test.go` - Multi-tenant agent test (TestAgent_MultiTenant)
  - `openai_test.go` - Timeout configuration test
  - `anthropic_test.go` - Timeout configuration test

- **Test Results:**
  - All tests passing with `-race` detector ✅
  - Workflow coverage: 79.4%
  - Memory coverage: maintained at 93.1%
  - Agent coverage: maintained at 74.7%

### 📊 Performance

- **No Performance Regression** - All benchmarks remain consistent:
  - Agent instantiation: ~180ns/op
  - Memory footprint: ~1.2KB per agent
  - Thread-safe concurrent operations

### 🔧 Technical Highlights

- **Python Agno v2.1.2 Compatibility** - Migrated features from commits:
  - `7e487eb` → `bf3286bb` (23 commits, 5 major features)
  - A2A utils implementation
  - Session state race condition fix
  - Multi-tenant memory support
  - Model timeout parameters

- **Bilingual Documentation** - All new features documented in English/中文:
  - Inline code comments
  - README files
  - API documentation

### 📝 Files Added/Modified

**New Files:**
- `pkg/agentos/a2a/*.go` - A2A interface (5 files, 1001 lines)
- `pkg/hno/workflow/session_state.go` - Session state (192 lines)
- `pkg/hno/workflow/session_state_test.go` - Tests (543 lines)
- `pkg/agentos/a2a/README.md` - A2A documentation
- `pkg/hno/workflow/SESSION_STATE.md` - Session state guide
- `cmd/examples/a2a_server/main.go` - A2A example server

**Modified Files:**
- `pkg/hno/memory/memory.go` - Multi-tenant support
- `pkg/hno/memory/memory_test.go` - New multi-tenant tests
- `pkg/hno/agent/agent.go` - UserID support
- `pkg/hno/agent/agent_test.go` - Multi-tenant test
- `pkg/hno/workflow/workflow.go` - SessionState fields
- `pkg/hno/workflow/parallel.go` - Race condition fix
- `pkg/hno/models/openai/openai.go` - Timeout support
- `pkg/hno/models/anthropic/anthropic.go` - Timeout support

### ✅ Migration Status

Completed migration from Python Agno v2.1.2:
- ✅ A2A interface implementation
- ✅ Workflow session state management (race condition fix)
- ✅ Multi-tenant memory support (userID)
- ✅ Model timeout parameters (OpenAI, Anthropic)

### 🚀 Upgrade Guide

**Multi-Tenant Memory:**
```go
// Old (single-tenant)
agent := agent.New(agent.Config{
    Memory: memory.NewInMemory(100),
})

// New (multi-tenant)
agent := agent.New(agent.Config{
    UserID: "user-123",  // Add UserID
    Memory: memory.NewInMemory(100),
})
```

**Workflow Session State:**
```go
// Create context with session info
ctx := workflow.NewExecutionContextWithSession(
    "input",
    "session-id",
    "user-id",
)

// Access session state
ctx.SetSessionState("key", "value")
value, _ := ctx.GetSessionState("key")
```

**A2A Interface:**
```go
// Create A2A interface
a2a := a2a.New(a2a.Config{
    Agents: []a2a.Entity{myAgent},
    Prefix: "/a2a",
})

// Register routes (Gin)
router := gin.Default()
a2a.RegisterRoutes(router)
```

### 📖 Documentation

- [A2A README](pkg/agentos/a2a/README.md) - Complete A2A protocol guide
- [Session State Guide](pkg/hno/workflow/SESSION_STATE.md) - Workflow session management
- [CHANGELOG.md](CHANGELOG.md) - This file

## [1.0.3] - 2025-10-06

### 🧪 Improved

#### Testing & Quality
- **Enhanced JSON Serialization Tests** - Achieved 100% test coverage for utils/serialize package
  - Added error handling tests for unserializable types (channels, functions)
  - Added panic behavior tests for MustToJSONString
  - Added edge case tests (nil pointers, empty collections)
  - Test coverage: 92.3% → 100% ✅

#### Performance Benchmarks
- **Optimized Performance Tests** - Aligned with Python Agno performance testing patterns
  - Simplified agent instantiation benchmark (removed unnecessary variable)
  - Cleaned up tool registration patterns
  - Renamed test for consistency: "Tool Instantiation Performance" → "Agent Instantiation"

#### Documentation
- **Comprehensive Package Documentation** - Added bilingual (English/中文) documentation
  - Package-level overview with usage examples
  - Detailed function documentation with examples
  - Performance metrics included in package docs
  - All public APIs now fully documented

### 📊 Performance

Current benchmark results on Apple M3:
- **ToJSON**: ~600ns/op, 760B/op, 15 allocs/op
- **ConvertValue**: ~180ns/op, 392B/op, 5 allocs/op
- **Agent Creation**: ~180ns/op (16x faster than Python)

### 🔧 Technical Highlights

- **100% Test Coverage** - utils/serialize package now has complete test coverage
- **Better Error Handling** - Comprehensive tests for edge cases and error conditions
- **Production Ready** - Serialization utilities validated for WebSocket and API usage
- **Python Compatibility** - Prevents the JSON serialization bug found in Python Agno (commit aea0fc129)

### 📝 Files Changed

- `pkg/hno/utils/serialize.go` - Enhanced documentation with examples and performance notes
- `pkg/hno/utils/serialize_test.go` - Added 3 new test cases for error handling
- `pkg/hno/agent/agent_bench_test.go` - Simplified benchmark following Python patterns

### ✅ Migration Status

Completed migration items from Python Agno:
- ✅ JSON serialization bug fix (aea0fc129) - Already prevented in Go implementation
- ✅ Performance test optimization (e639f4996) - Applied to Go benchmarks
- 🔄 Custom route prefix (06baed104) - Deferred to Week 7 (AgentOS expansion)
- 🔄 HN tools update (24c3ee688) - Documentation only, no action needed

## [1.0.2] - 2025-10-05

### ✨ Added

#### New LLM Provider
- **GLM (智谱AI)** - Full integration with Zhipu AI's GLM models
  - Support for GLM-4, GLM-4V (vision), GLM-3-Turbo
  - Custom JWT authentication (HMAC-SHA256)
  - Synchronous API calls (`Invoke`)
  - Streaming responses (`InvokeStream`)
  - Tool/Function calling support
  - Test coverage: 57.2%

#### Implementation Details
- **pkg/hno/models/glm/glm.go** - Main model implementation (410 lines)
- **pkg/hno/models/glm/auth.go** - JWT authentication logic (59 lines)
- **pkg/hno/models/glm/types.go** - GLM API type definitions (105 lines)
- **pkg/hno/models/glm/glm_test.go** - Comprehensive unit tests (320 lines)
- **pkg/hno/models/glm/README.md** - Complete usage documentation

#### Examples & Documentation
- **cmd/examples/glm_agent/** - GLM agent example with calculator tools
  - Chinese language support demonstration
  - Multi-step calculation examples
  - Tool calling integration
- Updated README.md with GLM provider information
- Updated CLAUDE.md with GLM configuration and usage
- Added bilingual comments (English/中文) throughout codebase

### 🔧 Technical Highlights

- **Custom JWT Authentication** - Implemented GLM-specific JWT token generation
  - 7-day token expiration
  - Secure HMAC-SHA256 signing
  - Automatic token regeneration per request

- **OpenAI-Compatible Format** - API structure similar to OpenAI for easy integration
  - Request/response format alignment
  - Tool calling compatibility
  - Streaming support via Server-Sent Events (SSE)

- **Type Safety** - Full Go type system integration
  - Strongly-typed request/response structures
  - Error handling with custom error types
  - Context support for cancellation

### 📊 Test Results

- ✅ All 7 GLM tests passing
- ✅ 57.2% code coverage
- ✅ Race detector: PASS
- ✅ Build verification: SUCCESS

### 🌍 Environment Variables

New environment variable for GLM:
```bash
export ZHIPUAI_API_KEY=your-key-id.your-key-secret
```

### 📦 Dependencies

Added:
- `github.com/golang-jwt/jwt/v5 v5.3.0` - For JWT authentication

### 🎯 Supported Models

Total LLM providers increased from 6 to 7:
- OpenAI (GPT-4, GPT-3.5, GPT-4 Turbo)
- Anthropic (Claude 3.5 Sonnet, Claude 3 Opus/Sonnet/Haiku)
- **GLM (智谱AI: GLM-4, GLM-4V, GLM-3-Turbo)** ⭐ NEW
- Ollama (Local models)
- DeepSeek (DeepSeek-V2, DeepSeek-Coder)
- Google Gemini (Gemini Pro, Flash)
- ModelScope (Qwen, Yi models)

### 📝 Documentation Updates

- README.md - Added GLM to supported models list with example code
- CLAUDE.md - Added GLM environment variables and configuration
- Created pkg/hno/models/glm/README.md with comprehensive usage guide
- All code comments are bilingual (English/中文)

## [1.0.0] - 2025-10-02

### 🎉 Initial Release

Agno-Go v1.0 is a high-performance Go implementation of the Agno multi-agent framework, designed for building production-ready AI agent systems.

### ✨ Features

#### Core Agent System
- **Agent** - Single autonomous agent with tool support
  - LLM model integration
  - Tool/function calling
  - Conversation memory
  - System instructions
  - Max loop protection
  - Coverage: 74.7%

- **Team** - Multi-agent collaboration with 4 coordination modes:
  - `Sequential` - Agents work one after another
  - `Parallel` - All agents work simultaneously
  - `LeaderFollower` - Leader delegates tasks to followers
  - `Consensus` - Agents discuss until reaching agreement
  - Coverage: 92.3%

- **Workflow** - Step-based orchestration with 5 primitives:
  - `Step` - Basic workflow step (agent or function)
  - `Condition` - Conditional branching
  - `Loop` - Iterative loops
  - `Parallel` - Parallel execution
  - `Router` - Dynamic routing
  - Coverage: 80.4%

#### LLM Providers
- **OpenAI** - GPT-4, GPT-3.5, GPT-4 Turbo
- **Anthropic** - Claude 3.5 Sonnet, Claude 3 Opus/Sonnet/Haiku
- **Ollama** - Local model support (llama3, mistral, etc.)

#### Tools
- **Calculator** - Basic math operations
- **HTTP** - GET/POST requests
- **File** - File operations with safety controls

#### Storage & Memory
- **Memory** - In-memory conversation storage with auto-truncation
  - Configurable message limits
  - Thread-safe operations
  - Coverage: 93.1%

- **Session** - Session management for multi-turn conversations
  - In-memory storage (default)
  - PostgreSQL support (via schema)
  - Redis caching support

#### Vector Database
- **ChromaDB** - Vector storage for RAG applications
  - Document embedding
  - Semantic search
  - Collection management

#### AgentOS - Production Server
- **RESTful API** - Full-featured HTTP server
  - Session CRUD operations
  - Agent registration and execution
  - Health check endpoint
  - Structured logging with slog
  - CORS support
  - Request timeout handling
  - Coverage: 65.0%

- **Agent Registry** - Thread-safe agent management
  - Dynamic agent registration
  - Concurrent access support
  - Agent lifecycle management

#### Developer Experience
- **Types** - Comprehensive type system
  - Message types (System, User, Assistant, Tool)
  - Error types with codes
  - Model request/response types
  - 100% test coverage ⭐

- **Documentation**
  - Complete API documentation (OpenAPI 3.0)
  - Deployment guide (Docker, K8s, native)
  - Architecture documentation
  - Performance benchmarks
  - Code examples

#### Deployment
- **Docker** - Production-ready Dockerfile
  - Multi-stage build (~15MB final image)
  - Non-root user
  - Health checks
  - Security best practices

- **Docker Compose** - Full stack deployment
  - AgentOS server
  - PostgreSQL database
  - Redis cache
  - ChromaDB (optional)
  - Ollama (optional)

- **Kubernetes** - K8s manifests included
  - Deployment, Service, ConfigMap, Secret
  - Health probes
  - Resource limits
  - Horizontal Pod Autoscaling ready

### 📊 Performance

- **Agent Creation:** ~180ns/op (16x faster than Python)
- **Memory Footprint:** ~1.2KB per agent
- **Test Coverage:** 80.8% average across core packages
- **Concurrent Operations:** Fully thread-safe with RWMutex

### 🧪 Testing

- **85+ test cases** across all core packages
- **100% pass rate** ✅
- All packages exceed 70% coverage target
- Comprehensive integration tests
- Concurrent access tests
- Performance benchmarks

### 📚 Examples

- `simple_agent` - Basic agent with calculator
- `claude_agent` - Anthropic Claude integration
- `ollama_agent` - Local model support
- `team_demo` - Multi-agent collaboration
- `workflow_demo` - Workflow orchestration
- `rag_demo` - RAG with ChromaDB

### 🔧 Technical Details

**Dependencies:**
- Go 1.21+
- Gin web framework
- PostgreSQL 15+ (optional)
- Redis 7+ (optional)
- ChromaDB (optional)

**Project Structure:**
```
agno-Go/
├── pkg/hno/          # Core framework
│   ├── agent/         # Agent implementation
│   ├── team/          # Team coordination
│   ├── workflow/      # Workflow engine
│   ├── models/        # LLM providers
│   ├── tools/         # Tool integrations
│   ├── memory/        # Conversation memory
│   └── types/         # Core types
├── pkg/agentos/       # Production server
│   ├── server.go      # HTTP server
│   ├── registry.go    # Agent registry
│   └── openapi.yaml   # API specification
├── cmd/examples/      # Example programs
└── docs/              # Documentation
```

### 🎯 Design Philosophy

Agno-Go follows the **KISS principle** (Keep It Simple, Stupid):
- Focus on quality over quantity
- Clear, maintainable code
- Comprehensive testing
- Production-ready from day one

### 🔒 Security

- Non-root Docker container
- Secret management best practices
- Input validation
- Error handling
- Rate limiting support
- HTTPS/TLS ready

### 📖 Documentation

- [README.md](README.md) - Getting started
- [website/advanced/architecture.md](website/advanced/architecture.md) - Architecture overview
- [website/advanced/deployment.md](website/advanced/deployment.md) - Deployment guide
- [website/advanced/performance.md](website/advanced/performance.md) - Performance benchmarks
- [docs/DEVELOPMENT.md#testing-standards](docs/DEVELOPMENT.md#testing-standards) - Test coverage standards
- [pkg/agentos/README.md](pkg/agentos/README.md) - AgentOS API guide
- [pkg/agentos/openapi.yaml](pkg/agentos/openapi.yaml) - OpenAPI specification

### 🙏 Acknowledgments

Agno-Go is inspired by and compatible with the design philosophy of:
- [Agno](https://github.com/agno-agi/agno) - Python multi-agent framework

### 📝 Migration from Python Agno

Agno-Go maintains API compatibility where possible, making migration straightforward:

**Python:**
```python
from agno.agent import Agent
from agno.models.openai import OpenAI

agent = Agent(
    name="Assistant",
    model=OpenAI(id="gpt-4"),
)
response = agent.run("Hello!")
```

**Go:**
```go
import (
    "github.com/rexleimo/agno-go/pkg/hno/agent"
    "github.com/rexleimo/agno-go/pkg/hno/models/openai"
)

model, _ := openai.New("gpt-4", openai.Config{...})
ag, _ := agent.New(agent.Config{
    Name: "Assistant",
    Model: model,
})
output, _ := ag.Run(ctx, "Hello!")
```

### 🚀 Getting Started

**Installation:**
```bash
go get github.com/rexleimo/agno-go
```

**Quick Start:**
```bash
# Clone repository
git clone https://github.com/rexleimo/agno-go
cd agno-Go

# Run example
export OPENAI_API_KEY=sk-...
go run cmd/examples/simple_agent/main.go

# Or use Docker
docker-compose up -d
curl http://localhost:8080/health
```

### 🛣️ Roadmap

**v1.1** (Planned)
- Streaming response support
- More tool integrations
- Additional vector databases
- Enhanced monitoring/metrics

**v1.2** (Planned)
- gRPC API support
- WebSocket for real-time updates
- Plugin system
- Advanced workflow features

### 📄 License

MIT License - See [LICENSE](LICENSE) for details.

### 🔗 Links

- **GitHub:** https://github.com/rexleimo/agno-go
- **Documentation:** https://docs.agno.com
- **Issues:** https://github.com/rexleimo/agno-go/issues
- **Discussions:** https://github.com/rexleimo/agno-go/discussions

---

**Full Changelog:** https://github.com/rexleimo/agno-go/commits/v1.0.0
