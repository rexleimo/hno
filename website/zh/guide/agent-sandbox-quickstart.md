# 从零到 Agent 边写边跑

这一页只有一个目标：**照着做，能让 Agent 生成的代码在沙盒里真的跑起来**。上一页
[沙盒运行时](/zh/guide/sandbox-runtime) 讲的是契约和默认值，这一页讲操作顺序。

四个场景，都在 `golang:1.26` 镜像上真实跑过（文末附实测输出）：

1. 先把文件写进去，再跑命令（最常见，也最容易踩坑）
2. 边生成边验证（Agent 每写一个文件就问一次沙盒）
3. 跑完了让结果自己驱动下一步（不用人盯着）
4. 一整棵项目树 + 多个阶段 + 收集产物

---

## 第 0 步：装了什么就能用？（这一节回答「还要不要额外配置」）

**装了 Go 依赖还不够。** 有三件事必须自己明确做，否则第一次 `Open` 就会失败——下面每条都是
实测报错，不是推测。

### (1) provider 必须显式注册，不会自动发现

不 import provider 包时，`Open` 直接拒绝：

```
no sandbox provider can meet this request (isolation floor container): no provider
could be used for "docker"; fallback is disabled (FailFast per D-13)
  - docker: not registered (fix: import a provider package, ...)
```

`D-03` 是刻意的：不做 `init()` 扫 PATH。所以二选一：

```go
import _ "github.com/rexleimo/agno-go/pkg/hno/sandbox/provider/docker" // 只要副作用
```

或者显式注册（推荐，错误能自己处理）：

```go
// 在引入它们的包里各注册一次即可（例如 main 或 provider 装配处）。
// docker / boxlite 都各自提供 Register() 和 NewProvider(ProviderConfig)。
if err := docker.Register(); err != nil {
    log.Fatal(err)
}
// 想用微虚拟机档时再加上：
//   if err := boxlite.Register(); err != nil { log.Fatal(err) }
```

### (2) 宿主机要有引擎，而且要用对二进制名

| provider | 需要宿主具备 | 常见坑 |
|---|---|---|
| `docker` | `docker`（或 `podman`）可执行，daemon 在跑 | macOS 上 `docker` 可能只是 Apple containerisation 的 shim；用 `WithAttrs{"binary": "container"}` 直连 |
| `boxlite` | `boxlite` 可执行 + **硬件虚拟化可用** | 容器里跑不了（无 `/dev/kvm`），见下方说明 |

`boxlite` 的可用性**不按操作系统猜**，而是问引擎：`boxlite info` 报
`virtualization: available` 才通过；报 `unavailable` 或干脆不回答，`Probe` 都会拒绝并给出去处。

> **实测**：在 Apple containerisation 的 guest 里（arm64 与 amd64 都试过）`/dev/kvm` 不存在、
> CPU 无 `vmx/svm`，所以**容器里跑不了 BoxLite**。它需要真实 Linux 宿主 + KVM 可访问。

### (3) 会话恢复需要额外开关

`sess := sb.Session(ctx, "my-id")` 对一个**还没存在的 id** 会失败：

```
session "my-id" no longer has a live environment: this handle has no session store,
so an unknown id cannot be verified as a resumption; use WithCreateIfMissing to start fresh
```

第一次创建必须加：

```go
sess, err := sb.Session(ctx, "my-id", sandbox.WithCreateIfMissing(true))
```

想要跨进程/跨重启复用同一个 id，再配 `sandbox.WithSessionStore(...)`；否则 id 只在本进程有效。

### (4) 镜像要先在本地存在

`doctor` 会如实告诉你缺哪个镜像，并给出拉取指引：

```
FAIL image  want=present in the local store got=absent: golang:1.26
     fix: pull or build golang:1.26 in this engine, or point WithImage at an image it already has
```

先自检，再跑业务：

```bash
hno sandbox doctor --provider docker --image golang:1.26
# 或者 Go 里： sb.Diagnose(ctx)
```

---

## 场景 1：先写文件，再跑命令

这是「Agent 输出代码 → 执行」的最小闭环。关键点：**写和跑是两个不同的契约**——
`Files()` 负责写入，`Session.Exec()` 负责执行，前者成功后不代表后者能跑。

