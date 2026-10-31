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
