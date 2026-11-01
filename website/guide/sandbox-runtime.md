# Sandbox runtime

The provider-neutral runtime under `pkg/hno/sandbox`: one handle, one policy, one error
vocabulary, shared by one-shot runs and long-lived sessions. This page describes what the
runtime actually reports — the numbers below are the resolved defaults, and a test in
`pkg/hno/sandbox/dx_gate_test.go` fails if the table drifts from the code.

**Want the task-oriented version — write files, then run, then let the result drive the next
step?** Read [Agent: write, then run](/guide/agent-sandbox-quickstart) first; it states the four
prerequisites (provider registration, engine binary, local image, `WithCreateIfMissing`) and
shows four worked examples.

If you are using `code_execution` from `pkg/hno/tools/run`, see
[Sandbox code execution](/guide/code-execution-sandbox) for the legacy `Executor` surface, and
[From the legacy executor](#from-the-legacy-executor) below for the compatibility adapter.

## Thirty-second version

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

No engine on the host means `Open` or `Probe` fails with a typed error that names the engine it
tried, why, and what would change the answer. It never falls back to a host process.

## Defaults you are actually getting

`Limits`, from `sandbox.DefaultLimits()`:

| Field | Default | What happens past it |
| --- | --- | --- |
| `InlineFileMaxBytes` | 1 MiB | Larger inline file payloads are refused before the run |
| `OutputMaxBytes` | 8 MiB | Combined stdout+stderr cap per run; past it the overflow policy applies |
| `WorkspaceMaxBytes` | 2 GiB | Workspace staging refuses beyond this |
| `ArtifactMaxBytes` | 64 MiB | Per-artifact capture ceiling |
| `ArtifactTotalMaxBytes` | 256 MiB | Total artifact budget for one run |
| `EnvMaxEntries` | 128 | More entries is a refusal, not a truncation |
| `CPUCount` | 0 | 0 means the engine decides; set it with `WithLimits` |
| `MemoryMB` | 0 | 0 means the engine decides |
| `PidsLimit` | 0 | 0 means the engine decides |
| `DiskMB` | 0 | 0 means the engine decides |
| `SessionMaxConcurrentRuns` | 1 | Sessions are serial by default |
| `SnapshotKeep` | 4 | Older snapshots are pruned past this |
| `SnapshotTTL` | 1h0m0s | Snapshots older than this are reclaimed |
| `DefaultMountMode` | copy-snapshot | Snapshots are the default, not a live host bind mount |
| `Overflow` | drop | Output past the cap drops; `OverflowFault` stops the run with a typed error |

`Timeouts`, from `sandbox.DefaultTimeouts()`:

| Field | Default | Scope |
| --- | --- | --- |
| `Probe` | 5s | Engine reachability probe |
| `Connect` | 3s | Dial budget only; it does not cover creating anything |
| `Provision` | 2m0s | Control-plane step: create plus start |
| `Exec` | 30s | One request's execution |
| `Setup` | 30s | Install/setup steps |
| `Collect` | 30s | Artifact capture |
| `Drain` | 200ms | Final output drain after the process exits |
| `Idle` | 15m0s | A session idle this long is reclaimable |
| `SessionTTL` | 30m0s | Hard session lifetime |
| `Cleanup` | 10s | Engine cleanup step |
| `ApprovalHold` | 5m0s | How long a suspended approval stays answerable |
| `BoundaryFresh` | 30m0s | How long a kernel-boundary measurement may be used to admit a provider below the floor; 0 means the default, negative means measure per Open |

Byte caps are per handle; only `WithOutputLimit` tightens a single run. Per-run CPU and memory
ceilings do not exist in this API — they are set on the handle with `WithLimits`, which is what
the compatibility adapter refuses to fake.

## Before you debug a run, ask the handle

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

`Diagnose` reports the provider that answered, the isolation floor versus the isolation you got,
capabilities the provider claims but the core cannot act on, whether the image the handle is bound
to can actually start here, whether `ReadyProbe` and `Expose` will be honoured here, live session
counts, outstanding approvals, and leaked environments.

The image check is separate from the probe on purpose. The probe asks whether the engine can
create anything at all and deliberately never pulls; an engine that answers that happily can still
be holding no copy of your image. Providers that can answer implement `sandbox.ImageQueryable`, and
the three possible answers map to three different report lines — present, absent, and *"the engine
would not say"* — of which only the first is a pass. An unanswerable image question is reported as
a failure rather than rounded up to healthy, because a green light that means "nobody checked" is
the reason this check exists.

Every remediation string is checked by a test against the package's exported surface, so a hint
cannot tell you to call an option that does not exist or to run a CLI that was never shipped.

## Leaked containers after a crash

Engine-held environments this handle no longer owns are observable and reclaimable separately
from the health report:

```go
if r, ok := sb.Reclaimer(); ok {
    leaked, _ := r.Sweep(ctx) // look first: this deletes nothing
    removed, _ := r.Prune(ctx) // reclaim what it reported
    _ = leaked
    _ = removed
}
```

`Sweep` asks the engine rather than process-local state, which is the only way to see what a
crash left behind. `Prune` refuses to delete containers labelled as owned by another live
process, and says so: `SkippedPrune()` on the docker provider returns what it declined and why.

## Approvals a human has to answer

A policy that returns `ActRequireApproval` suspends the work inside the same environment:

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

An approval is permission for the environment the human was shown. If the box was lost or
rebuilt underneath it, `Resume` refuses with `*ApprovalEnvironmentChangedError` and does not
consume the grant. Approvals expire on the shorter of `Approval.Expires` and
`Timeouts.ApprovalHold`, and `Diagnose` reports any that are holding work with nobody to answer.

You do not need an `Observer` to discover a suspended run: `Session.Awaiting()` works on its own.

## Running approvals inside a graph

`pkg/hno/sandbox/graph` turns a sandbox request into a graph node that uses the graph's own
interrupt and checkpoint surface — no second waiting mechanism:

```go
node := sgraph.Node("deploy", sess, sandbox.Shell("deploy"))
result, err := g.Run(ctx, input)
```

When the node suspends, the interrupt payload is an `sgraph.Ask` carrying the session, the
approval id, and the question. Answering it later, from another process, is:

```go
out, err := ask.Answer(ctx, sb, true, "rex")
```

A refusal is a branch, not a fault: `Result.Refused` with `Result.Reason`, unless
`WithRefusalFails()` asks for an error. A non-zero exit from the payload stays data under
`Result.Run`.

## Projects, files, and outputs

`RunProject` executes phases against one workspace snapshot and collects declared artifacts:

```go
res, err := sb.RunProject(ctx, sandbox.Project{
    Phases: []sandbox.Phase{
        {Name: "build", Run: sandbox.Shell("go build ./..."), Required: true},
        {Name: "test", Run: sandbox.Shell("go test ./..."), Required: true},
    },
    Artifacts: []sandbox.ArtifactSpec{{Path: "bin/cli", Required: true}},
})
```

Declared artifacts that are missing fail the project; undeclared files are dropped. `Files` is
the inline channel (see `InlineFileMaxBytes`), `Workspace` stages a directory tree in, and
`Outputs` copies declared paths back out. Symlink escapes are refused during staging.

## From the legacy executor

`NewSandboxExecutor` implements the frozen `Executor` signature on top of the runtime, so an
existing `code_execution` toolkit can move without changing its callers:

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

The adapter refuses rather than silently reinterpreting, and the refusals are typed:

- `Spec.Workspace` was a read-write host bind mount; the runtime stages a snapshot in and copies
  declared outputs out. Migrate to `RunProject` or `WithWorkspace` instead.
- `Spec.MemoryLimit` / `Spec.PidLimit` per run cannot be honoured — ceilings are set on the
  handle. A value differing from the handle's is `ErrSandboxLimitMismatch`.
- `podman` and `e2b` have no provider registered in this build. They are refused, not quietly
  swapped for `docker`.
- A missing engine still surfaces the legacy `ErrNoProvider` sentinel, and a timeout arrives as
  both a wrapped `ErrTimeout` and `Result.TimedOut`.

## Failure visibility in the toolkit

`run_code` distinguishes the two failure kinds that used to look identical:

- The payload ran and exited non-zero → a result map, no error. The model reads it as data about
  its own code.
- The environment could not run the payload → the map is still returned, **and** a real error is
  returned, so the agent's logger, the per-call outcome, and any monitoring see a failure rather
  than a success with sad text.

A timeout is payload data (`timed_out` is already in the map); the classification uses only what
this layer can prove, so a working script is never reported as an outage.

## From a shell: `hno sandbox`

`cmd/hno` exists so that what the runtime already knows can be read without writing a Go
program. It adds no policy of its own — each verb is a thin shell over one exported call, and
every flag is named after the option it sets.

```bash
go build -o hno ./cmd/hno

./hno sandbox doctor                      # why can't this run here?
./hno sandbox doctor --json               # same verdict, machine-readable
./hno sandbox prune                       # list what this host left behind (read-only)
./hno sandbox prune --yes                 # delete them
./hno sandbox run -- go version           # one command, payload status passed through
./hno sandbox run --timeout 30s --shell "deploy"
./hno sandbox run --attr binary=container --image golang:1.26 -- sh -c "exit 5"
./hno sandbox boundary                      # is the isolation a measurement or a name? (starts boxes)
./hno sandbox boundary --boxes 3 --expect private-kernel --json
```

| Verb | Runtime call | Exit status |
| --- | --- | --- |
| `doctor` | `Sandbox.Diagnose` | `0` all checks passed, `1` a check failed |
| `prune` | `Sandbox.Reclaimer` | `0`; refuses to delete without `--yes` |
| `run` | `Sandbox.Run` / `Session.Exec` | the program's own code, `124` timed out, `70` environment unusable, `2` bad command line |
| `boundary` | `Sandbox.MeasureKernelBoundary` | `0` measured and matches `--expect`, `1` verdict is `unknown` or differs, `70` could not measure, `2` bad flag |

The three `run` outcomes are deliberately not merged. A program that exited 5 is not a
sandbox that failed, and a sandbox that could not start has no opinion about the program:
`70` means **the status of your command is unknown**, so a wrapping script must not read it
as "it ran and failed". `124` follows `timeout(1)`. Same split as
[Failure visibility in the toolkit](#failure-visibility-in-the-toolkit).

`--attr k=v` is the engine-config channel (`sandbox.WithAttrs`), which is how you point the
CLI at a different container binary (`--attr binary=container`) or an engine runtime such as
`runsc`. `--isolation` takes exactly the names `Isolation.String` renders — `none`,
`container`, `syscall-filtered`, `microvm`, `microvm-hosted` — and a value the parser refuses
stops the command instead of quietly running below the floor you asked for.

Prune is read-only by default because a reclaim tool that guesses is worse than one that asks,
and what the engine declined to delete is printed rather than hidden inside a zero count.

## Isolation is measured, not named

The ladder above says what a provider **claims**. One call says what the engine
actually does, and the difference matters: on the host this runtime was built on,
`docker` is not Docker Desktop but a compatibility shim over the Apple
containerisation engine (`docker info` reports `Apple Containerization`), and every box
it starts gets its own kernel. Four boxes produced four different
`/proc/sys/kernel/random/boot_id` values and four different `/proc/uptime` readings.
Assigning a tier from the provider's name would have called that engine container
isolation while it was handing out VMs — and the same binary on a Linux host with a
shared-kernel daemon would then have been telling the truth, which is why no name-based
mapping can be correct in general.

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

Four verdicts, and they are not interchangeable: `private-kernel`, `shared-kernel`,
`unknown`, and `unmeasured`. Only the first supports a micro-VM floor; `unknown` means
the engine gave readings that cannot support a conclusion, and it exits nonzero from the
CLI rather than looking healthy. The readings travel with the verdict — one guest boot id
per box, each box's seconds-since-kernel-boot, and the guest kernel against the host's —
because a conclusion you cannot check is a claim.

Two cost rules, learned from the mistakes this package already made:

- Nothing is provisioned to discover the question cannot be answered. A provider without
  `sandbox.KernelBoundaryQueryable` gets a typed error naming the interface (same
  house rule as D-36's image question), and an invalid box count is refused before any
  boot.
- The measurement boots real boxes, so it is a deliberate call: `sandbox.MaxBoundaryBoxes`
  caps one measurement at `sandbox.DefaultBoundaryBoxes` or a few more. It is
  **never** part of `Diagnose`. Once you have measured, though, `Diagnose` reports the
  cached verdict against the floor this handle was given, so an over-claim surfaces in
  the health report instead of staying a secret of the person who ran the measurement.

The core also refuses a verdict its own evidence does not support: a provider that
reports `private-kernel` while handing back two identical boot ids is corrected to
`shared-kernel` with the reason in `Fixes`. The readings decide, not the assertion.

This batch reports the measurement; the micro-VM **floor** is still admitted on the
provider's claim. Making admission use the measurement is a deliberate widening of a
gate, so it is a decision rather than a side effect — see `hno sandbox boundary` below.

### Letting the measurement decide admission

The section above reports. This one lets the report change what is allowed, and it is the
only place in the runtime where evidence can **widen** a gate rather than close one — so it
is opt-in twice: the measurement must exist, and it must be handed in.

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

Two directions, only one of them new:

- A `private-kernel` measurement admits a provider whose claim is **below** the floor. On
  the host this page describes that is the difference between `IsolationMicroVM` being
  refused and being honoured: the engine puts every box in its own VM but reports container
  isolation, so the claim alone can never satisfy it.
- A `shared-kernel` measurement **refuses** a provider that claims the VM tier. The claim
  alone would have passed; the measurement is the only thing that can stop it. That
  direction is a tightening and is always honoured.

`Status` keeps reporting the provider's own claim in `Isolation` and carries the evidence in
`Boundary`, because rewriting the claim would tell every downstream reader that an engine
changed its nature. `Diagnose` agrees with whatever admission did and says so in the
isolation line rather than contradicting the handle it describes.

Evidence is spent, not swallowed. A report is refused as a reason to lift when it came from
a different provider, was taken on a different host, or is older than
`Timeouts.BoundaryFresh` — and the refusal says which of the three, because the alternative
is an operator re-measuring an engine that already measured fine. An `unknown` verdict moves
nothing in either direction.

From a shell the flag is `--measure-boundary`, and it is not free: it starts boxes before the
run does.

```bash
hno sandbox run --isolation microvm --measure-boundary -- go version
```

## The micro-VM provider

`boxlite` is registered in this build as a second engine, and it is the one that reaches the
VM tier on a laptop. Two facts about it are worth knowing before you point a profile at it,
because both were measured rather than read:

- The **Go SDK exists from v0.11.0 only** (`go get .../sdks/go` resolves for no earlier
  version), and it is not what this provider uses. It is cgo-only — with `CGO_ENABLED=0`,
  which is how this project's CI builds, it does not compile at all — and it needs a separate
  `go run .../cmd/setup` step that downloads a ~70MB prebuilt native archive. On the machine
  this was measured on, that download failed twice, arrived truncated, and shipped an empty
  `include/` directory, so cgo then reported `boxlite.h not found`. The provider therefore
  wraps the CLI, exactly like the container provider: no cgo, no Rust toolchain, no new
  module dependency, and a build that needs no network.
- The CLI has **no label concept**. So a box found on a host cannot be attributed to this
  process, and `Prune` refuses instead of guessing — `SkippedPrune()` returns the reason,
  which is printed rather than hidden behind a zero count. The container provider can delete
  because every box it creates carries an ownership label; this one cannot, and says so.

```go
sb, err := sandbox.Open(ctx,
    sandbox.WithProvider("boxlite"),
    sandbox.WithImage("alpine:latest"),
    sandbox.WithAttrs(map[string]any{
        "bin":      "/home/me/.local/bin/boxlite",
        "registry": "mirror.example",
    }),
)
```

The acceptance target is Linux. `Probe` does not branch on the operating system — it asks the
engine, whose `info` verb reports `virtualization: available` or not, and refuses with a typed
`UnavailableError` naming what is missing. Unknown is not a pass: an engine that does not
answer the question cannot be assumed to virtualise, and a box per kernel is this provider's
entire claim.

Two operational details the engine imposes:

- `--registry` is a pull-time flag that does not persist into the box record, so the provider
  qualifies the image reference itself. Without that, `start` pulls from the default registry
  and fails on a network where only the mirror answers.
- the engine holds an **exclusive lock on `BOXLITE_HOME`**, one runtime per directory. The
  provider serialises its own CLI calls for that reason; a second process pointed at the same
  home gets a lock error rather than a queue.

Capabilities claimed are the ones the CLI surface backs: `session`, `streaming`,
`port-forward`, `egress-allowlist`. Snapshot, clone, rebuild and the file bridge are absent
because the engine documents no verb for them or the bridge has not been written, and a claim
the core cannot honour is refused at bind time rather than discovered on the four hundredth
run.

## Recipes

**Pin an image and refuse anything looser than a container:**

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

**One long-lived box, many commands, resume across a restart:**

```go
sess, _ := sb.Session(ctx, "notebook", sandbox.WithCreateIfMissing(true))
_, _ = sess.Exec(ctx, sandbox.Shell("pip install pandas"))
_, _ = sess.Exec(ctx, sandbox.Shell("python analyze.py"))
_ = sess.Release(ctx)
```

Re-acquiring a session whose environment is gone is a rebuild from the recorded spec when the
provider declares `CapRebuild`; a step is skipped only when the rebuilt box actually carries
that state.

**Ask before anything destructive, and let a human answer out of band:**

```go
sb, _ := sandbox.Open(ctx,
    sandbox.WithProvider("docker"),
    sandbox.WithImage("postgres:16"),
    sandbox.WithPolicy(policy),
    sandbox.WithTimeouts(sandbox.Timeouts{ApprovalHold: 30 * time.Minute}),
)
```

`Diagnose` names any approval that is holding work; `Session.Awaiting()` lists them;
`Approve`/`Deny`/`Resume` finish the exchange in the same environment.

**Check what the runtime will say before you write it in a runbook:**

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