```go
sb, err := sandbox.Open(ctx,
    sandbox.WithProvider("docker"),
    sandbox.WithImage("golang:1.26"),
    sandbox.WithAttrs(map[string]any{"binary": "container"}),
)
if err != nil { log.Fatal(err) }
defer sb.Close()

sess, err := sb.Session(ctx, "agent-writes-then-builds", sandbox.WithCreateIfMissing(true))
if err != nil { log.Fatal(err) }
defer sess.Release(ctx)

// 写入通道
files, err := sess.Files()
if err != nil { log.Fatal(err) }          // 没有 FileBridge 能力时会返回 *CapabilityError
if err := files.MkdirAll(ctx, "hello", 0o755); err != nil { log.Fatal(err) }
if err := files.Write(ctx, "hello/main.go", src, 0o644); err != nil { log.Fatal(err) }
if err := files.Write(ctx, "hello/go.mod", []byte("module hello\n\ngo 1.26\n"), 0o644); err != nil { log.Fatal(err) }

// 执行通道：工作目录是 /workspace，写进去的相对路径就在这下面
rep, err := sess.Exec(ctx, sandbox.Shell("cd hello && GOFLAGS=-mod=mod go build -o /tmp/app . && /tmp/app"))
if err != nil { log.Fatal(err) }           // 引擎坏了才是 error
fmt.Println(rep.ExitCode, rep.Stdout)      // 非 0 退出码是「代码的问题」，不是 error
```

实测输出：

```
listed: main.go|82|regular file size=0
--- exit=0 ---
stdout: hello from the sandbox
```

**两个坑，都踩过：**

- **没 `go.mod` 就没有模块上下文**：报 `go: go.mod file not found in current directory or any
  parent directory`。Agent 只写 `main.go` 时一定要同时给 `go.mod`。
- **`List` 报 `size=0`** 是目录条目的常见表现，不代表文件是空的——用 `Read` 或 `Stat` 确认内容。

### 两种写入通道，别混用

**通道 A：`sess.Files()`** —— 适合「边写边验」，因为写完立刻能读、能 List、能在下一次 `Exec` 里看到。

**通道 B：请求内联输入** —— 适合「一次性把文件交给这次运行」，不需要先建会话文件句柄：

```go
rep, err := sess.Exec(ctx, sandbox.Request{
    Runtime: "shell",
    Code:    "cat gen/app.py && python3 gen/app.py",
    Inputs: []sandbox.Input{{Path: "gen/app.py", Bytes: []byte("print('inline input ran')\n")}},
})
```

实测：`inline exit=0 stdout="print('inline input ran')\ninline input ran\n"` —— 文件在命令执行前已经落盘。

**通道 C：整个工作区来自宿主** —— 不用一个个写文件：

- `sandbox.FromDir("...")`：把宿主机目录作为工作区来源
- `sandbox.FromArchive(reader)` / `sandbox.FromGit(url, ref)` / `sandbox.FromImage(image)` / `sandbox.FromArtifacts(set, sub)`
- 这些返回 `*sandbox.WorkspaceSource`，通过 `sandbox.WithWorkspace(...)` 或 `sandbox.Project.From` 使用

想让它「重建之后还在」，用 `sess.RecordInput(ctx, in)` 把输入登记进 spec —— 没登记的写入不会
在重建时重现，这是「能跑一次」和「可复现」的区别。

---

## 场景 2：边生成边验证（Agent 边写边跑）

这才是「Agent 生成过程中写入」的正解：**每写一个文件就跑一次轻量校验**，把沙盒当反馈回路，
而不是等全部写完再一次性构建。

```go
steps := []struct{ path, body string }{
    {"app/go.mod", "module app\n\ngo 1.26\n"},
    {"app/main.go", "package main\n\nfunc main() { println(Greet()) }\n"},
    {"app/greet.go", "package main\n\nfunc Greet() string { return \"hello\" }\n"},
}
_ = files.MkdirAll(ctx, "app", 0o755)
for i, st := range steps {
    if err := files.Write(ctx, st.path, []byte(st.body), 0o644); err != nil { log.Fatal(err) }
    rep, err := sess.Exec(ctx, sandbox.Shell("cd app && GOFLAGS=-mod=mod go vet ./..."))
    if err != nil { log.Fatal(err) }
    if rep.ExitCode != 0 {
        // 把 stderr 交回给模型，让它重写这一个文件，而不是重写整棵树
        fmt.Printf("step %d failed: %s\n", i+1, rep.Stderr)
    }
}
```

