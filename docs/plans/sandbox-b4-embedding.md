# B4 嵌入与收尾 — 工作项计划

来源：`docs/design/sandbox-runtime-architecture.html` §12.1 批次表 B4、§12.2 兼容层决策、台账 D-10（旧 `Executor` 签名冻结）。
前置：B1 契约已冻结（`92991ab`），B2 真引擎四项已量过（`f1baee4`/`743b541`）。B3 的 BoxLite P0 实测门仍需 `/dev/kvm` 主机 + 显式授权，**不在本计划内**。

## 完成门（设计原文）

> graph 内跑通「挂起 → 审批 → `Resume` 回到同一个 box」；guide 数字与 `Diagnose` 文案一致

## B4a — HITL 审批闭环（`planned`，包内）

**现状证据**：`execute.go:311` 能把 `ActRequireApproval` 变成 `ApprovalPendingError`，但
① 没有任何回答入口（无 approve/deny/resume API）；② `Timeouts.ApprovalHold` 与 `Approval.Expires`
两个字段在 `pkg/hno/` 内**零消费者**（`grep` 证实）。也就是说审批只能发生一次、然后永久悬空。

**目标契约**

- `Session.Awaiting() []PendingApproval`：调用方**无需配 Observer** 就能看到挂了什么（本包既有规则：
  要调用方额外装观察者才能核对的契约声明，本身就是静默空转）。
- `Session.Approve(ctx, id, approver)` / `Session.Deny(ctx, id, reason)`：记录裁决。
- `Session.Resume(ctx, id)`：把**同一次请求**送回管线，并让它**回到申请审批时那个 box**。
- 悬空审批有过期：`Approval.Expires` 覆盖句柄级 `Timeouts.ApprovalHold`（同名默认），过期后裁决作废，
  必须重新申请 —— 这是两个字段第一次被真正读取。

**信任不变量（不可让步）**

审批绑定的是「这个环境 + 这次请求」。若在等待期间环境换代（lost / rebuilt / dropped generation），
把已批准的工作跑进另一个 box 就是审批要防的那种替换 ⇒ **返回带分类的类型化错误，绝不静默在新盒子里继续**。

**allowedWrites**：`pkg/hno/sandbox/{approval.go,execute.go,session.go,observer.go,failure.go,options.go,lifecycle.go}` 及其测试。
**failureClass**：pending = `ClassDenied`（可续跑），换代 = `ClassLost`，配置不匹配 = `ClassPermanent`。

## B4b — `sandboxgraph`（`planned`，新包）

图节点内运行沙箱：`NewNode(SessionSpec|Request, ...)`，把 `Report`/artifacts 接进图状态，
挂起时节点可被 checkpoint/恢复 —— 即完成门的 graph 侧。
**allowedWrites**：`pkg/hno/sandbox/graph/**`（新建）。不改 `pkg/hno/graph` 本体语义。

## B4c — `run/adapter.go`（`planned`，纯新增文件）

旧 `Executor` 签名冻结（D-10），适配层内部转调新 core。**只新增文件，不动现有调用方。**
**allowedWrites**：`pkg/hno/tools/run/adapter.go`、`adapter_test.go`。

## B4d — `toolkit.go:106-115` 双写迁移 ⚠ **需人类授权**

这一步修改既有调用方行为（`pkg/hno/tools/run/toolkit.go`），正是 B1/B2 全程隔离的边界。
B2 已完成 ⇒ 可以谈，但**不在获得明确同意前执行**。

## B4e — P5 DX 门禁

recipes / CLI / guide 的示例数字与 `Diagnose` 实际文案一致（设计明确要求），benchmark 门。

## B4a 完成记录（实测）

- 交付：approval.go +12 测试；6 个新类型化错误；4 个新事件；Diagnose approvals 检查。
- 顺带修掉三个 B1/B2 遗留真缺陷：锁序死锁、Resumable 重建分支不可达、懒建/重建丢 provenance。
- 决策 D-24..D-27 已入台账。
- 遗留：B4b sandboxgraph 检查点/恢复对接尚未开始；B4c run 兼容适配器未动；B4d 需授权。

## B4b 工作项（本轮执行）

