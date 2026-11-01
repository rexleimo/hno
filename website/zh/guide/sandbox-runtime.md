# 沙盒运行时

`pkg/hno/sandbox` 下这套与 provider 无关的运行时：一个句柄、一份策略、一套错误词汇表，
一次性运行与长生命周期会话共用。本页描述运行时**实际上**报告的内容——下面的数字是解析后的
默认值，`pkg/hno/sandbox/dx_gate_test.go` 里有一条测试会在表格与代码漂移时失败。

如果你在用 `pkg/hno/tools/run` 的 `code_execution`，遗留 `Executor` 接口见
[Sandbox 代码执行](/zh/guide/code-execution-sandbox)，兼容适配器见下面的
[从遗留 Executor 迁移](#从遗留-executor-迁移)。

## 三十秒版本

```go
sb, err := sandbox.Open(ctx,
    sandbox.WithProvider("docker"),
    sandbox.WithImage("golang:1.26"),
    sandbox.WithMinimumIsolation(sandbox.IsolationContainer),
)
if err != nil {
    // Typed: *sandbox.UnavailableError carries what was tried and what to do about it.
    log.Fatal(err)
}
defer sb.Close()

report, err := sb.Run(ctx, sandbox.Shell("go version"))
if err != nil {
    // The environment failed. The report is not trustworthy.
    log.Fatal(err)
}
// A non-zero exit is data about the code, not an error about the sandbox.
fmt.Println(report.ExitCode, report.Stdout)
```

主机上没有引擎时，`Open` 或 `Probe` 会返回一个类型化错误，里面写清它试了哪个引擎、为什么失败、
以及改变结论需要做什么。**它绝不会退回到宿主进程。**

## 你实际拿到的默认值

`Limits`，来自 `sandbox.DefaultLimits()`：

| 字段 | 默认值 | 超出之后 |
| --- | --- | --- |
| `InlineFileMaxBytes` | 1 MiB | 更大的内联文件在运行前就被拒绝 |
| `OutputMaxBytes` | 8 MiB | 单次运行 stdout+stderr 合计上限；超出后按溢出策略处理 |
| `WorkspaceMaxBytes` | 2 GiB | 工作区暂存超过此值直接拒绝 |
| `ArtifactMaxBytes` | 64 MiB | 单个产物的捕获上限 |
| `ArtifactTotalMaxBytes` | 256 MiB | 一次运行的产物总预算 |
| `EnvMaxEntries` | 128 | 条目更多是拒绝，不是截断 |
| `CPUCount` | 0 | 0 表示交给引擎决定；用 `WithLimits` 设置 |
| `MemoryMB` | 0 | 0 表示交给引擎决定 |
| `PidsLimit` | 0 | 0 表示交给引擎决定 |
| `DiskMB` | 0 | 0 表示交给引擎决定 |
| `SessionMaxConcurrentRuns` | 1 | 会话默认串行 |
| `SnapshotKeep` | 4 | 超出此数的旧快照会被清理 |
| `SnapshotTTL` | 1h0m0s | 比这更旧的快照会被回收 |
| `DefaultMountMode` | copy-snapshot | 默认是快照，不是宿主实时 bind mount |
| `Overflow` | drop | 超出上限的输出被丢弃；`OverflowFault` 会用类型化错误中止运行 |

`Timeouts`，来自 `sandbox.DefaultTimeouts()`：

| 字段 | 默认值 | 作用域 |
| --- | --- | --- |
| `Probe` | 5s | 引擎可达性探测 |
| `Connect` | 3s | 只覆盖拨号预算，不覆盖创建任何东西 |
| `Provision` | 2m0s | 控制面步骤：create 加 start |
| `Exec` | 30s | 单个请求的执行 |
| `Setup` | 30s | 安装/setup 步骤 |
| `Collect` | 30s | 产物捕获 |
| `Drain` | 200ms | 进程退出后的最后一次输出抽取 |
| `Idle` | 15m0s | 会话空闲到这个长度即可被回收 |
| `SessionTTL` | 30m0s | 会话硬性存活期 |
| `Cleanup` | 10s | 引擎清理步骤 |
| `ApprovalHold` | 5m0s | 挂起的批准可被答复的时长 |
| `BoundaryFresh` | 30m0s | 一份内核边界测量可用于放行「低于下限的 provider」的有效期；0 表示取默认，负数表示每次 Open 都要重测 |

字节上限是**每句柄**的；只有 `WithOutputLimit` 能收紧单次运行。这套 API 不存在逐次运行的
CPU 与内存上限——它们在句柄上用 `WithLimits` 设置，也正是兼容适配器拒绝伪造的东西。

## 排查一次运行之前，先问句柄

```go
diag, err := sb.Diagnose(ctx)
fmt.Println(diag.Summary) // Summary, Reached, Checks and Fixes are fields, not methods
for _, c := range diag.Checks {
    if !c.OK {
        fmt.Printf("%s: want %s, got %s — %s\n", c.Env, c.Want, c.Got, c.Fix)
    }
}
for _, f := range diag.Fixes {
    fmt.Println(f)
}
```

`Diagnose` 会报告：应答的 provider 是谁、隔离下限与你实际得到的隔离、provider 声明了但核心
无法据此行动的能力、句柄绑定的镜像**是否真能在这里启动**、`ReadyProbe` 与 `Expose` 在这里是否
会被兑现、活跃会话数、待答复的批准，以及泄漏的环境。

镜像检查是刻意与 probe 分开的两件事。probe 问的是引擎到底能不能 create，并且故意不做 pull；
一个对此愉快应答的引擎，仍然可能本地根本没有你的那份镜像。能回答的 provider 实现
`sandbox.ImageQueryable`，三种可能答案对应三行不同的报告——present、absent、以及*「引擎不肯说」*，
其中只有第一种算通过。无法回答的镜像问题会被报成**失败**，而不是四舍五入成健康，因为一句
「没人检查过」却显示绿灯，正是这项检查存在的原因。

每条修复提示都会由一条测试拿去对照包的导出面核查，所以提示不可能让你去调一个不存在的选项，
或者跑一个从未发布的命令。

## 崩溃之后泄漏的容器

引擎侧持有、而本句柄已不再拥有的环境，是可观测、可回收的，并且与健康报告分开：

```go
if r, ok := sb.Reclaimer(); ok {
    leaked, _ := r.Sweep(ctx) // look first: this deletes nothing
    removed, _ := r.Prune(ctx) // reclaim what it reported
    _ = leaked
    _ = removed
}
```

`Sweep` 问的是引擎，而不是进程内的状态——这是看见崩溃留下了什么的唯一办法。`Prune` 拒绝删除
被标记为另一个存活进程所有的容器，并且会把原因说出来：docker provider 的 `SkippedPrune()`
返回它拒绝了什么以及为什么。

## 需要人来答复的批准

返回 `ActRequireApproval` 的策略会把工作挂在**同一个环境**里：

```go
sess, err := sb.Session(ctx, "build-42", sandbox.WithCreateIfMissing(true))
if err != nil {
    log.Fatal(err)
}
report, err := sess.Exec(ctx, sandbox.Shell("deploy"))
var pending *sandbox.ApprovalPendingError
if errors.As(err, &pending) {
    fmt.Println(pending.ApprovalID, pending.Reason, pending.ExpiresAt)
    fmt.Println(pending.Remediation())
}

for _, p := range sess.Awaiting() {
    fmt.Println(p.ID, p.BoundToEnvironment())
}
_ = sess.Approve(ctx, pending.ApprovalID, "rex")
report, err = sess.Resume(ctx, pending.ApprovalID)
```

一次批准，授权的是**人当时看到的那个环境**。如果盒子在其间丢失或被重建，`Resume` 会以
`*ApprovalEnvironmentChangedError` 拒绝，并且不会消耗这次授权。批准的过期时间取
`Approval.Expires` 与 `Timeouts.ApprovalHold` 中较短的那个；若有批准正挂着没人答复的工作，
`Diagnose` 会报告出来。

发现一次挂起的运行**不需要** `Observer`：`Session.Awaiting()` 自己就够用。

## 在图里跑批准

`pkg/hno/sandbox/graph` 把一个沙盒请求变成使用图自身 interrupt 与 checkpoint 机制的节点——
不存在第二套等待机制：

```go
node := sgraph.Node("deploy", sess, sandbox.Shell("deploy"))
result, err := g.Run(ctx, input)
```

节点挂起时，interrupt 的载荷是一个 `sgraph.Ask`，带着会话、批准 id 与问题本身。之后从另一个进程
答复它是：

```go
out, err := ask.Answer(ctx, sb, true, "rex")
```

拒绝是一条分支，不是故障：`Result.Refused` 与 `Result.Reason`，除非用 `WithRefusalFails()`
要求返回错误。载荷自身非零退出仍然作为数据留在 `Result.Run` 里。

## 项目、文件与输出

`RunProject` 在同一份工作区快照上按 phase 执行，并收集声明过的产物：

```go
res, err := sb.RunProject(ctx, sandbox.Project{
    Phases: []sandbox.Phase{
        {Name: "build", Run: sandbox.Shell("go build ./..."), Required: true},
        {Name: "test", Run: sandbox.Shell("go test ./..."), Required: true},
    },
    Artifacts: []sandbox.ArtifactSpec{{Path: "bin/cli", Required: true}},
})
```

声明了却缺失的产物会让整个项目失败；未声明的文件被丢弃。`Files` 是内联通道（见
`InlineFileMaxBytes`），`Workspace` 把一棵目录树暂存进去，`Outputs` 把声明过的路径拷回来。
暂存过程中的符号链接逃逸会被拒绝。

## 从遗留 Executor 迁移

`NewSandboxExecutor` 在运行时之上实现了被冻结的 `Executor` 签名，因此已有的
`code_execution` 工具包可以在不改调用方的前提下迁移：

```go
ex, err := run.NewSandboxExecutor(run.SandboxConfig{
    Image:      "golang:1.26",
    Limits:     sandbox.Limits{MemoryMB: 512},
    MinIsolation: sandbox.IsolationContainer,
})
if err != nil {
    log.Fatal(err)
}
tk := run.NewToolkit(ex)
```

适配器选择拒绝而不是悄悄改解语义，且拒绝都是类型化的：

- `Spec.Workspace` 原本是宿主的读写 bind mount；运行时是把快照暂存进去、再把声明过的输出拷出来。
  请迁移到 `RunProject` 或 `WithWorkspace`。
- 逐次运行的 `Spec.MemoryLimit` / `Spec.PidLimit` 无法兑现——上限设在句柄上。与句柄取值不同即
  `ErrSandboxLimitMismatch`。
- `podman` 与 `e2b` 在本构建中没有注册 provider。它们被拒绝，而不是被悄悄换成 `docker`。
- 引擎缺失仍然会浮出遗留的 `ErrNoProvider` 哨兵；超时会同时以包装后的 `ErrTimeout` 和
  `Result.TimedOut` 到达。

## 工具包里的失败可见性

`run_code` 区分过去看起来一模一样的两种失败：

- 载荷跑了并以非零退出 → 返回结果 map，不返回 error。模型把它当作关于自己代码的数据来读。
- 环境没能跑这个载荷 → map **照旧返回**，同时返回一个真正的 error，于是 agent 的 logger、
  逐次调用结果、以及任何监控系统看到的都是失败，而不是「带着一行丧气话的成功」。

超时属于载荷数据（map 里已有 `timed_out`）；分类只使用这一层能证明的东西，所以一个正常工作的
脚本永远不会被报成事故。

## 从命令行运行 hno sandbox

`cmd/hno` 的存在，是为了让运行时已经知道的事情不必写一个 Go 程序就能读到。它不加入任何自己的
策略——每个动词都是对一个导出调用的薄壳，每个 flag 都以它设置的选项命名。

```bash
go build -o hno ./cmd/hno

./hno sandbox doctor                      # why can't this run here?
./hno sandbox doctor --json               # same verdict, machine-readable
./hno sandbox prune                       # list what this host left behind (read-only)
./hno sandbox prune --yes                 # delete them
./hno sandbox run -- go version           # one command, payload status passed through
./hno sandbox run --timeout 30s --shell "deploy"
./hno sandbox run --attr binary=container --image golang:1.26 -- sh -c "exit 5"
./hno sandbox boundary                      # 隔离度是实测还是名字？（会真的起盒）
./hno sandbox boundary --boxes 3 --expect private-kernel --json
```

| 动词 | 运行时调用 | 退出码 |
| --- | --- | --- |
| `doctor` | `Sandbox.Diagnose` | `0` 全部检查通过，`1` 有检查失败 |
| `prune` | `Sandbox.Reclaimer` | `0`；不加 `--yes` 拒绝删除 |
| `run` | `Sandbox.Run` / `Session.Exec` | 程序自己的退出码，`124` 超时，`70` 环境不可用，`2` 命令行写错 |
| `boundary` | `Sandbox.MeasureKernelBoundary` | `0` 测得且与 `--expect` 相符，`1` 结论为 `unknown` 或不符，`70` 无法测量，`2` 参数写错 |

`run` 的三种结果刻意不做合并。一个退出码为 5 的程序不等于一个失败的沙盒；而一个没能启动的沙盒
对你的程序没有任何判断：`70` 意味着**你的命令状态未知**，所以外层脚本不能把它读成「跑了但失败」。
`124` 沿用 `timeout(1)` 的约定。这与
[工具包里的失败可见性](#工具包里的失败可见性) 是同一套切分。

`--attr k=v` 是引擎配置通道（`sandbox.WithAttrs`），换另一个容器二进制就靠它
（`--attr binary=container`），指定 `runsc` 这类引擎 runtime 也是它。`--isolation` 只接受
`Isolation.String` 渲染出的那些名字——`none`、`container`、`syscall-filtered`、`microvm`、
`microvm-hosted`——解析器拒绝的取值会让命令停下来，而不是悄悄在你要求的下限之下运行。

prune 默认只读，因为一个靠猜的回收工具比一个会问的更糟；而引擎拒绝删除的那些会被打印出来，
不会藏在零计数里。

## 隔离度是实测出来的，不是按名字给的

上面这套阶梯说的是 provider **声称** 什么；还有一个调用说引擎**实际**做什么，而这个差别是
要紧的：在这套运行时被开发出来的那台主机上，`docker` 并不是 Docker Desktop，而是套在 Apple
containerisation 引擎上的兼容 shim（`docker info` 报 `Apple Containerization`），而且它起的
每一个盒都用自己的内核。四个盒给出四个不同的
`/proc/sys/kernel/random/boot_id` 与四个不同的 `/proc/uptime` 读数。
按 provider 名字分配档位，就会把一台正在发 VM 的引擎叫作「容器隔离」——而同一个二进制装在一台
共享内核的 Linux 宿主上说的却是实话，这正是为什么任何「按名字映射」都不可能普遍正确。

```go
report, err := sb.MeasureKernelBoundary(ctx, sandbox.DefaultBoundaryBoxes)
if err != nil {
    var budget *sandbox.BoundaryBudgetError
    var unsupported *sandbox.BoundaryUnsupportedError
    switch {
    case errors.As(err, &budget):
        fmt.Println(budget.Remediation()) // box count refused before anything booted
    case errors.As(err, &unsupported):
        fmt.Println(unsupported.Remediation())
    default:
        log.Fatal(err)
    }
}
fmt.Println(report.Verdict, report.Boxes, report.Distinct(), report.GuestKernel)
for _, line := range report.Evidence {
    fmt.Println(line)
}
```

四种结论，彼此不可混用：`private-kernel`、`shared-kernel`、`unknown`、`unmeasured`。
只有第一种支持 micro-VM 下限；`unknown` 表示引擎给出的读数不足以支撑任何结论，而它从命令行
返回**非零**，不会伪装成健康。读数与结论一起返回——每盒一个 guest boot id、每盒自内核启动以来的
秒数、以及 guest 内核与宿主内核的对照——因为一个你无法核查的结论只是句宣称。

两条成本规则，都来自这个包自己已经犯过的错：

- 为了发现「这个问题答不了」而不起任何盒。没有 `sandbox.KernelBoundaryQueryable` 的
  provider 会拿到点名该接口的类型化错误（与 D-36 的镜像问题同一家规），非法的盒数在任何启动之前就被拒绝。
- 这个测量会真的起盒，所以它必须是一次**有意的**调用：`sandbox.MaxBoundaryBoxes`
  把单次测量限制在 `sandbox.DefaultBoundaryBoxes` 或略多。它**绝不**属于 `Diagnose`。
  但一旦你测过，`Diagnose` 就会把缓存的结论拿去对照本句柄给的下限，于是过度声称会浮现在
  健康报告里，而不是留在跑过测量的那个人手里。

核心还会拒绝「自己的证据不支持」的结论：provider 报 `private-kernel` 却交回两个相同的 boot id
时，会被纠正为 `shared-kernel` 并把原因写进 `Fixes`。**由读数决定，不由宣称决定。**

本批次落地的是「报告测量结果」；micro-VM **下限**目前仍按 provider 的声称准入。让准入改用
测量结果，是一道闸门的**有意放宽**，所以它是你的决策，而不是顺手产生的副作用——见下面的
`hno sandbox boundary`。

### 让测量结果决定准入

上一节只是报告。这一节让报告改变**允许什么**，也是整套运行时里唯一一处「证据可以**放宽**闸门」
而不是只能收紧的地方——所以它要**两道**自愿：测量必须存在，而且必须被显式交进来。

```go
report, err := probe.MeasureKernelBoundary(ctx, sandbox.DefaultBoundaryBoxes)
if err != nil {
    log.Fatal(err)
}
sb, err := sandbox.Open(ctx,
    sandbox.WithProvider("docker"),
    sandbox.WithImage("golang:1.26"),
    sandbox.WithMinimumIsolation(sandbox.IsolationMicroVM),
    sandbox.WithBoundaryMeasurement(*report),
)
```

两个方向，只有一个是新的：

- `private-kernel` 的测量会放行**声称低于下限**的 provider。在本页描述的那台主机上，这就是
  `IsolationMicroVM` 被拒与被兑现的差别：引擎把每个盒放进自己的 VM，却声称容器隔离，于是单靠声称
  永远满足不了它。
- `shared-kernel` 的测量会**拒绝**声称 VM 档的 provider。单靠声称本会通过，测量是唯一能拦住它的
  东西。这个方向是收紧，永远被采纳。

`Status` 在 `Isolation` 里仍然报 provider 自己的声称，并把证据放在 `Boundary` 里，因为改写声称
等于告诉所有下游读者「这个引擎换了性质」。`Diagnose` 与准入结论一致，并在 isolation 那一行里
说明，不会自相矛盾。

证据是会被花掉的，而不是被吞掉。来自**别的 provider**、在**别的宿主**上测的、或比
`Timeouts.BoundaryFresh` 更旧的报告，都不构成放行的理由——而拒绝会指明是这三种里的哪一种，
否则结果就是让人去重测一台本来就测得好好的引擎。`unknown` 结论对两个方向都不起作用。

从命令行走，flag 是 `--measure-boundary`，它不免费：它会在运行之前先起盒。

```bash
hno sandbox run --isolation microvm --measure-boundary -- go version
```

## 配方

**钉住镜像，并拒绝任何比容器更松的隔离：**

```go
sb, _ := sandbox.Open(ctx,
    sandbox.WithProvider("docker"),
    sandbox.WithImage("python:3.12-slim"),
    sandbox.WithMinimumIsolation(sandbox.IsolationContainer),
    sandbox.WithPolicy(sandbox.PolicyFunc(func(_ context.Context, rc *sandbox.RunContext) (sandbox.Decision, error) {
        if rc.Request.Egress.Mode == sandbox.EgressUnset {
            return sandbox.Decision{Action: sandbox.ActDeny, Reason: "egress posture not declared"}, nil
        }
        return sandbox.Decision{Action: sandbox.ActAllow}, nil
    })),
)
```

**一个长期盒子、多条命令、重启之后继续：**

```go
sess, _ := sb.Session(ctx, "notebook", sandbox.WithCreateIfMissing(true))
_, _ = sess.Exec(ctx, sandbox.Shell("pip install pandas"))
_, _ = sess.Exec(ctx, sandbox.Shell("python analyze.py"))
_ = sess.Release(ctx)
```

重新获取一个环境已丢失的会话，在 provider 声明 `CapRebuild` 时是按记录的 spec 重建；
某一步被跳过，只在重建出来的盒子**确实**带着那份状态时。

**先问再做破坏性的事，并让人在带外答复：**

```go
sb, _ := sandbox.Open(ctx,
    sandbox.WithProvider("docker"),
    sandbox.WithImage("postgres:16"),
    sandbox.WithPolicy(policy),
    sandbox.WithTimeouts(sandbox.Timeouts{ApprovalHold: 30 * time.Minute}),
)
```

`Diagnose` 会点名任何正挂着工作的批准；`Session.Awaiting()` 把它们列出来；
`Approve`/`Deny`/`Resume` 在同一个环境里完成这次交互。

**在把话写进 runbook 之前，先看运行时会说什么：**

```go
diag, _ := sb.Diagnose(ctx)
failed := false
for _, c := range diag.Checks {
    if !c.OK {
        failed = true
        fmt.Printf("%s: %s\n", c.Env, c.Fix)
    }
}
if !failed {
    fmt.Println("ready")
}
```