实测输出（注意第 2 步：文件还没写完，`vet` 立刻报出真实错误）：

```
step 1 wrote app/go.mod     vet exit=0 go: warning: "./..." matched no packages
step 2 wrote app/main.go    vet exit=0 # app
                                   vet: ./main.go:3:23: undefined: Greet
step 3 wrote app/greet.go   vet exit=0
final build exit=0
```

> ⚠️ **这里有个必须知道的坑**：`go vet ./... | head -5` 的 **退出码是 `head` 的**，所以上面
> step 2 明明有编译错误却显示 `exit=0`，而 stderr 里躺着 `undefined: Greet`。**不要用管道截断
> 一个要被判断成败的命令**——要么直接跑、要么 `set -o pipefail`，否则你的 Agent 会「绿灯
> 通过」一个编译失败的项目。

---

## 场景 3：跑完之后，让结果自己驱动下一步

需求里说的「执行完了之后怎么引导 Agent 去做后续的事情」有两种做法，不要混：

### (a) 用失败分类决定下一步（机械、可判定）

`Report.Failure` 已经分好类，不用自己解析字符串：

```go
rep, err := sess.Exec(ctx, sandbox.Shell("make build"))
switch {
case err != nil:
    // 环境坏了：重建/重试，别让模型改代码
case rep.ExitCode != 0 && rep.Failure.NextAction == sandbox.ActionModelFix:
    // 模型的问题：把 rep.Stderr 回灌，让它改
case rep.ExitCode != 0:
    // 其他（超时、丢失、配额）：换策略，不是改代码
}
```

### (b) 边跑边收输出，实时决定要不要打断

用 `WithRunSink` 挂一个 sink，输出是**边产生边到**的，`Report.OutputSent` 记录实时送达的字节数：

```go
sink := &mySink{} // 实现 Emit(ctx, sandbox.OutputChunk) error
rep, err := sess.Exec(ctx,
    sandbox.Shell("npm run build 2>&1"),
    sandbox.WithRunSink("collect", sink),
)
// rep.OutputSent > 0 证明「流式送达」是真的发生了，而不是最后给一整块
```

实测输出：

```
run1 exit=2 chunks=4 outputSent=34
run1 stderr saw failure marker: true        ← sink 在运行中就看到了 BUILD_FAILED
next action derived from failure class: true ← Failure.NextAction == ActionModelFix
```

这条链路就是「执行完 → 判断 → 下一步」的自动化版本：sink 拿到实时输出做判断，`Report.Failure`
提供机械的下一步建议，模型只在 `ActionModelFix` 时被叫回来。

---

## 场景 4：一整棵项目树 + 多阶段 + 产物

单个文件用 `Exec` 就够；**整棵项目**要用 `Project`：阶段按顺序跑、共享同一个工作区、
产物只回收你声明的那些。

```go
proj := sandbox.Project{
    ID: "agent-project",
    Phases: []sandbox.Phase{
        {Name: "scaffold", Required: true, Run: sandbox.Shell(scaffoldScript)},
        {Name: "vet",      Required: true, Run: sandbox.Shell("cd app && GOFLAGS=-mod=mod go vet ./...")},
        {Name: "run",      Required: true, Run: sandbox.Shell("cd app && GOFLAGS=-mod=mod go run . > /workspace/out.txt")},
    },
    Artifacts: []sandbox.ArtifactSpec{{Path: "out.txt", Required: true}},
}
res, err := sb.RunProject(ctx, proj)
if err != nil { log.Fatal(err) }
fmt.Println(res.Status, res.ExitCode)
for _, a := range res.Artifacts {
    fmt.Printf("%s = %q\n", a.Path, string(a.Content()))
}
```

实测输出：

```
project status=succeeded exit=0 phases=3
  phase scaffold exit=0
  phase vet   exit=0
  phase run   exit=0
  artifact out.txt 2 bytes: "5\n"          ← 真的是 sum(2,3) 算出来的
```