```yaml
type: planned
targets:
  - pkg/hno/sandbox/graph/node.go        (new)
  - pkg/hno/sandbox/graph/graph.go       (new)
  - pkg/hno/sandbox/graph/*_test.go      (new)
allowedWrites:
  - pkg/hno/sandbox/graph/**             # 仅新建包
failureClass: contract
explicitIntent: plan
```

**复用而非再造**：`pkg/hno/graph` 已有 HITL（`Interrupt/RequestInterrupt/Suspension/
ResumeRerun/ResumeHandoff`）与 `Checkpointer/Checkpoint{Kind:EntryInterrupt}`。B4b 只做
`sandbox.ApprovalPendingError ↔ graph.Interrupt` 的双向映射，不改 `pkg/hno/graph` 语义。

完成门（每条先写会失败的测试）：
1. 审批中断成为图的中断，而不是终态失败。
2. `graph.Resume({approved:true})` 在**同一台盒**里跑完原请求。
3. 盒被换掉 ⇒ 继承 B4a 的 `ApprovalEnvironmentChangedError`，不消耗授权。
4. 原请求恰好执行一次（重入不得双跑）。
5. `Report`/artifacts 进图状态，供后继节点分支。
6. 中断条目落 checkpoint：含 session/approval/lineage，够新进程重取会话并作答。
7. Deny 是可分支的输出，配合 `WithRefusalFails()` 也可变错误。

边界（诚实记录，不夸大）：`pkg/hno/graph` 明示「跨进程重建不属本片」，故 B4b 承诺的是
**沙箱侧跨进程可重取同一会话/审批**，不是图引擎跨进程续跑。

## B4b 完成记录（实测）

交付：`pkg/hno/sandbox/graph/{graph.go,ask.go}` + 22 条测试，覆盖率 78.9%。
映射不改 `pkg/hno/graph` 语义：`*sandbox.ApprovalPendingError ↔ coregraph.Interrupt`
（`Payload` 带 `Ask{Session,Approval,Lineage,Question}`），人类响应经
`InterruptResponse` 读回并翻成 `Approve/Deny + Resume`。完成门 7 条全部有测试，
反证 3 条（去钉定 / 不转中断 / 去守卫）均对旧实现失败。

### 本轮发现、**未修**的上游缺陷（超出本工作项 allowedWrites，需授权另开）

1. `pkg/hno/sandbox`：`WithCreateIfMissing(false)` 在无 session store 时是**静默 no-op**。
   文档（`sandbox.go:295`）承诺「unknown id 返回 `*StaleSessionError`」，实现被
   `if sb.cfg.Sessions != nil` 守卫，故存储缺失时照常开机。后果：跨进程读旧 checkpoint
   作答会为没人请求的 session 起一台新盒。绕法（本包内）：先用 `Diagnose` 的审批计数排除，
   再以 `Awaiting()` 精确定位；`graph_test.go` 用 `t.Log` 显式记录，不静默。
2. `pkg/hno/graph`：`checkShape` 对**可选** schema 属性也会校验——它遍历 schema properties
   并用 `respMap[name]`（缺失即 nil）判定，与自身注释「缺失的属性只由 required 管，这里跳过」
   矛盾。后果：声明 `reason?: string` 会让所有省略该字段的回答被 `ErrInvalidResponse` 拒掉。
   绕法（本包内）：默认 schema 只声明决策字段，`reason` 仍接受但不声明（改动在
   `graph.go` 注释中说明）。

## B4c 工作项（本轮执行）

```yaml
type: planned
targets:
  - pkg/hno/tools/run/adapter.go   (new file only)
  - pkg/hno/tools/run/adapter_test.go (new file only)
allowedWrites:
  - pkg/hno/tools/run/adapter.go
  - pkg/hno/tools/run/adapter_test.go
failureClass: contract
explicitIntent: implement
```

纯新增：遗留 `Executor` 签名冻结（D-10），故适配器实现同一个
`Run(ctx, Spec) (Result, error)` + `Close() error`，由 `pkg/hno/sandbox` 承接。
**不改** `executor.go/podman.go/e2b.go/local.go/toolkit.go` 与任何调用方（B4d 需授权）。

