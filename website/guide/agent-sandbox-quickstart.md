# From zero to an agent that writes and runs

One goal: **follow this and the code your agent generates will actually run in the sandbox**.
The [Sandbox runtime](/guide/sandbox-runtime) page states the contract and the defaults; this
page gives the order of operations.

Every scenario below was executed against a real engine on the `golang:1.26` image, and the
quoted output is the measured output.

1. write files first, then run them (the most common shape, and the easiest to get wrong)
2. verify as you generate (the agent asks the sandbox after every file)
3. let the result drive the next step (nobody watches the run)
4. a whole project tree, ordered phases, collected artefacts

---

## Step 0: what do I have to configure? (the "is installing the dependency enough?" question)

**No — adding the Go dependency is not enough.** Four things must be true or the first `Open`
fails. Each item below quotes the real error that was produced on this machine, not a guess.

### (1) A provider is registered explicitly — never discovered

Without importing a provider package, `Open` refuses:

```
no sandbox provider can meet this request (isolation floor container): no provider
could be used for "docker"; fallback is disabled (FailFast per D-13)
  - docker: not registered (fix: import a provider package, ...)
```

That is deliberate (D-03: no `init()` PATH scanning), so choose one of the two:

```go
import _ "github.com/rexleimo/agno-go/pkg/hno/sandbox/provider/docker" // side effect only
```

Or register explicitly — preferred, because you get to handle the error:

```go
// Register each provider once, in whichever package wires them up (main is fine).
// Both packages expose Register() and NewProvider(ProviderConfig).
if err := docker.Register(); err != nil {
    log.Fatal(err)
}
// For the micro-VM tier, also register boxlite:
//   if err := boxlite.Register(); err != nil { log.Fatal(err) }
```

### (2) The host needs an engine, under the right binary name

| provider | host requirement | usual trap |
|---|---|---|
| `docker` | a `docker` (or `podman`) executable with a running daemon | on macOS `docker` may be a shim for Apple containerisation; point at it with `WithAttrs{"binary": "container"}` |
| `boxlite` | a `boxlite` executable **and working hardware virtualisation** | cannot run inside a container (no `/dev/kvm`) — see below |

BoxLite availability is **not guessed from the operating system**; the engine is asked.
`boxlite info` must report `virtualization: available`. If it reports `unavailable`, or does not
answer at all, `Probe` refuses and says where to go.

> **Measured**: inside an Apple containerisation guest (both arm64 and amd64 were tried)
> `/dev/kvm` is absent and the CPU flags contain no `vmx/svm`, so **BoxLite cannot run inside a
> container**. It needs a real Linux host with KVM accessible.

### (3) Resuming a session needs an explicit switch

`sb.Session(ctx, "my-id")` fails for an id that does not exist yet:

```
session "my-id" no longer has a live environment: this handle has no session store,
so an unknown id cannot be verified as a resumption; use WithCreateIfMissing to start fresh
```

The first creation must say so:

```go
sess, err := sb.Session(ctx, "my-id", sandbox.WithCreateIfMissing(true))
```

To keep using the same id across processes or restarts, also configure
`sandbox.WithSessionStore(...)`; otherwise the id only means something inside this process.

### (4) The image must already be in the local store

`doctor` reports the missing image and how to get it:

```
FAIL image  want=present in the local store got=absent: golang:1.26
     fix: pull or build golang:1.26 in this engine, or point WithImage at an image it already has
```

Check the host before running any workload:

```bash
hno sandbox doctor --provider docker --image golang:1.26
# or in Go: sb.Diagnose(ctx)
```

---

## Scenario 1: write files, then run them

The minimal "agent emits code → it executes" loop. The key point: **writing and running are
separate contracts** — `Files()` writes, `Session.Exec()` runs, and a successful write proves
nothing about whether the payload compiles.

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

// the write channel
files, err := sess.Files()
if err != nil { log.Fatal(err) }          // *CapabilityError when there is no file bridge
if err := files.MkdirAll(ctx, "hello", 0o755); err != nil { log.Fatal(err) }
if err := files.Write(ctx, "hello/main.go", src, 0o644); err != nil { log.Fatal(err) }
if err := files.Write(ctx, "hello/go.mod", []byte("module hello\n\ngo 1.26\n"), 0o644); err != nil { log.Fatal(err) }

