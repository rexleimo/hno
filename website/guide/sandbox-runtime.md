# Sandbox runtime

The provider-neutral runtime under `pkg/hno/sandbox`: one handle, one policy, one error
vocabulary, shared by one-shot runs and long-lived sessions. This page describes what the
runtime actually reports — the numbers below are the resolved defaults, and a test in
`pkg/hno/sandbox/dx_gate_test.go` fails if the table drifts from the code.

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

Byte caps are per handle; only `WithOutputLimit` tightens a single run. Per-run CPU and memory
ceilings do not exist in this API — they are set on the handle with `WithLimits`, which is what
the compatibility adapter refuses to fake.

## Before you debug a run, ask the handle

```go
diag, err := sb.Diagnose(ctx)
fmt.Println(diag.Summary())
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
capabilities the provider claims but the core cannot act on, whether `ReadyProbe` and `Expose`
will be honoured here, live session counts, outstanding approvals, and leaked environments.

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
report, err := sb.RunSession(ctx, "build-42", sandbox.Shell("deploy"))
var pending *sandbox.ApprovalPendingError
if errors.As(err, &pending) {
    fmt.Println(pending.ApprovalID, pending.Reason, pending.ExpiresAt)
    fmt.Println(pending.Remediation())
}

sess, _ := sb.Session(ctx, "build-42")
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
if !diag.OK() {
    for _, f := range diag.Fixes {
        fmt.Println(f)
    }
}
```