已实测的合同落差（适配器必须说实话，不许静默弱化）：
1. `Spec.Workspace` 遗留是宿主目录**读写 bind mount**；新模型是快照式 stage-in/declared-out，
   且 docker provider 的 bind mount 需显式 `allow_bind_mounts`。⇒ 拒绝并指向新 API，
   不偷偷改成快照（宿主目录不再被实时改写是语义变化）。
2. 内存/PID 限额是**句柄级**（`runoption.go:126`，per-run 只收紧 `OutputMaxBytes`）。
   ⇒ 适配器以构造期 `sandbox.Limits` 为已知上限，对与之不同的 `Spec.MemoryLimit/PidLimit`
   返回类型化拒绝并说明配置点，而不是承诺了不执行。
3. 新 registry 只有 `docker` provider。⇒ `Backend` 为 `podman`/`e2b` 时明确拒绝，
   不做静默替换 provider；`auto`/`docker` 引擎缺失时映射回遗留 `ErrNoProvider` 哨兵。

## B4c 完成记录（实测）

交付 `pkg/hno/tools/run/adapter.go` + `adapter_test.go`（仅此两文件，既有文件零改动）。
14 条测试；run 包覆盖率 61.1% → 65.8%；反证 4 项有效；真实门 27s 通过、残留 0。

过程中被自己的测试抓到一处「记录与执行分叉」：拒绝理由引用构造期记忆的限额，而调用方
`Options` 可覆盖句柄限额 ⇒ 改为从 `sb.Status().Limits` 读回（同 D-20/D-25 的同类缺陷）。

B4d（`toolkit.go:106-115` 双写迁移）**未做**：需授权。

## D-31 完成记录（实测）

修复 B4b 遗留的第 1 项上游缺陷。拒绝不再嵌在 `sb.cfg.Sessions != nil` 内：无 store 句柄取未知 id 也返回
`*StaleSessionError`，不注册会话、不开环境。`RunProject` 显式传 `WithCreateIfMissing(true)`。
`sandboxgraph` 的 `Diagnose` 计数 workaround 删除，`ErrNoSession` / `ErrNoApproval` 改由事实驱动。

5 条新合同测试（含「不创建环境」用 provider 引用直接探测）；反证：旧实现返回带 `gen:...` 血缘的活会话。
`-race` 四包全过；真实门 9 PASS / 0 FAIL、残留 0；覆盖 `sandbox` 73.2%、`sandbox/graph` 80.3%。

第 2 项（`pkg/hno/graph.checkShape` 校验缺失可选属性）**仍未修**：跨包、影响所有 schema 校验使用方。


D-32：第 2 项上游缺陷（`pkg/hno/graph` 可选属性误校验）已按授权修复，`sandboxgraph` 默认 schema 声明回可选 `reason`。

## B4d 工作项（本轮执行，已获授权）

```yaml
type: planned
targets:
  - pkg/hno/tools/run/toolkit.go          # 既有：106-115 双写迁移
  - pkg/hno/tools/run/toolkit_test.go     # 既有：补分类回归
allowedWrites:
  - pkg/hno/tools/run/toolkit.go
  - pkg/hno/tools/run/toolkit_test.go
failureClass: contract
```

设计依据（§216/§815）：今天把 provider 错误塞进 map 并 `return ..., nil` ⇒ 模型、监控、Policy
三方都看不见失败；要改成**双写**（真 error + map 内 `failure` 摘要）。

已实测的前置事实（决定分类规则，不是拍脑袋）：
1. `agent.executeToolCalls` 恒返回 nil ⇒ handler error **不会中断整轮**；模型仍收到
   `"tool execution error: …"` 消息，但 **map 在 error 分支被丢弃** ⇒ error 文本必须自足。
2. `podman.go/local.go` 的 `exitCodeOf` 对非 `*exec.ExitError` 返回 **-1**；e2b 用
   `Result{ExitCode: -1}` 作「未观测到退出码」哨兵 ⇒ `ExitCode > 0` 可安全表示「用户代码自己退的」。
3. 遗留 executor 对「用户代码非 0 退出」**也返回 error** ⇒ 无脑上抛会把代码故障说成
   环境故障，正是设计禁止的混淆方向。

分类顺序（先哨兵、再超时、再退出码）：
`err==nil` → 数据；`ErrNoProvider/ErrUnsupportedBackend/适配器拒绝/*sandbox 类型化错误` → 环境故障；
`TimedOut` → 数据（map 已有 timed_out）；`ExitCode > 0` → 数据；其余（-1/0 带 error）→ 环境故障。