// the run channel: the working directory is /workspace, so relative paths land under it
rep, err := sess.Exec(ctx, sandbox.Shell("cd hello && GOFLAGS=-mod=mod go build -o /tmp/app . && /tmp/app"))
if err != nil { log.Fatal(err) }           // a non-nil error means the engine broke
fmt.Println(rep.ExitCode, rep.Stdout)      // a non-zero exit code means the code is wrong
```

Measured output:

```
exit=0 stdout="hello from the sandbox"
```

**Two traps, both actually hit:**

- **no `go.mod`, no module context**: `go: go.mod file not found in current directory or any
  parent directory`. When the agent writes only `main.go`, write the `go.mod` too.
- **`List` reporting `size=0`** is normal for directory entries and does not mean the file is
  empty — confirm content with `Read` or `Stat`.

### Three write channels — do not mix them up

**Channel A: `sess.Files()`** — best for "verify while generating", because a write is instantly
readable, listable, and visible to the next `Exec`.

**Channel B: inputs inline in the request** — best when the files belong to one run only, with no
file handle involved:

```go
rep, err := sess.Exec(ctx, sandbox.Request{
    Runtime: "shell",
    Code:    "cat gen/app.py && python3 gen/app.py",
    Inputs: []sandbox.Input{{Path: "gen/app.py", Bytes: []byte("print('inline input ran')\n")}},
})
```

Measured: `exit=0 stdout="print('inline input ran')\ninline input ran"` — the file was on disk
before the command started.

**Channel C: the whole workspace comes from the host** — when writing files one by one is not the
shape you want:

- `sandbox.FromDir("...")` — a host directory becomes the workspace source
- `sandbox.FromArchive(reader)` / `sandbox.FromGit(url, ref)` / `sandbox.FromImage(image)` / `sandbox.FromArtifacts(set, sub)`
- each returns a `*sandbox.WorkspaceSource`, used via `sandbox.WithWorkspace(...)` or
  `sandbox.Project.From`

To make a write survive a rebuild, register it with `sess.RecordInput(ctx, in)`. Unregistered
writes do not reappear after a rebuild — that is the difference between "it ran once" and
"it is reproducible".

---

## Scenario 2: verify as you generate

This is the real answer to "how does the agent write while it is generating": **run a cheap
check after each file** and treat the sandbox as a per-step feedback loop rather than a final
gate.

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
        // hand this one error back to the model, not the whole tree
        fmt.Printf("step %d failed: %s\n", i+1, rep.Stderr)
    }
}
```

Measured output (the run **without** a pipe, where the exit codes are honest):

```
step 1 wrote app/go.mod     vet exit=1
  model should fix: go: warning: "./..." matched no packages
step 2 wrote app/main.go    vet exit=1
  model should fix: # app            ← undefined: Greet follows this line
step 3 wrote app/greet.go   vet exit=0
final build exit=0 stdout=""
```

All three details are real, and all three matter:

- **Step 1 `exit=1`**: with only `go.mod` in the tree, `./...` matches no packages and `vet`
  treats that as failure. It is not a defect — there is simply nothing to check yet, and the
  agent should not "fix" the go.mod it just wrote.
- **Step 2 `exit=1`**: `undefined: Greet` is caught immediately. That is the whole point of
  asking after each file instead of once at the end.
- **`final build stdout=""`**: `println` writes to **stderr**, not stdout. Empty stdout does not
  mean nothing ran — read `rep.Stderr`, or use `fmt.Println`.

> ⚠️ **The trap this measurement replaced**: `go vet ./... | head -5` exits with **`head`'s**
> status, so an earlier run of exactly this loop reported `exit=0` for step 2 while
> `undefined: Greet` sat in stderr. **Never pipe a command whose exit code you are going to
> judge** — run it directly, or `set -o pipefail`. Otherwise your agent green-lights a project
> that does not compile.

---

## Scenario 3: let the result drive the next step

"After the run, how do I guide the agent to continue?" — two different mechanisms, do not conflate
them:

### (a) branch on the failure class (mechanical, decidable)

`Report.Failure` already classifies the outcome; do not parse strings:

```go
rep, err := sess.Exec(ctx, sandbox.Shell("make build"))
switch {
case err != nil:
    // the environment broke: rebuild or retry, never ask the model to edit code
case rep.ExitCode != 0 && rep.Failure != nil && rep.Failure.NextAction == sandbox.ActionModelFix:
    // the model's problem: feed rep.Stderr back and regenerate the failing file
case rep.ExitCode != 0:
    // timeout, lost run, quota: change strategy, do not change code
}
```

### (b) collect output while it runs, and decide mid-flight

Attach a sink with `WithRunSink`; chunks arrive as they are produced and `Report.OutputSent`
counts what was actually delivered live:

```go
sink := &mySink{} // implements Emit(ctx, sandbox.OutputChunk) error
rep, err := sess.Exec(ctx,
    sandbox.Shell("npm run build 2>&1"),
    sandbox.WithRunSink("collect", sink),
)
// rep.OutputSent > 0 is the proof of live delivery, not a final one-shot blob
```

Measured output:

```
exit=2 chunks=4 outputSent=34
sink saw failure marker live: true
class=user_code next action=model_fix
-> route stderr back to the model and regenerate the failing file
```