要点：

- `Required: true` 的阶段失败会中止后续；`FailOpen: true` 适合 lint 这类「结果也是数据」的阶段。
- `Artifacts` 只回收声明的路径；没声明的东西跑完就随沙盒一起消失。
- `Project` 没有内联 `Inputs` 字段——**先写树再跑**（scaffold 阶段），或者用
  `From: sandbox.FromDir(...)` 把宿主机目录作为工作区来源。

---

## 场景 5（进阶）：不确定就先问，别猜

宿主环境不确定时，先 `Diagnose`，它逐项回答「这个镜像能不能起、隔离实际是什么档、有没有孤儿」：

```go
diag, err := sb.Diagnose(ctx)
// DiagReport 全是字段，没有方法；逐项打印 Checks 与 Fixes
```

命令行等价物：

```bash
hno sandbox doctor  --provider docker --image golang:1.26
hno sandbox boundary --provider docker --boxes 2     # 实测隔离档，不是按名字猜
hno sandbox run     --provider docker --image golang:1.26 -shell 'go version'
```

退出码：`0` 成功 / `1` 检查跑完但没通过 / `70` 环境不可用 / `2` 用法错误 / `124` 超时。

---

## 这一页用到的 API 面

下面每个名字当前都是导出的；文档门禁会校验这一点（页面里编造 API 会让构建失败）。

| 名字 | 它是什么 |
|---|---|
| `sandbox.WithProvider` | 按名字选择已注册的 provider |
| `sandbox.WithImage` | 这个句柄里每个盒子启动用的镜像 |
| `sandbox.WithAttrs` | 引擎透传参数（`binary`、`registry` 等） |
| `sandbox.WithMinimumIsolation` | 句柄必须达到的隔离下限 |
| `sandbox.WithCreateIfMissing` | 允许 `Session` 为没见过的 id 现场创建 |
| `sandbox.WithSessionStore` | 让 session id 跨进程存活 |
| `sandbox.WithRunSink` | 为单次运行挂实时输出 sink |
| `sandbox.FileService` | `sess.Files()` 背后的写入通道 |
| `sandbox.ArtifactSpec` | 一个要回收的声明式产物 |
| `sandbox.DiagReport` | `Diagnose` 的结果：全是字段，没有方法 |
| `sandbox.ActionModelFix` | 该让模型重写时 `Report.Failure.NextAction` 的取值 |
| `sandbox.ClassUserCode` | payload 非 0 退出时 `Report.Failure.Class` 的取值 |
| `docker.Register` / `boxlite.Register` | 显式注册 provider（D-03） |

## 一页速查

| 我要做的事 | 用哪个 API | 容易忘的前提 |
|---|---|---|
| 写文件进沙盒 | `sess.Files()` → `Write/MkdirAll` | provider 要有 `CapFileBridge`，否则 `*CapabilityError` |
| 跑一条命令 | `sess.Exec(ctx, sandbox.Shell(...))` | 工作目录是 `/workspace`；非 0 退出码不是 error |
| 边跑边收输出 | `sandbox.WithRunSink(name, sink)` | 用 `Report.OutputSent` 证明真的流式送达 |
| 跑整棵项目 | `sb.RunProject(ctx, proj)` | `Project` 无内联 Inputs，先写树或用 `FromDir` |
| 判断下一步 | `Report.Failure.Class` / `.NextAction` | 别去解析 stderr 文本 |
| 首次创建会话 | `sandbox.WithCreateIfMissing(true)` | 否则未知 id 会被拒绝 |
| 宿主自检 | `sb.Diagnose(ctx)` / `hno sandbox doctor` | 镜像不在本地会 FAIL，并给拉取指引 |
| 实测隔离档 | `sb.MeasureKernelBoundary(ctx, 2)` | 要真的起 2 个盒子，不是免费操作 |

**最小可运行前提清单**：① import 并注册 provider；② 宿主有引擎可执行文件（并选对 binary 名）；
③ 镜像已在本地；④ 首次 `Session` 加 `WithCreateIfMissing(true)`。四件事都做到，上面的代码就能跑。