已知不精确处（如实记下，不为此扩范围去改 executor）：podman 的「输出捕获失败 + 代码非 0 退出」
组合会判为数据，但完整错误文本仍留在 map 的 `error` 键里，类型化信号缺失仅此窄组合。

## B4d 完成记录（实测）

`toolkit.go` 改双写：环境故障 → map（含 `failure` 摘要）+ 非 nil 包装 error；用户代码非 0 退出 /
超时 → 仍为数据。分类顺序：哨兵 → `TimedOut` → `ExitCode > 0` → 其余为环境故障。

未改动 `executor.go/local.go/podman.go/e2b.go` 与 `adapter.go`（仅读它们的错误形状来定规则）。
既有测试 `TestCodeExecutionToolkitReturnsExecutionOutputOnFailure` 在新规则下仍要求 nil，未改即通过。

验证：5 条新分类测试（3 条对旧实现红）；`-race` 含 `pkg/hno/agent`（使用方）全绿；
真实门 25.2s、残留 0；`run` 包覆盖 65.8% → 66.4%。台账 D-33。

后续可选项（需另行授权，不在本轮范围）：
1. 让 `pkg/hno/agent` 在 handler error 分支也渲染结果 map（目前 map 被丢弃）。
2. 给 podman 的「输出捕获故障」加哨兵，消除 D-33 那条窄不精确。

## B4e 完成记录（实测）

交付：文案修正（3 处虚构 CLI 引用 + 1 处假选项名 + provider 截断标记）；两道 DX 门禁
（dx_gate_test.go 文案↔导出面、dx_docs_gate_test.go guide 数字/名字/围栏↔代码）；
新 guide 页 sandbox-runtime.md 并挂上英文侧栏。

门禁先于我看见的 4 个文档错误：sandbox.Context / OverflowFail / WithRunOutputMax /
Provision 应渲染为 2m0s。反证 6 项各命中一次。

未做（需另行决定）：
1. `hno sandbox` CLI —— 文案曾指向它，说明设计里想要；现在文案改指 Go API。
   若真建 CLI，门禁应扩到校验子命令存在。
2. `website/` 无 node_modules，未跑 `npm run docs:build`（需联网安装）。
3. 中文 guide 页未翻译，故未加 zh 链接。

## B4f 工作项 — `hno sandbox` CLI（本轮执行，已获授权）

```yaml
type: planned
targets:
  - cmd/hno/main.go                 (new)  # 薄入口
  - cmd/hno/sandbox.go              (new)  # doctor / prune / run
  - cmd/hno/*_test.go               (new)  # 离线：sandboxtest 提供注入
  - pkg/hno/sandbox/dx_gate_test.go (既有) # CLI 存在后，门禁改校验子命令而非禁 CLI
  - pkg/hno/sandbox/lifecycle.go    (既有) # 文案可恢复指 CLI（现在为真）
  - pkg/hno/sandbox/sandbox.go      (既有) # 同上
  - pkg/hno/sandbox/lockdown.go     (既有) # 同上
allowedWrites:
  - cmd/hno/**                      # 仅新建
  - pkg/hno/sandbox/dx_gate_test.go
  - pkg/hno/sandbox/lifecycle.go    # 仅改可执行文案字符串
  - pkg/hno/sandbox/sandbox.go      # 仅改可执行文案字符串
  - pkg/hno/sandbox/lockdown.go     # 仅改可执行文案字符串
failureClass: contract
```

可行性已实测（不是假设）：sandbox-only 二进制在本机可链接可执行（3.0 MB）；
`cmd/efprobe` 的 TAPI 失败是其 cgo/`-lresolv` 依赖特有，与本 CLI 无关。

安全设计（先定，后写）：
- `prune` 默认**只列不删**（`Sweep`），删除必须显式 `--yes`（`Prune`），并打印 `SkippedPrune()`
  的拒绝项；本会话内我不会执行删除分支，只测「不带 --yes 绝不删」。
- `run` 的退出码分两类：payload 非 0 → 透传该码；环境故障 → 独立退出码，二者不得混淆（D-33 同源）。
- `doctor` 在 `!diag.OK()` 时非 0 退出，便于 CI 当门禁用。