That is the automated form of "run → judge → next step": the sink holds live output for
immediate decisions, `Report.Failure.Class` and `.NextAction` give the machine-readable route, and
the model is only called back when the class says the code is wrong.

---

## Scenario 4: a whole project tree, ordered phases, artefacts

One file at a time fits `Exec`; **a whole project** belongs in `Project`: phases run in order,
share one workspace, and only declared artefacts come back.

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

Measured output:

```
status=succeeded exit=0 phases=3
  phase scaffold exit=0
  phase vet      exit=0
  phase run      exit=0
  artifact out.txt  2 bytes: "5\n"     ← genuinely the result of sum(2, 3)
```

Notes:

- a failed `Required: true` phase stops the run; `FailOpen: true` suits stages whose result *is*
  data, like a lint pass.
- `Artifacts` collects only what you declared; anything else disappears with the sandbox.
- `Project` has no inline `Inputs` field — **materialise the tree first** (a scaffold phase), or
  set `From: sandbox.FromDir(...)` to take the workspace from the host.

---

## Scenario 5 (optional): a runnable end-to-end demo

The four scenarios above are not just prose — they ship as a program:

```bash
go run ./cmd/examples/sandbox_agent_workflow -doctor          # host check, exit 1/70 on failure
go run ./cmd/examples/sandbox_agent_workflow -scenario all    # every scenario, real engine
go run ./cmd/examples/sandbox_agent_workflow -scenario streaming
```

It reads `HNO_SANDBOX_IMAGE` and `HNO_SANDBOX_BINARY` (set the latter to `container` on macOS
when `docker` is only a shim). `-doctor` prints every `Diagnose` check with its `fix` line and
exits non-zero if any of them fails, so you can tell "my code is wrong" from "this host cannot
run at all" before burning a build.

The same checks are available from the CLI, including the measured isolation tier:

```bash
hno sandbox doctor   --provider docker --image golang:1.26
hno sandbox boundary --provider docker --boxes 2   # measure the tier, do not infer it from a name
hno sandbox run      --provider docker --image golang:1.26 -shell 'go version'
```

Exit codes: `0` success / `1` checks ran but did not pass / `70` environment unavailable /
`2` usage error / `124` timeout.

---

## API surface this page uses

Every name below is exported today; the doc gates assert that (a page that invents an API
fails the build).

| Name | What it is |
|---|---|
| `sandbox.WithProvider` | chooses the registered provider by name |
| `sandbox.WithImage` | the image every box in this handle starts from |
| `sandbox.WithAttrs` | engine passthrough (`binary`, `registry`, ...) |
| `sandbox.WithMinimumIsolation` | the floor the handle must reach |
| `sandbox.WithCreateIfMissing` | lets `Session` provision an id it has never seen |
| `sandbox.WithSessionStore` | makes a session id survive this process |
| `sandbox.WithRunSink` | attaches a per-run sink for live output |
| `sandbox.FileService` | the write channel behind `sess.Files()` |
| `sandbox.ArtifactSpec` | one declared output to collect |
| `sandbox.DiagReport` | the `Diagnose` result: fields, no methods |
| `sandbox.ActionModelFix` | `Report.Failure.NextAction` when the model should rewrite |
| `sandbox.ClassUserCode` | `Report.Failure.Class` when the payload exited non-zero |
| `docker.Register` / `boxlite.Register` | explicit provider registration (D-03) |

---

## One-page cheat sheet

| What I want | API | The prerequisite people forget |
|---|---|---|
| write files into the sandbox | `sess.Files()` → `Write/MkdirAll` | provider needs `CapFileBridge`, else `*CapabilityError` |
| run one command | `sess.Exec(ctx, sandbox.Shell(...))` | cwd is `/workspace`; a non-zero exit is not an error |
| stream output while it runs | `sandbox.WithRunSink(name, sink)` | use `Report.OutputSent` to prove live delivery |
| run a whole project | `sb.RunProject(ctx, proj)` | no inline Inputs — scaffold first, or use `FromDir` |
| decide the next step | `Report.Failure.Class` / `.NextAction` | do not parse stderr text |
| first session creation | `sandbox.WithCreateIfMissing(true)` | otherwise an unknown id is refused |
| check the host | `sb.Diagnose(ctx)` / `hno sandbox doctor` | a missing image FAILs, with a pull hint |
| measure the real tier | `sb.MeasureKernelBoundary(ctx, 2)` | it starts 2 boxes; not a free call |

**Minimum runnable checklist**: ① import and register a provider; ② the engine binary exists on
the host and is addressed by its real name; ③ the image is already local; ④ the first `Session`
call passes `WithCreateIfMissing(true)`. All four, and the code above runs as written.