D-34 的收尾承诺：CLI 存在后，门禁必须校验文案里 `hno sandbox <verb>` 的 verb 真存在，
而不是简单解禁 —— 否则只是把一类假阳性换成一类假阴性。

## B4f 完成记录（实测）

交付：`cmd/hno`（doctor/prune/run，薄壳 + 退出码三分）；dx 文案门禁改为**解析 CLI verb 常量**校验
（D-34 承诺的收尾）；guide 新增 shell 章节；Isolation 词汇往返 + 帮助文本词汇测试。

本轮由 CLI 暴露的自身缺陷（都已修并反证）：
1. `--isolation` 帮助文本写了 `micro-vm`，解析器只认 `microvm` ⇒ 词汇由测试钉住。
2. verb 集合读取把「源文件读不到」静默当成「CLI 不存在」⇒ 三态分离 + t.Fatalf。
3. 超时被我的初版分类成环境故障（ExitCode=-1）⇒ 独立 124，且 ≠ 70 有测试。
4. flag→option 断言最初比较构造期记忆值 ⇒ 改读回 `sb.Status()`。

真实引擎：doctor exit 0（8 项）/坏二进制 exit 70 / `go version` 真输出 / exit 5 透传 / prune 只读零删除。
覆盖 cmd/hno 77.2%，核心 73.2% 不变，`-race` 绿，真实门 26.0s 残留 0。

**D-36 待授权**：doctor 对缺失镜像给假绿灯（exit 0 "all 8 checks passed"），同镜像 run 是 exit 70；
`ImagePresent` 能力已实现已测但生产路径唯一调用者是集成测试。需一次 Diagnose image 检查，
代价是检查集合与 CI 绿灯语义变化，故只记录。

## B4g 工作项 — 修 D-36：`Diagnose` 必须回答「这个镜像在这里能起吗」

```yaml
type: planned
targets:
  - pkg/hno/sandbox/provider.go            (new interface ImageQueryable)
  - pkg/hno/sandbox/lifecycle.go           (Diagnose gains the "image" check)
  - pkg/hno/sandbox/sandboxtest/memory.go  (double answers honestly via the attrs channel)
  - pkg/hno/sandbox/provider/docker/docker.go (delegates to the existing dialect path)
  - pkg/hno/sandbox/image_check_test.go    (new)
  - pkg/hno/sandbox/provider/docker/integration_test.go (opt-in real-engine assertion)
  - cmd/hno/hno_test.go                    (doctor exit 1 on an absent image)
  - website/guide/sandbox-runtime.md       (check list gains image)
allowedWrites: 同上；不改 execute/provision 路径，不改 Probe 的「不 pull」范围
failureClass: contract
```

已测事实（决定语义，不是猜）：
- `Diagnose` 当前 8 项里没有镜像项；`Probe` 只在 `refusesCreate` 命中时升级 ⇒ 缺镜像被吞。
- `docker.ImagePresent` 已在两种 dialect 实现并有测试，但**非测试调用者为 0**。
- 非测试 `Diagnose` 调用点只有 `cmd/hno`；核心测试里对 checks 的断言均按 `Env` 名定位 ⇒ 新增一项不破坏它们。
- 9 个测试文件给 memory double 设了非空 `WithImage` ⇒ double 默认必须答「present」，否则反向撒谎。
- 同族先例：`capabilities-without-behaviour` 检查就是「声明了却接不上 ⇒ 不算 OK」。

语义（四态，只有第三种是绿的）：present ⇒ OK；absent ⇒ 不 OK 并给可执行修复；
问了但引擎答不出 ⇒ 不 OK，`Got: "unknown: …"`（D-31「无法验证即拒绝」）；
provider 根本不提供镜像查询 ⇒ 不 OK 并明说（不是假绿，也不是把诚实的 double 判死——double 会答）。

## B4g 完成记录（D-36 已修，实测）

真实引擎复现同一命令的前后对比：
- 修复前：`hno sandbox doctor --image no-such-image-hno:0` ⇒ exit 0，"all 8 checks passed"
- 修复后：⇒ exit 1，`FAIL image … got=absent: no-such-image-hno:0` + 可执行修复，summary "1 of 9 checks failed"

交付：`sandbox.ImageQueryable`（provider 经 dialect 回答）、`Diagnose` 镜像检查四态、
double 的 `AbsentImages`/`ImageQueryErr` 旋钮、provider 离线 4 组测试、真实引擎闭环测试、
CLI 离线钉桩、guide 说明镜像项与「无法验证不算通过」。

计划里预写的语义被实测细化：`WithImage("")` 不是无镜像态（解析到 DefaultImage），
因此「不 OK 只发生在能问到答案却非 present / 问不出结果 / provider 不实现」这三类，
而"无镜像"这一态在解析后的 handle 上不存在——测试已改为钉住「留空不得豁免验证」。

半径实测：非测试调用点 1 处；七包（含 agent）零破坏；覆盖 73.2%→73.3%，docker 59.6%→62.2%。

## B4h 完成记录（真实构建 + 链接门禁）

npm ci（127 包/5s）+ docs:build（22.1s exit 0）+ 渲染证据（页面 115 KB、11 个 h2、侧栏与
prev/next 出现、进 sitemap）。构建污染检查：跟踪文件零改动（dist 与 node_modules 均被 ignore，
rss.xml 生成后仍干净）。

负结果（决定了本轮价值）：`ignoreDeadLinks: true` ⇒ 注入死链 `/guide/does-not-exist` 后构建
仍 68s exit 0。所以「构建通过」不能当链接证据，TestGuideLinksResolve 才是。
它当场抓出注入的死链，也抓出我 siteRoot 少一层（`../../website` → `pkg/website`），
修成自检站点根后再无假报。guide 已恢复原状，链接门禁 0 违规。

全站测量：26 页 / 45 条无法解析（修正扫描器误报后）。翻 `ignoreDeadLinks=false` 属全局变更，未做。

## B4i 完成记录（中文 guide + 跨语言门禁）

zh 页 + zh 侧栏条目落位，真实构建（15.7s exit 0）后以产物核验：页面渲染、侧栏与 prev/next 链接、
sitemap 条目、页内两条锚链 id 与渲染器一致。

门禁改造：guidePages 逐页跑（缺页 = Errorf，不再是 Skipf），限定名集合跨语言相等（反漂移）。
slug 规则由构建产物建表（TestGuideSlugMatchesTheRenderer，7 例），中英各 12 标题逐条比对零偏差。

被实测推翻的两条自定假设：① 以为全角标点都折成连字符——顿号 U+3001 无兼容分解，原样保留；
② 上一轮写的「ASCII 冒号删除」规则对中文页标题并不成立，改为复刻渲染器（先折叠兼容字符）。
验证：-race 四包绿；真实引擎 10 PASS / 0 FAIL / 残留 0；覆盖 73.3% / 78.6% / 62.2% 不变。

## B5a 完成记录（D-38 A1：档位实测）

用户指出「本机有 docker 容器，能否用它测」→ 实测答案比预期强：本机 docker 是 Apple
containerisation 的 shim，4 盒 4 个不同 boot_id（且 guest=Linux 6.18.35 / host=Darwin arm64）
⇒ 每盒一个内核。既有 `Provider.Isolation()` 写死 `IsolationContainer` 在此低报，反方向（引擎
自称 VM 而实际共享内核）准入则无从发现。故档位改由测量决定。

A1（本批，只报告）落地：KernelBoundaryQueryable + docker 实现 + 核心纠正过度声称 +
Diagnose 缓存报告 + `hno sandbox boundary` 动词 + 真实引擎门 private-kernel。
自查发现「测量与 micro-VM 下限矛盾」分支在诚实夹具下**永不可达**（Open 先挡），改用
「声称 VM 档但实测共享内核」的夹具把分支走进去。
自测抓出 2 处自己的缺陷：defer 改值拷贝丢泄漏信息（改具名返回值）；断言打错对象
（Error() 不渲染 Fixes，改断言 UnavailableError.Remediation()）。
另补齐 D-37 未堵完的多语言洞：围栏语法门 / 方法调用门 / 裸名门此前只读英文页。

A2（准入门生效）**未授权未做**：本机 `WithMinimumIsolation(microvm)` 现在失败，A2 后会成功，
属闸门有意放宽，等决策。留白：Docker Desktop 共享 VM 的说法在本机未实测，实现不含任何按名字的结论。
