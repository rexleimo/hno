# Sandbox: LiteBox Evaluation and Provider Upgrade Plan

**Status:** proposed (decision brief + work-item plan)
**Scope:** `pkg/hno/tools/run`, `pkg/hno/tools/file`, `deploy/sandbox`, `benchmarks/`
**Target release:** v3.1.0 (additive) — v4.0.0 only if the frozen `Executor` contract must break

## 1. Question asked

> We have a sandbox-based runtime that does not feel good. Microsoft has a sandbox tool
> called LiteBox. Should we embed it into our system, or integrate it? Use the answer to
> scope this version upgrade.

The answer depends on a fact that changes the whole decision, so it is resolved first.

## 2. Disambiguation: two projects, nearly identical names

| Project | Owner | What it is | License | Signals |
|---|---|---|---|---|
| **LiteBox** — `github.com/microsoft/litebox` | Microsoft | A **security-focused library OS** in `no_std` Rust. North = a `nix`/`rustix`-like POSIX interface *and* a Linux-ABI shim; South = pluggable `Platform` providers (Linux userland, Linux kernel, Windows userland, LVBS, SEV-SNP, OP-TEE) | MIT | 3.2k stars, created 2024-12, **one tag: `v0.1.0`**, README warns APIs will keep changing, 154 open issues |
| **BoxLite** — `github.com/boxlite-ai/boxlite` | `boxlite-ai` org (not Microsoft) | **Micro-VM sandbox for AI agents**: daemonless embeddable library + CLI + REST server, runs any OCI image inside its own kernel, boxes persist across turns | Apache-2.0 | 2.6k stars, created 2025-12, Python/Node/**Go**/Rust/C SDKs, adopted as a sandbox backend by Databricks Omnigent, Alibaba AgentScope Runtime, ByteDance deer-flow |

The user request said "Microsoft's LiteBox", which is the **library OS**. Both are evaluated here
because the second one is what our gap actually calls for.

## 3. Evidence about microsoft/LiteBox

Inspected at `main` (shallow clone, commit dated 2026-10-30):

- **It is a framework, not a runtime.** No binary you can point an agent at. You compose a
  *runner* = one shim × one platform (`litebox_runner_linux_userland`,
  `litebox_runner_linux_on_windows_userland`, `litebox_runner_snp`, `litebox_runner_optee_...`).
- **The easy Linux path does not isolate memory.**
  `litebox_platform_linux_userland/src/lib.rs`: *"The guest shares the host process's address
  space, so this platform does not isolate host memory from the guest: guest code or
  fixed-address mappings can read or overwrite it."* It is also hard-gated
  `#![cfg(all(target_os = "linux", target_arch = "x86_64"))]` — no arm64 Linux, no macOS.
- **The hook that makes untrusted Linux ELF work is self-declared as not a boundary.**
  `litebox_syscall_rewriter/src/lib.rs`: *"This approach is not 100% foolproof, and should not be
  considered a security boundary... will **NOT** support dynamically generated `syscall`
  instructions (for example, generated in a JIT)... currently only supports x86-64 (i.e., amd64)
  ELFs."* CPython/Node extension modules, `pip` wheels, and any JIT are exactly that population.
- **Real isolation requires infrastructure we do not have.** `litebox_platform_linux_kernel`
  generates bindings with `bindgen` against an external *sandbox driver* (kernel component);
  `litebox_platform_lvbs/README.md` states: *"This crate is work in progress. Currently, it is a
  copy of `litebox_platform_linux_kernel`."* LVBS needs Hyper-V VTL1, SNP needs AMD SEV-SNP.
- **Userland networking needs a pre-created TUN device**
  (`litebox_platform_linux_userland/README.md` + `scripts/tun-setup.sh`), and the interesting
  runner flags (`--rewrite-syscalls`, `--initial-files`, `--tun-device-name`,
  `--program-from-tar`) are all behind `--unstable`.
- **No host-facing ABI.** No C ABI, no cbindgen/uniffi, no Go/Python bindings; the `extern "C"`
  sites are internal hypervisor/signal plumbing. Consuming it from Go means owning a Rust
  sidecar, our own ELF packaging pipeline (`litebox_packager`), and a control protocol.

**Conclusion on LiteBox:** embedding is technically impossible (Rust `no_std` library, no FFI
surface); integrating it as a provider means becoming a LiteBox maintainer for an API the authors
say is unstable, on x86-64 Linux only, whose two usable modes are either "no memory isolation"
(userland) or "needs a Microsoft kernel driver / Hyper-V VTL1" (kernel/LVBS). **Not this
version's answer.**

## 4. Our current sandbox system and why it "does not feel good"

Inventory: `pkg/hno/tools/run/{executor,podman,local,e2b,toolkit}.go` (execution),
`pkg/hno/tools/file/sandbox.go` + `filegen` (path confinement), `deploy/sandbox/agent-runner/`
(base image), `docs/design/code-execution-sandbox.md` (contract).

What is genuinely good and must be preserved: fail-closed provider probing (`ErrNoProvider`,
`NewExecutor` never falls back to host exec), payload on stdin (no argv leakage), fixed
non-caller-tunable policy flags (`--rm --network none --cap-drop ALL --no-new-privileges
--pids-limit --memory --memory-swap --read-only --tmpfs /tmp`), explicit `local` opt-in for dev,
bounded output buffers, and `file.Sandbox` built on `os.Root` with byte caps and `AuditEntry`.

The eight concrete weaknesses, with receipts:

1. **No state.** `Executor` is `Run(ctx, Spec) (Result, error)` + `Close()`
   (`executor.go:103-106`). One call = one disposable container. An agent cannot
   `pip install` in turn 1 and use it in turn 2 — the capability that every serious coding agent
   needs.
2. **E2B is charged and cold per call.** `e2bExecutor.Run` creates a sandbox, uploads, runs,
   and unconditionally deletes (`e2b.go:96-148`). Latency and money scale with tool-call count.
3. **Exec sandbox and file sandbox are two unrelated systems.** `e2b.go:101` returns
   *"e2b sandbox workspace mounts are not implemented"*, and `file.Sandbox` confines host paths
   in-process. So the model can write a file through one sandbox and cannot reliably read it back
   from the other.
4. **Network policy is a placeholder.** Only `NetworkNone` exists; `validateSpec` rejects any
   other value (`executor.go:219`) while the Spec comment promises future policies. Real agents
   need egress allowlists (`pip`, a package registry, one API host) — today it is all-or-nothing.
5. **Isolation ceiling is a shared host kernel.** Local path is podman/docker only.
   `docs/design/code-execution-sandbox.md` lists gVisor/microVM as *Future work* and the user guide
   tells users to go elsewhere for kernel-escape protection. Strong isolation today means paying
   E2B.
6. **Language support is a hardcoded map of three entries** (`runtimeTemplates`: python, node,
   shell) against one image; go/rust/custom templates from our own design doc were never built.
7. **No pooling, no budget, no admission control on the exec path.** Each `Run` forks a `podman`
   CLI process; nothing caps concurrent sandboxes per tenant/service, no warm pool, no metrics.
8. **Audit exists only for file ops.** `AuditEntry` is in `pkg/hno/tools/file`; the design doc
   promises an `AuditEntry` per `Run` and the code does not produce one.

Root cause is architectural, not provider choice: **we named our seam `Executor` (a single-shot
`Run`), so every provider we add is forced into statelessness.** The upgrade should widen the
seam once, then attach providers to it.

## 5. Decision

**Neither "embed LiteBox" nor "swap in a vendor".** Do this:

1. Keep ownership of the seam. Replace the single-shot notion with a **provider capability
   model** in our own package: `Executor` stays frozen; add opt-in interfaces (`SessionSandbox`,
   `FileTransfer`, `StreamingExec`, `EgressPolicy`, `CapabilitySet`, `RunAudit`) that providers
   assert and the toolkit probes. This is additive → minor version.
2. Integrate **external isolation engines as interchangeable providers**, ranked by tier, with
   explicit probing and no silent degradation — the same discipline `NewExecutor` already uses.
3. For this release, the local strong-isolation tier is **BoxLite as a provider** (Apache-2.0,
   Go SDK, OCI-native, persistent boxes, `allow_net` egress allowlist, secret placeholders,
   copy-on-write disk, PTY exec) — *behind our interface*, never as our architecture. LiteBox
   stays on strategic watch (§8).
4. Cheapest first win that needs no new dependency: **gVisor (`runsc`) as an OCI runtime** for the
   existing container executor, which closes the doc's own admitted kernel-escape gap.

Why not "embed LiteBox": no ABI to embed, x86-64 Linux only, the usable modes either share host
memory or need a Microsoft kernel driver/Hyper-V VTL1, the syscall-rewrite boundary is explicitly
not a security boundary, and v0.1.0 API churn would be absorbed by us every upgrade.
Why not "adopt BoxLite wholesale": it would leak a vendor's box model into `pkg/hno`, break our
fail-closed contract, and make E2B/our file sandbox dead code. It enters as one provider among
several, through our interface.

## 6. Target architecture

```
                     run.CodeExecutionToolkit (agent-facing)
                                  │
                        SandboxService (policy, quota, audit, metrics)
                                  │
                     Executor (frozen) ── Run/Close, stateless calls
                                  │
              opt-in capability interfaces (asserted, not assumed)
   ┌──────────────┬──────────────┬───────────────┬──────────────┬─────────────┐
 SessionSandbox  FileTransfer   StreamingExec   EgressPolicy   RunAudit      │
   │                                                                          
 podman/docker  runsc (gVisor)  boxlite (microVM, local/self-host)  e2b (hosted)  local (dev, explicit)
```

Isolation ladder (config-selected, always fail-closed):

| Tier | Engine | Boundary | Use |
|---|---|---|---|
| L0 | `local` | none | dev only, keeps current explicit opt-in |
| L1 | `podman`/`docker` | userns + caps + cgroups | default today, unchanged |
| L2 | `docker/podman --runtime runsc` | syscall interception (gVisor) | kernel-escape hardening, no new code path |
| L3 | `boxlite` | microVM per box (KVM / Hypervisor.framework) + OCI image inside | persistent multi-turn agent sessions locally |
| L4 | `e2b` | hosted microVM | elastic/billable, no local virt dependency |

One `Spec` shape serves all tiers; `Workspace` semantics are identical everywhere (upload/mount or
explicit refusal), and every `Run` emits one audit record regardless of provider.

## 7. Work items for this version

Ordered; P0 gates everything after it. `progress_made` / evidence per step is recorded in the run
journal of whichever provider executes these.

**P0 — Spike and go/no-go gate (timeboxed, throwaway code).**
(a) gVisor: run the existing container executor against `runsc` with the current policy flags.
(b) BoxLite Go SDK: minimal provider behind a fake seam — `Create/Exec/Stop/Resume` on
`python:slim`, plus file copy and `allow_net`.
Measure vs. today: cold start, per-exec latency, 20-turn session total time, RSS, and whether
`/dev/kvm` exists in our real deployment (containers on managed k8s usually say no). Verify the
CGO/Rust build story against `golang:1.24.11-alpine` + `alpine:3.22` (`Dockerfile`) — expect to
move the runner to a glibc image or ship a sidecar.
*Exit:* one-page receipt in this file's §9; if KVM is unavailable **and** CGO blocks the build,
drop L3 and ship L2 + state + network policy only. Stop rule: do not proceed on optimism.

**P1 — Seam hardening (no provider change, no behavior change).**
Add opt-in interfaces + `Capabilities`; add `Spec` fields (`SessionID`, `Egress allowlist`,
`Files`, `WorkingDir`, `RunAs`) defaulted so today's behavior is byte-identical; extend
`validateSpec` to reject unknown policies fail-closed instead of only `NetworkNone`; wire a
`RunAudit` record and provider-agnostic metrics (count, duration, exit class, output bytes,
timeout, kill) into the exec path.
*Accept:* table-driven tests with fakes for each capability; `go test ./...` green; existing
integration tests unchanged in outcome; `make vet`/`make lint` clean.

**P2 — Stateful sessions across agent turns.**
`SessionSandbox` implementation for the container tier (long-lived named container with hard TTL,
idle reclaim, `--rm` on release) and the L3/L4 tiers (native box persistence). Bind the sandbox id
to the session identity already in `pkg/hno/session` / Store so a resumed session reuses its box.
Kill-switch: max TTL, max concurrent boxes, per-tenant quota; leaked-box sweeper on process start.
*Accept:* integration test proves turn-1 `pip install` is visible in turn-2, and that a killed or
expired box never leaks a container (`podman ps` empty after release).

**P3 — One workspace, two tools.**
Make `file.Sandbox` and the exec sandbox share a single workspace root and audit stream; add
`FileTransfer` upload/download so E2B and BoxLite paths stop lying about `Workspace`; replace the
`e2b.go:101` "not implemented" with either real behavior or an explicit, tested refusal.
*Accept:* round-trip test (write via file tool → read via `run_code`, and reverse) on every tier
that claims support; refusals tested as errors, not silent divergence.

**P4 — Egress policy and runtimes.**
`NetworkAllowlist` (default deny, explicit hosts/CIDRs, DNS-only mode) implemented per tier
(podman container-network/netavark or a small egress proxy; BoxLite `allow_net`; E2B flag), plus
runtime templates for go/rust/custom and per-runtime images, keeping templates *outside* the core.
*Accept:* tests prove `curl` to a non-allowlisted host fails while an allowlisted one succeeds;
`--network none` remains the default and unknown policies still fail closed.

**P5 — Docs, benchmark gate, release notes.**
Rewrite `docs/design/code-execution-sandbox.md` status, update
`website/{,zh/}guide/code-execution-sandbox.md` + `sandboxed-file-io.md`, and add a sandbox suite
under `benchmarks/` next to the existing runtime benchmark so latency claims stay honest.
Conventional Commits, `Unreleased` changelog entry, v3.1.0 if `Executor` stayed frozen.

## 8. LiteBox: strategic watch, with concrete revisit triggers

LiteBox is interesting for a niche we do not yet serve: running **one** untrusted, ahead-of-time
compiled binary with a *minimal syscall surface* and no VM — sub-container startup cost, in-process.
Re-open this evaluation when **all** of these hold:

1. a stable release past `v0.1.0` with a documented, frozen API;
2. a host-facing ABI (C ABI or FFI crate) so a Go host can drive it without owning a fork;
3. a userland platform that does **not** share the host address space, or a platform we can
   actually deploy (no bespoke kernel sandbox driver, no Hyper-V VTL1);
4. arm64 Linux (and ideally macOS) support — our dev laptops are Apple Silicon;
5. a declarable file/network policy model that maps onto our `Spec` instead of raw punchthrough.

Then it becomes tier "L2.5: library-OS syscall reducer" behind the same interface, and this plan
needs no restructuring — that is the point of widening the seam rather than swapping engines.

> **状态更新（v1.0）**：本节 6 条已全部关闭，映射到台账 ID：命名→D-01、介入点→D-02、
> 装配形态→D-03（remote sidecar）、并发→D-06（serial）、无 KVM→D-07（FailFast）、
> MITM/TTL/Executor→D-08/D-09/D-10。以下为历史原文，不再作为待决依据。

## 9. Open decisions — CLOSED in v1.0 (see architecture doc §12.1 ledger D-01…D-19)

1. **Name check:** did you mean `microsoft/litebox` (library OS, analyzed above) or
   `boxlite-ai/boxlite` (micro-VM agent sandbox)? The recommendation flips if you meant the second.
2. **Deployment substrate:** is `/dev/kvm` available in production (bare metal / GCP nested
   virtualization / k8s with device plugin)? No KVM ⇒ no local microVM tier, and gVisor + E2B
   carry the release.
3. **Build policy:** are we willing to carry CGO + a Rust-built runner image (BoxLite needs
   `go get github.com/boxlite-ai/boxlite/sdks/go`, Go 1.24+ with CGO), or must the sandbox engine
   stay a sidecar binary/service like `podman`/E2B today?
4. **Compatibility posture:** is `Executor` frozen for the 3.x line (recommended, → v3.1.0), or do
   we allow a breaking redesign (→ v4.0.0)?
5. **Spending:** keep hosted E2B as the L4 escape hatch, or aim to be self-hostable only?

## 10. Evidence references

- Repo: `pkg/hno/tools/run/executor.go` (Executor, Spec, validateSpec, probe order),
  `podman.go` (policy flags), `local.go` (dev-only, unsandboxed), `e2b.go:96-148`
  (per-run create/delete) and `e2b.go:101` (workspace mounts unimplemented),
  `toolkit.go` (fail-closed nil executor, 5-minute model-timeout cap),
  `pkg/hno/tools/file/sandbox.go` (`os.Root` confinement, `AuditEntry`),
  `deploy/sandbox/agent-runner/Dockerfile`, `docs/design/code-execution-sandbox.md`
  (Security model, Testing strategy, Future work), `Dockerfile` (alpine build/runtime images),
  `CHANGELOG.md` (`[3.0.0] - 2026-09-29`, `[Unreleased]`).
- microsoft/LiteBox @ `main`: `README.md`, `Cargo.toml` workspace members,
  `litebox/src/lib.rs`, `litebox_shim_linux/src/lib.rs`,
  `litebox_platform_linux_userland/src/lib.rs` (+ `README.md`), `litebox_syscall_rewriter/src/lib.rs`,
  `litebox_platform_lvbs/README.md`, `litebox_platform_linux_kernel/build.rs`,
  `litebox_runner_linux_userland/src/lib.rs` (unstable flags), tags API (`v0.1.0`).
- boxlite-ai/BoxLite @ `main`: `README.md` (Go/Python/Node/Rust/C SDKs, features table,
  architecture), `docs/getting-started/README.md` (KVM / Apple Silicon requirements).

## 11. v0.3 增补：复杂场景与流程嵌入（评审追加）

评审中确认的三个缺口，已写入 `sandbox-runtime-architecture.html` §6.4–6.5、§13–§15：

1. **单文件 API 撑不起真实项目**。v0.2 的 `Request.Files map[string][]byte` 只能把小文件读进内存，
   既装不下项目目录，也拿不出执行产物。改为三件套：`Inputs []Input`（来源式、流式）、
   `Workspace *WorkspaceSpec`（`FromDir/FromArchive/FromGit/FromArtifacts/FromImage` 五种来源 ×
   `CopySnapshot|MountShare|Hybrid` 三种挂载模式）、`Outputs []OutputSpec`（声明式产物，带 `Required/MaxBytes`）。
   默认必须 `CopySnapshot`：让 agent 直接写宿主目录不是便利，是事故。
2. **没有打通"嵌入别人的流程"**。新增 `Project/Phase/ArtifactSpec/ArtifactSet`（一次调用内部按 Phase
   顺序共享同一工作区，`Required/FailOpen` 决定失败是否继续，`res.Resume()` 从失败 Phase 续跑而不重跑 setup），
   以及 `sandboxgraph.NewNode().In()/Out()/OnFail()` —— 用**已有的** graph 节点策略
   （`pkg/hno/graph/policy.go:78`）、HITL（`hitl.go:47` `RequestInterrupt`/`*Suspension`/`Graph.Resume`）、
   持久化（`durability.go:43` `Checkpointer`）与 `store.Store` 完成嵌入，不新造沙盒编排语言。
3. **异常场景未覆盖**。§15 列出 15 条并给出契约：后台长驻服务 + `ReadyProbe`、端口转发、
   REPL/PTY 内核、`RunRef` 脱离重连、宿主重启后的 lease 清扫、VM 半路死亡（`*LostError`）、
   同工作区并发（`LockMode`）、`CloneAs` CoW 分叉（多 agent 试方案）、`IdempotencyKey` + `EffectClass`
   防止自动重试重复副作用、输出/产物爆炸截断、执行中途出网被拒、宿主水位预检、版本漂移拒绝启动、
   跨引擎状态迁移**明确不支持**。

分期新增 **P6（项目级工作区）** 与 **P7（流程嵌入 + 异常语义）**；P7 的恢复/并发/幂等语义
**必须与 P2（Session 池）同批交付**，不能先上池子再补事故处理。
新增待决：默认挂载模式（默认 `CopySnapshot`）、产物超限额是截断交付还是失败（默认按 `Required` 区分）。

## 12. v0.4 增补：失败分类与重试预算（评审追加）

评审指出 v0.3 只写了 `IdempotencyKey` / `EffectClass` 两个名词，没有重试策略。契约已写入
`sandbox-runtime-architecture.html` §16，要点：

1. **七类失败** `ClassUserCode | Denied | Permanent | Transient | Resource | Timeout | Lost`，
   逐类定义「可否自动重试、由谁重试、默认处置」。核心判定：`ClassUserCode` **绝不自动重跑**（同 payload
   必然同结果），而是 `NextAction=model_fix` 把 stderr 摘要回环给模型；`ClassTimeout` 语义未知
   （可能已执行一半副作用），只有 `EffectPure` 才允许自动重试。
2. **不新造退避引擎**：分类结果直接喂框架已有的 `graph.RetryConfig.ShouldRetry func(error) bool`
   （`pkg/hno/graph/policy.go:22-29`，full-jitter 已落地）。重试职责分四层（Core / graph 节点策略 /
   agent 模型回环 / 人工），并明确 **Core 预算不得与节点预算相乘**（防 2×3=6 次放大）。
3. **`TimeoutConfig.PerAttempt` 对沙盒是必须的**：BoxLite 一次 boot 1–2s，若按整节点算超时，
   第二次 attempt 必然假超时；Profile 需拆 `SetupTimeout` / `ExecTimeout`。
4. **副作用闸门**：`EffectPure` 任意重试；`EffectWorkspace` 必须先回滚或重建；`EffectExternal`
   （写回宿主、git push、发 HTTP）**永不自动重试**，只允许 `AtMostOnce` + 幂等 journal。
   journal 落 `store.Store` / `Checkpointer`，`payloadHash` 不符 ⇒ `*IdempotencyConflictError`；
   提交后断连且引擎无 `CapQueryStatus` ⇒ `*UncertainOutcomeError`，禁止自动重试。
5. **状态感知的重试**（复杂场景真正的答案）：`Session.Checkpoint()`（QCOW2 CoW，不复制盘）+
   `Restore()` ⇒ `RetryMode ∈ {Blind | RebuildFromSpec | RollbackToPhase | ForwardsOnly}`。
   默认 `RebuildFromSpec`；有状态 + 有副作用的重试只有能退回已知点才成立。
6. **熔断与防风暴**：provider 级 `observability.CircuitBreaker`（open 时直接失败且**不烧 attempt**，
   沿用 `runner/resilience.go:29` + `observability.ErrOpen` 语义，只有 `Transient|Lost` 计入）；
   同幂等键并发只允许 1 个在途；同 Session 滑动窗口 + 全局 ceiling ⇒ `*RetryBudgetError`。
7. **失败即修复信息**：新增 `Failure{Class, Retryable, NextAction, Hint, Fixes, BlockedHosts, Attempt, Budget}`。
   toolkit 层改为双写（`error` 非 nil + map 内放 failure 摘要），**替换今天 `toolkit.go:106-115`
   把错误塞进 map 返回 `nil` 的做法** —— 否则模型分不清「沙盒坏了」与「代码写错了」，只会无限重投。
8. 另含 Kill 阶梯（SIGTERM→grace→SIGKILL→整组校验，存活则停止接新 Exec）、Kill 后有界
   drain（抢救 stderr 尾段供模型修正）、`Probe TTL=60s`（装机类失败不在每次 tool call 全链重探）。

新增待决 #9–#11：默认 `RetryMode`、生产是否允许 tier 降级、Phase 边界是否自动打快照。

## 13. 评审决议 #9–#11（v0.5）

- **#9 默认 `RetryMode = RebuildFromSpec`**。`EffectPure` 才降到 `Blind`；`RollbackToPhase` 因 #11
  默认关快照而不可作默认，只能显式启用。
- **#10 默认 `FailFast`**。tier 降级需 Profile 显式 `FallbackTier` + 强制 `EventIsolationChanged`；
  开了降级却没注册 Observer ⇒ `Diagnose()` 报配置错误（防止静默降级）。
- **#11 Phase 边界自动快照默认关**，`SessionOption{AutoCheckpoint:true}` 显式开。

两条连带义务（写入 §16.9）：(a) 默认 Rebuild 会丢临时安装物 ⇒ **声明式输入是硬前提**，
`Session.Install()` 之类动作必须记入 spec 可重放，不能只改盘；(b) 手工快照需定义保留策略
`snapshots{keep, ttl, on_session_destroy:purge}` + `Diagnose` 报孤儿快照，避免宿主磁盘泄漏。

## 14. 决议 #9 落地：Replayable Spec（v0.6）

默认 `RetryMode = RebuildFromSpec` ⇒ 环境是「spec 的渲染结果」而非「盘上累积历史」。接口契约见
`sandbox-runtime-architecture.html` §17：

- 改环境只能走 `Session.Install(Pip/Apt/NpmCi…)` / `Setup(Shell)` / `Files()/FromDir/FromArchive` / `SetEnv`，
  每个动作记成 `Step{Kind, Tool, Args, Digest}`；`Exec` 里的一次运行不算改环境、不入 spec。
- 重放带 **Step 级缓存**：`stepKey = sha256(baseImageDigest|Tool|Args|envFingerprint)`，后端复用框架已有的
  `graph.CacheStore`（`pkg/hno/graph/policy.go:31-42`），因此 Rebuild 通常远低于全量重跑（`npm ci` 命中即省 3 分钟）。
- 偶然状态：`lockdown = enforce`（断源 + Policy 拦裸安装，唯一能保证重建等价的模式）/ `detect`（抽样指纹 +
  `Session.Drift()`，尽力而为、<b>不是安全边界</b>）/ `off`（放弃 #9 前提，`Diagnose` 报不可靠）。
- 工作区偶然文件：默认 `RebuildPolicy = CollectThenRebuild`，把 delta tar 成 `Step{Kind:materialize}`
  追加进 spec ⇒ 偶然状态被提升为声明状态；另有 `Abort` / `Discard`。
- 新增待决 #12（lockdown 默认档：开发 detect / 生产 enforce）、#13（撞 enforce 时给 `use_install_api` 建议还是直接失败）。

## 15. 决议 #12 / #13 + 台账重建（v0.7）

- **D-15**：`lockdown` 默认按 Profile 分档 —— 开发 `detect`、生产 `enforce`，`off` 不作生产默认。
- **D-16**：模型在 `Exec` 里改环境撞 `enforce` 时不白拦，返回 `Failure.NextAction=use_install_api`
  与等价写法 Hint，并把被拦命令记入 `DriftReport`。

⚠️ 过程修正：架构文档 §12 的决议表在 v0.2 之后**从未真正更新**（一次批量编辑被中断，当时误报为已同步）。
v0.7 已用统一 ID 重建为 §12.1「决议台账」，旧编号（#1–#15 各表自定）作废，以 D-01…D-19 为唯一事实源。

- **已定 10**：D-01 命名 / D-02 介入点 / D-03 remote sidecar / D-04 保留 hosted / D-05 探测+Remediation /
  D-11 RebuildFromSpec / D-13 FailFast+显式降级 / D-14 快照默认关 / D-15、D-16。
- **待拍 9**：D-06 serial、D-07 KVM 缺失 FailFast 推导确认、D-08 MITM 默认、D-09 TTL、D-10 冻结 Executor、
  D-12 CopySnapshot、D-17 产物超限、**D-18 分期切法（必须人定）**、D-19 P8 合并与隔离档数。

全部决议已汇总为架构文档 §12.1「决议台账」（单一事实源）：#0/#1/#7/#8/#9/#10/#11/#12/#13 已定；
仍待拍 **#2 同会话并发（建议 serial）**、**#3 KVM 缺失（由 #10 推导 FailFast，待确认）**、
**#4 MITM 默认（建议仅 hosted 档）**、**#5 是否冻结 run.Executor（建议冻结）**、
**#6 分期切法（影响节奏，必须人定）**、**#14 默认挂载模式（建议 CopySnapshot）**、
**#15 产物超限额（建议按 Required 区分）**。

## 16. v1.0 定稿：19 条决议全部关闭

上文的「新增待决 #7–#13」是历史叙述，现统一以架构文档 §12.1 台账 **D-01…D-19** 为准，**19 条全部已定**。
最后 8 条以「采纳默认」方式关闭：D-06 serial、D-07 FailFast（确认 D-13 推导成立）、D-08 MITM 仅 hosted 开、
D-09 30m/5m、D-10 冻结 `run.Executor`、D-12 `CopySnapshot`、D-17 产物按 `Required` 区分、D-19 P8a→P2。

**D-18 交付切法（唯一需要人判断的，已定「先最小可用」）** —— 这是我替你做的选择，理由与可逆性写清楚：

| 批次 | 内容 | 完成门 |
|---|---|---|
| B1 契约 | P0 实测门 + P1 类型/错误/能力位 + `stub` Provider | `go test -race ./pkg/hno/sandbox/...` 全绿；`Probe→Resolve→Validate→Run` 贯通；§11 数字成表 |
| B2 最小可用 | P2 Session+Admission（含原 P8a 清扫/孤儿回收）+ P3 Files 与执行合一 + §13 工作区/产物 + §16 失败分类与 Rebuild + §17 Replayable Spec | 真实项目跑通：`FromDir` 进 → `RunProject` → 取回 `ArtifactSet`；注入 `ClassLost` 后 `Rebuild` 恢复且依赖仍在 |
| B3 引擎 | P4 BoxLite remote（生产 `MicroVM`）+ E2B host 档（`MicroVMHosted`）；runsc 转测试档 | `Close` 后无孤儿 VM；BoxLite 与 E2B 各过一轮真实往返（非 stub） |
| B4 嵌入收尾 | `sandboxgraph.NewNode` + HITL/Checkpointer/Store 深整合 + P5 DX | graph 内跑通「挂起 → 审批 → `Resume` 回同一 box」；guide 数字与 `Diagnose` 文案一致 |

选「最小可用」而非一次全落的原因：§13/§16/§17 三者互相定义（声明式输入是 Rebuild 的前提、Rebuild 是产物可靠的前提），
必须同批；但 graph 嵌入与引擎扩展是加法，不改变契约，可以后置。B2 完成即对外可用。
**可逆性**：批次只是提交切分，不改 API；若 B1 后发现需要一次全落，直接合并提交即可，无回滚成本。

不变前提：**P0 实测门不许跳** —— 拿不到 §11 的 boot/拉镜像/单盒内存真实数字，就不接 BoxLite。

## 17. B2 开工：工作项声明与本机可用性（实测）

### 17.1 工作项（显式声明，不由文本推断）

| 字段 | 值 |
|---|---|
| id | `SB-B2a` |
| type | `planned`（多步、需真实引擎证据） |
| targets | `pkg/hno/sandbox/provider/docker/`（新增包）、本节 |
| allowedWrites | 仅上述新增路径；**不改** `pkg/hno/tools/run/**`、不改 `cmd/**` |
| failureClass | `additive-only`：新包若失败，只影响自身测试；现有 caller 零暴露面 |
| completion gate | §12.2 B2 门：`FromDir` 进 → `RunProject` → 取回 `ArtifactSet`；注入 `ClassLost` 后 `Rebuild` 恢复且依赖仍在 |
| side-effect policy | 默认零网络拉取、零新成本、零外部发布；真实容器往返仅在显式 `HNO_SANDBOX_DOCKER=1` 下运行 |

### 17.2 本机可用性实测（改变 B2/B3 路径判断）

```
docker CLI            PRESENT
docker daemon         server=0.9.4  os=Apple Containerization  driver=apple-container
podman                absent
runsc                 absent
boxlite               absent
qemu / firecracker    absent
/dev/kvm              absent   (Darwin arm64)
golangci-lint         ABSENT   → §12.2「每批过 make lint」当前无法满足，需另行批准安装
本地已有镜像           golang:1.26 (305MB), redis:7-alpine, postgres:16/17-alpine
```

结论（三条，都影响计划）：

1. **macOS 上容器档不需要 `/dev/kvm`**：`apple-container` 走 Apple 的 VFKit/HVF
   虚拟化栈，容器本身已在一台独立 VM 内。因此 §12.2 的 B2 门在本机**可在
   不拉镜像、不联网**的前提下用已存在的 `golang:1.26` 真实跑通，而
   不是只能靠 stub 声称。
2. **B2 的真实 provider 入口是容器档，不是 BoxLite**：BoxLite 仍受 D 系列的
   P0 实测门约束（无 `/dev/kvm`、无 boxlite 二进制 → 不接）。这把「B3 才有
   真引擎」修正为「B2 就有可验证的真引擎」，B3 只补 micro-VM 档。
3. **`make lint` 门当前不可达**：`golangci-lint` 未安装，安装属网络动作，
   需单独批准。在获得批准前，B2 的验证证据为 `gofmt -s`、`go vet`、
   `go test -race`，并明确记录 lint 未跑，而不是声称过了门。

### 17.3 SB-B2a 设计决定

- **命令边界可注入**：provider 通过 `commandRunner`（argv → stdout/stderr/exit）
  与 docker CLI 交谈。真实实现 `exec.CommandContext`，测试实现为 fake，于是
  契约测试与故障注入（`ClassLost`、孤儿清扫、产物超限）**离线可跑**，不需要
  守护进程；真实往返只在显式 opt-in 时发生，`make test` 永不隐式起容器。
- **能力位只声明已实现的**：容器档实际声明 `CapSession`/`CapFileBridge`/
  `CapStreaming`/`CapRebuild`（以 `provider/docker` 的 `Capabilities()` 为准）；
  **不**声明 `CapEgressAllowlist`（docker 只有 `--network=none` 的全关/全开，
  白名单是 BoxLite MITM 或代理的事），也不声明 `CapQueryStatus`/`CapSnapshot`，
  因此对相应请求返回 `*CapabilityError` 而不是假装生效——这正是 D-05 要消灭的
  静默降级。
  （本条原文多写了 `CapQueryStatus`/`CapAudit`，属文档漂移，已按代码修正。）
- **孤儿回收用 label，不用进程表**：所有容器打
  `hno.sandbox.session=<id>`，`Sweep` 以 label 查询引擎侧残留。进程重启后
  「谁是孤儿」必须有权威事实源，而这个事实源只能在引擎侧（§15 / D-19 的 P8a）。


### 17.4 SB-B2a 执行结果（真实引擎，已提交 6e6d6ef）

门命令：

```
HNO_SANDBOX_DOCKER=1 HNO_SANDBOX_BINARY=container HNO_SANDBOX_IMAGE=golang:1.26 \
  go test -count=1 -timeout 600s -run "TestReal" ./pkg/hno/sandbox/provider/docker/
```

真实事实：Apple `container` 在 Darwin/arm64 上跑 Linux aarch64 容器，**不需要
`/dev/kvm`**、不需要拉镜像、不需要联网。清扫后引擎侧带 `hno.sandbox.managed`
标的容器数为 0——回收真实性以引擎为证，不以进程内 map 为证。

真实引擎暴露 9 个缺陷（probe 说谎、`ProviderConfig.Raw` 从不填充、`alive()` 写死
docker `inspect` 导致健康容器被判 `ClassLost`、CLI 捕获无界、失败阶段被盖章
succeeded、`PhaseReport` 不带输出、核心校验 `Workspace.SourceDir` 却从不投递、
`statProbe` 引号不配对导致每个 `Stat` 谎报文件不存在、`Artifact.Size`/`Digest`
从不赋值 + 惰性句柄指向已关闭环境），另加 `WorkspaceMaxBytes: 0` 拒掉每个文件。
逐条修复并各带测试。

### 17.5 SB-B2b：能力声明必须可测量（本轮）

B2a 的教训不是「docker provider 有 9 个 bug」，而是**能力位是 provider 对自己
的断言，而这份断言没有任何执行机制**。于是 B2b 不补一个 bug，补这一类：

- **`CapStreaming` 被谎报**。契约原文是「delivers output incrementally instead
  of only at exit」，而 provider 的实现是**命令退出后发一整块 stdout、stderr
  一个字节都不发**。后果不是性能：一个只在 stderr 说话的构建，对流式监听者呈现
  为完全静默的画面，而能力位仍然亮着。
- **测试替身与 provider 朝同一方向撒谎**，所以这条路径离线永远测不出来：假
  runner 从不调用流式通道，断言只是在校验假象本身。修复必须同时改两侧，并把
  「假行为下测试必须失败」当作验证步骤（本轮已实测：旧实现下
  `TestStderrStreamsToo` 与 `TestChunksArriveBeforeExit` 双双 FAIL）。
- **`Report.OutputSent`/`OutputSkipped` 有文档、全代码库无人赋值**，与
  `Artifact.Size` 同类。现由核心在 fanout 处按流测量后填充：它是**测量值**，不是
  provider 的承诺。
- **违约必须不依赖调用方配置 Observer 就能被发现**。否则「没配 Observer ⇒ 静默」
  这个我本轮正在修的缺陷会在新位置复现。因此提供 `Report.StreamingContract()`：
  由数据机械推导（有输出而零 chunk ⇒ `*StreamingContractError`，带
  `Remediation()`），同时发一条 fault 事件供观察者消费。
- **违约不翻转运行结果**。第一版把违约写进 `Report.Failure`，立刻打挂一个通过的
  工程测试——这与「把非零退出盖章 succeeded」是同一类错误的反方向：`Failure`
  只回答「我的 payload 为什么没产出」，provider 遥测不诚实没有改变代码做了什么。
- **Sink 错误曾被 provider 吞掉**。`fanout.Report` 在 `OverflowFault` 绑定下返回
  `*HookFaultError`，而实现里写的是 `_ = sink.Report(...)`，等于让
  `WithOverflow(OverflowFault)` 这一公开策略在实践中不可达。现捕获首个非取消错误
  并在运行结束后上报，同时保留已捕获输出：区分「我的代码成功了但没人接收」与
  「代码失败」。（曾误判「没有选项能选 OverflowFault」，核对 `WithOverflow` 后
  撤回，未动 API。）
- **真实门新增** `TestRealStreamingIsIncremental`：容器内 `echo; sleep 2; echo`，
  断言两行作为**不同时刻的独立投递**到达且间隔 ≥500ms，并校验
  `Report.OutputSent` 与 sink 实际收到的字节一致。惰性/缓冲式投递不可能通过。

离线：`go test -race ./pkg/hno/sandbox/...` 全绿，core 覆盖率 68.7%。
真实：5/5（工程往返、实时输出、`ClassLost` 注入、引擎侧孤儿清扫、增量流式）。

### 17.6 SB-B2c：能力声明核对 + 真实门 flake 与跨进程数据损毁

本轮从「把 B2b 的测量思路推广到其余能力位」开始，实际产出四类修复。全部有
真实引擎证据，且两个最严重的缺陷是用**双进程实验**证明的。

**（一）能力位与实现接口从不对账。** 探针打印出的错误自相矛盾：

```
provider "liar2" lacks capability "snapshot" (has: session,snapshot,clone)
```

同一句话既否认又承认同一个位，而补救建议（换引擎）是错的——引擎没问题，缺的是
它的实现。修复：`CapabilitySet.VerifyClaims(Provider)` 在 `Open` 采纳 provider 前
把声明与核心实际断言的接口对账（表 `interfaceGatedCaps` 从核心自己的断言点推导
而来，不靠猜），矛盾则拒绝采纳；`CapabilityError` 区分「未声明」与「声明了但无
实现」，并首次带上 `Remediation()`。

**（二）枚举里的空词汇。** 逐一统计核心查询次数后确认：`CapAudit`、
`CapIdleSuspend`、`CapQueryStatus`、`CapPTY`、`CapHostedTrust` **没有任何核心代码
路径消费**（`CapQueryStatus` 在注释里被写成 `Attacher` 的门，实际代码只看
`CapBackground`）。调用方检查这些位会得到一个不产生任何行为的「是」。
处理是建语义登记表 `capabilitySemanticsTable`（未接线者显式记录 Owner=""并附
原因），`Unconsumed()` 经 `Diagnose` 报出，并用
`TestEveryCapabilityHasDeclaredSemantics` 钉住「枚举长于表」这件事。没有为这些位
发明语义——那是设计工作，不是修复。

**（三）真实门 flake 的根因：create 与 start 共用一个 deadline。** 门在 4 次里挂
2 次，症状是 `container start exited nonzero` **且没有任何原因**。小预算
（`startup_timeout=1200ms`）可确定性复现：create 吃满启动预算，start 拿到已过期
ctx，CLI 被杀、非零退出、stderr 为空。修复：两步各得独立窗口
（`startupWindow()`），并新增 `engineFailure()`——被 deadline 杀死时必须说是
deadline 及窗口长度，有 stderr 时保留引擎原文，全空时如实说「无输出」而不是给
出一条空原因。顺带发现并修复：`startup_timeout` **写在文档形状里却从未被读取**
（又一个静默 no-op），现在解析并拒绝未知/畸形键。

**（四）跨进程数据损毁（本轮最严重）。** 原 `Sweep` 用「不在本进程内存 map」判定
孤儿。双进程实验：A 持有 session 并写入 `victim.txt`，B 调
`Reclaimer().Prune()`，结果——

```
修复前： VICTEM LOST: container 477ac2b9… not found     → FAIL（数据被毁）
修复后： VICTIM survived, content="still in use"        → PASS（B 仍跑过 Prune）
```

内存 map 对引擎没有权威，而并发进程是 agent/CI 的常态。修复：容器打
`hno.sandbox.owner`（host/pid/nonce）；`listedContainer` 带引擎自述的
`Running`；`Sweep` 如实报告全部外来容器；`Prune` **只回收已停止的残骸**，拒删
外来运行中容器并把拒绝记入 `SkippedPrune()`（空结果不能被读成「没东西可清」）；
`prune_running` 为显式 opt-in，供单租户机器使用。

同轮修复 probe 泄漏：`Probe` 每次 `create` 一个**固定名**容器且从不删除——每次
`Open()` 留一个残骸，且并发第二个进程必然撞上 `container already exists`，把健康
机器误报为不可用引擎（与 B2a 的「说谎 probe」对称的危害）。现在名字带 pid+nonce，
探测后删除，失败也清理。

自我纠正两处：曾在测试里用固定容器名，并发时自己复现了刚修掉的撞名缺陷（已改
带 pid）；曾误判「`OverflowFault` 无公开选项可选」，核对 `WithOverflow` 后撤回并
未动 API。

**证据。** 离线 `go test -race` 全绿，core 覆盖 69.3%、docker provider 57.5%。
真实门 6 个，单进程连跑 4/4 全绿（修复前 4 次挂 2 次）；**两个进程并发各 6/6
PASS、零 SKIP、零 FAIL**，并发后引擎侧 `hno.sandbox.managed` 容器残留为 **0**。

### 17.7 Docker/Container provider 引擎属性（`ProviderConfig.Raw`）

这些是 provider 唯一接受并真正读取的引擎级配置。此前无表格文档，是 `startup_timeout`
能被写成文档形状却无人读取的原因之一：没有权威的清单，就没有人会去核对。

| 键 | 类型 | 默认 | 语义 |
|---|---|---|---|
| `binary` | string | `docker` | 引擎 CLI 名；Apple Containerization 填 `container` |
| `dialect` | string | 按 binary 推断 | `docker` 或 `apple`；同一 CLI 家族共用 provider，只换参数方言 |
| `allow_bind_mounts` | bool | `false` | 是否允许把宿主目录 bind 进容器；关闭时工作区走 `cp into container` |
| `startup_timeout` | duration 字符串或数值 | `60s` | **每个**引擎启动步骤（create、start）各自的窗口，不是整个启动过程的总预算 |
| `prune_running` | bool | `false` | 是否允许 `Prune` 删除「运行中但不由本进程持有」的容器。仅单租户机器应开启；双进程实验证明开启后互相销毁 workload |

未识别的键在 `NewProvider` 报错，不静默忽略——`Raw` 是引擎特定 passthrough，
一个拼错或被忽略的键会被读成「配置已生效」，而 provider 其实在用默认值；passthrough
表不能有这种失败模式。

### 17.8 SB-B2d：Request 上的静默 no-op —— ReadyProbe 与 Expose

延续 B2b/B2c 的思路，这次审计对象不是 provider 声明，而是**核心自己的请求字段**：
被 `clone()` 深拷贝、被 `Validate()` 检查、然后被读完为止没有任何代码使用的字段。

**（一）`ReadyProbe` 是装饰性字段。** `URL` / `Port` / `Pattern` / `Deadline` 四个字段
的消费点数量为 **0**。文档承诺 `Background` + probe = 「起服务并等到就绪」，实际是
「立刻返回，不管起没起」。证伪测试先行（`readiness_falsifier_test.go`）：

```
修复前： a background run whose probe never matched returned success → FAIL
修复前： a ready probe on a foreground run was accepted              → FAIL
```

实现（`pkg/hno/sandbox/readiness.go`）：

- `Pattern` 由**核心自己**匹配。为此 fanout 保留一段**有界输出窗口**（64 KiB 环形，
  保留最新），并且**与是否配置 Sink 无关**——B2b 的教训是「只有装了 Observer 才能
  验证的承诺本身就是静默 no-op」；这条用 `TestProbeWithoutSinkStillMatchesPattern`
  钉住，用 `TestFanoutTailStaysBounded` / `TestFanoutTailKeepsTheNewestOutput` 钉住
  成本与语义。
- `Port` / `URL` 只能从 guest 内部回答，因此新增可选 provider 接口
  `ReadinessProber`。**provider 做不到就明确拒绝**（`*UnsupportedProbeError`，
  `ClassPermanent`，附 `Remediation()` 指向可用的 Pattern），而不是「探测通过」——
  「静忽略」和「通过」在调用方看下来一模一样。
- 超时返回 `*ReadinessError`（`ClassUserCode`，永不自动重试），带上 provider 实际
  看到的 `LastSeen`（如 `connect: connection refused`）与回显的 probe，并首次补上
  `Remediation()`。
- 收紧校验：probe 不带 `Background` 现在**报错**（前台命令阻塞到退出，没有可以检查
  的时刻）；`Deadline < 0` 报错；非法正则报错（此前会被当字面量子串匹配，是没人要求
  的行为）。

**（二）`Expose` 是结构性不可实现。** 核心在 `checkCapabilities` 里检查
`CapPortForward`，看起来是接了；但端口映射必须在 **create** 时下发给引擎，而
`EnvSpec`（create 时唯一交给 provider 的切面）**不含 Expose 字段**。因此不只是某个
provider 没做——**任何 provider，现有与未来，都不可能履行这个字段**。

修复把语义放到它只能在的地方：

- `SessionSpec.Expose`（provision-time）+ `EnvSpec.Expose`，核心在 `newEnvironment`
  把映射交给 provider；`WithSessionExpose(...)` 是调用方的声明入口。
- run 级 `Request.Expose` 若与 session 已发布的映射不符 →
  `*ExposeConflictError`（`ClassPermanent`，`Remediation()` 指明应在创建 session 时声明）。
  静默丢弃会让调用方去探测一个根本没有转发的宿主端口。
- 用 `TestEnvSpecCarriesExpose` 证明 provider 在 create 时真的收到映射，
  用 `TestExposeOnForwardingSessionIsRefusedWhenNotPublished` /
  `TestExposeMatchingWhatWasPublishedIsAccepted` 证明拒绝与放行都正确。

**（三）上一轮的 bind 期核对立刻回敬了我。** 新写的测试 provider 声明了
`CapFileBridge` 却没实现 `FileBridge`，`Open` 直接拒绝：

```
- forwarder: declares "files" without implementing FileBridge
  (fix: implement FileBridge or drop the capability from Capabilities())
```

B2c 的机制在真实使用中抓住了一个新的谎报者——包括我自己刚写的那个。

**（四）测试双再次差点朝同方向撒谎。** 给内存 provider 加 `ProbeReady` 后，它就不再
是「无 prober 的 provider」，`UnsupportedProbeError` 路径**变得无法测试**。改为独立的
`sandboxtest.ProbedProvider` 类型，默认双保持不可探测：一个接口必须留一个真实实现者
（否则成功路径不可达），也必须留一个真实缺席者（否则拒绝路径不可达）。

**语义表同步。** `CapPortForward` 原写「port mapping requests」是过度声明——请求被读
了但没人能执行。现记录真实链路（SessionSpec→EnvSpec→create，mismatch→
ExposeConflictError）；`CapBackground` 注明 ReadyProbe 在 detached run 上求值。

**证据。** `go test -count=1 -race ./pkg/hno/sandbox/...` 全绿；core 覆盖 69.8%
（↑自 69.3%），docker provider 57.5%。真实门 6/6 `ok 16.6s`（create 通路因
EnvSpec 变更而受影响，故必须重跑）。新增 13 个测试，其中 2 个在修复前为红。

### 17.9 SB-B2e：恢复路径不重放步骤（已修）+ 步骤缓存语义待决（D-20）

延续「被校验但从不被读」的审计，这次的对象是**恢复与重建**。

**（一）已修：session 获取路径从不重放已记录步骤。**
`replayStep()` 此前只有一个调用点（`rebuild()` 内）。`Session()` 从记录里新建环境后
直接 `adopt`，于是「环境没了但记录还在」这一 session store 存在的**唯一理由**下，
新盒子是空的，而 session 状态显示 `Ready`。工作流记录的每个 `Install`/`Setup` 都丢失，
第一个失败被归因为 `ClassUserCode`——沙箱把自己的跳过算成调用方的代码错误。

修法：抽出**唯一的所有者** `replayRecorded()`，`rebuild()` 与 `Session()` 共用；恢复时
若重放失败，关闭新环境、状态置 `Lost`、返回 `StepReplayError`（不是静默 Ready）。
证据：`TestRecordedStepsReplayIntoAResumedEnvironment` 先观察为红（`history=[]`），
修复后为绿。

过程学到一个必须写下来的教训：**测试双两度差点让判断失真**。
1. 双只为 argv 命令写 history，不为 shell 步骤写；于是「Setup 没重放」和「Setup 重放
   了但双没记录」无法区分。补上「任何请求都留痕」后，恢复测试其实是通过的——
   我原本要报的第二个缺陷是**假的**。
2. 补 history 时我把它写成 `switch` 的一个 `case code != "":`，Go 的 switch 只执行
   第一个匹配分支，于是吞掉了 `#!echo` 等所有解释分支，弄坏 5 个既有测试。
   记录必须在 dispatch **之前**，不是当作一个分支。
3. 双的 `NewEnvironment` 有 `if existing { return existing }`：`MarkLost` 只置
   `sess.env = nil` 不 Close，于是「重建」拿回的是同一个容器，代际边界被隐藏。
   测任何跨代际语义必须显式 `DropEnvironment`。为此给双加了 `InstalledIn()`
   ——回答「盒子里到底有什么」，而不是「跑过什么」。

**（二）待决（D-20）：步骤缓存命中被当成关于容器的证据。**
测量结果（真实摘除盒子后）：

```
同 session：cached=1 replayed=0 history=[]   （工作流自己的依赖消失）
跨 session：cached=1 replayed=0 history=[]   （B 因 A 跑过同内容步骤而被跳过）
```

三个事实叠加导致：
- `StepCache` **默认开启**，且用进程内存 journal：
  `if c.StepCache == nil { c.StepCache = newMemoryJournal() }`
  —— 而紧邻的注释对 `Journal` 说的是「进程内存里的幂等承诺跨重启就是个谎，所以核心
  拒绝这个承诺」。同一函数内自相矛盾。
- `Step.Key()` 只哈希步骤内容：无 session、无镜像、无环境代际。
- `RebuildFromSpec` 从 `spec.Base` 新建盒子，**不恢复任何快照**，所以新盒子可证明从未
  运行过该步骤；跳过它就是把「A 进程里某处做过」当成「这个容器里有」。

现有 `TestStepCacheSkipsReplay` 把这一行为当作正确语义钉住了，因此这不是可以顺手改的
局部问题：它会改变 D-11 默认重试模式的成本结构。两个可选解：

- **B：从零新建的盒子一律重放**，缓存只对「继承状态的重建」生效。正确且最小；
  代价是 rebuild 恢复为「重装一切」，即 D-11 字面含义的成本。
- **C：`RebuildFromSpec` 在存在快照时先恢复最新快照**，只允许跳过该快照之前的步骤。
  保住成本收益且语义自洽（盒子里确实有），需要定义 D-11/D-12 的交互与代际标识。

新增 `TestStepCacheIsNotEvidenceAboutAFreshBox` 作为**决策验收件**：缺陷仍在时以
`D-20 OPEN` 显式 skip 并打印测量值；一旦选定方案并落地，它会自动变为通过或硬失败。

**（三）provider 侧不可达的重放代码。** `EnvSpec.ReplayFrom` / `CachedSteps` 在核心
**从未被赋值**（零赋值点），而 docker provider 的 `replay()` 遍历 `ReplayFrom`，注释
写着「核心会把剩下的放进 ReplayFrom，所以本 provider 不自创重试策略」——该注释为假，
代码路径不可达。核心重放已是唯一所有者且 provider 无关，故这段死代码与假注释应随
D-20 一并清理（避免下一个谎报者据此声称实现了重放）。

### 17.10 D-20 决议与实现：步骤缓存只在「继承状态」时才可跳过

**决议**：选 B。从零渲染的盒子一律重放；缓存只对真正继承前态的重建生效。

**实现**（`pkg/hno/sandbox`）：

1. `boxProvenance{lineage, upto, source}` 由 `newEnvironment` **返回**。造盒子的函数是唯一知道自己怎么造出来的地方，所以它是唯一有资格声明继承关系的来源。
2. 判据（`stepCached`）：`step.Sequence <= prov.upto` **且** 条目键为 `lineage + "|" + step.Key()`。
   - 由基础镜像渲染：`upto = -1` ⇒ 任何条目都无法授权跳过 ⇒ 一律重放。
   - `CloneAs`：子继承父 `lineage`，`upto = len(parent.Spec.Steps)-1` ⇒ 可跳过（真继承）。
   - `Restore`：取 `Checkpoint` 时记录的 `snapshotMark{lineage, upto}`；**无记录则退回 `upto = -1`**（未知水位绝不能授权跳过）。
3. `Step.Key()` 仍是纯内容摘要，但内容摘要不再是键的全部 —— 这是泄漏的根源所在。
4. 取消 `normalize()` 自动安装进程内存 `StepCache`。默认句柄不再携带一个能跨容器抑制工作的缓存；这与同一函数拒绝进程内存 `Journal` 的理由一致。

**实测的三条反向证伪**（每条都在被破坏时变红，证明机制是活的而非装饰）：

| 破坏 | 结果 |
|---|---|
| 去掉 `lineage` 作用域（恢复纯内容键） | `a foreign lineage skipped a step it never satisfied` |
| 去掉 `upto` 水位判据 | `a step beyond the watermark was skipped` |
| 恢复「按 env 反查 session」旧实现 | `the replay pipeline stored nothing`（缓存恒空，测试仍全绿 —— 静默空转） |

**过程中发现的同类缺陷（均已修）**：

- `replayStep` 只检查 `err`，不看退出码。provider 按运行契约把失败 payload 报成 `Report{ExitCode≠0}, err=nil`，于是**安装失败的步骤被记为已满足并写进缓存**。
- `Session.Install` / `Session.Setup` 同族且更严重：`Setup` 跑一条 exit 7 的命令 **返回 nil 并把步骤记成可重放状态** —— 调用方以为装好了，而之后每次重建都会忠实重跑一条已知失败的命令。新增 `runStateful`：状态型调用必须以退出码为证据，失败则**不记录该步骤**并返回带分类的错误。
- 测试替身 `Env.clone()` 复制文件却漏掉 `installed`/`history`，使「克隆继承状态」这条前提在替身里为假。
- 本轮自身引入的回归：`benignShellScript` 把以 `#` 开头的行当注释放行，而替身协议指令正是 `#!fail` ⇒ 失败命令报成功。已有测试钉住。
- `rebuild` 在重放前把 `sess.env` 置 nil、重放后才赋值，因此**按 env 反查 session 得到 lineage 恒为空**：结果看起来正确（全部重放），理由完全错误，缓存永久空转。改为由创建者显式返回 provenance。

**跨进程新缺陷（D-20 并发压力暴露，已修）**：两个真实门进程并发时，进程 B 刚 `create`（尚未 `start`）的容器被进程 A 的 `Prune()` 删除。原判据把「不在本进程内存表里 + 未运行」当作残留，正是此前已被否决的推断（所有权不能从单个进程的内存表推断），当时只补了 running 情形。现在 `SweepDetail` 增加 `HeldElsewhere`：owner label 的 host 为本机且 pid 存活 ⇒ 即使停止也**拒绝回收**；`Refused()` 与 `SkippedPrune()` 如实报告。新增真门断言，撤掉判据即变红。

**清理**：删除 `EnvSpec.ReplayFrom`/`CachedSteps` 与 docker `Environment.replay()`。核心从不填充 `ReplayFrom`，而该 provider 循环只处理 `StepInstall`、会**静默丢弃** setup/materialise 步骤；注释声称核心会传入剩余步骤，是假话。核心经由 `Environment.Exec` 是步骤重放的唯一所有者。

**验证**：`go test -race ./pkg/hno/sandbox/...` 绿；核心覆盖率 69.8% → **70.8%**，provider 57.5% → **57.9%**；真引擎门单进程 6/6；两个并发进程各 **6/6 PASS、0 SKIP、0 FAIL**，`hno.sandbox.managed` 残留 **0**。

### 17.11 B2 真引擎收尾：四项全部量过，不是推定

四项遗留都按「先在真引擎上量，再决定语义」处理。结论分两类：三项坐实（其中两项顺带挖出真缺陷），一项如实缺席。

**1. `CapRebuild` 真引擎验证 —— 通过，但暴露 `RecordInput` 不可重放**

`TestRealRebuildRestoresRecordedState` 用 setup + 录制输入两类步骤（不用 install，因为默认 egress=none，装包会去测网络而不是测重放）。断言的是**新容器里文件真的存在**，不是报告说它试过：

- `Rebuild` 后 `StateSource == "base image"`、`Cached == 0`（真容器不可能继承任何步骤）；
- `cat` 真读回 `built-by-replay` 与 `payload-42`。

第一跑就把两层真缺陷打出来：

- `RecordInput` 只存 `Path` + `Digest`，**不留任何内容来源** ⇒ `replayStep` 报 `materialize step "..." has no source` ⇒ **凡录制过输入的 session 一律无法重建**。
- 即便补上 source，旧重放只做 `test -e path`：**只检查、从不写回**。基础镜像渲染的新盒子里文件必然不存在。

修法（按所有权归位，而不是在 provider 里凑）：

- `Step` 新增 `Input *Input`，`RecordInput` 在**记录时**归一化为可重放载荷：`Bytes` 原样保留；`HostPath` 立即读成字节（记录的就是当时的内容，不是下次重建时恰好还在的）；`Reader` **当场拒绝** —— 它已被消费，记录下来就是让重建去兑现一个不可能兑现的承诺（D-05）。拒绝时不留残步。
- 重放走**核心自己的 `stageInputs`**。provider 的 `Exec` 从不负责解释 `Request.Inputs`（B2a 就定了 staging 是核心管线职责），所以让引擎「运行」一个纯内容步骤其实什么也没写。
- 后置校验用 **provider 自己的文件服务 `Stat`**，不再伪造一条 shell 命令：用 shell 断言测的是引擎的 shell 语法，不是「文件在不在」。校验通过后**直接返回**，不发明 payload。

**2. Request 层 `ReadyProbe` —— in-guest 探针坐实**

`TestRealReadinessProbeSeesARealListener` 先问一个**没人监听**的端口，必须回「未就绪」（若这里回 ready，说明探针压根没看 guest），再起真监听器轮询到就绪，并断言 `observed` 非空。

两处真机事实纠正了实现里的想当然：

- guest 的 `/proc/net/tcp` 打印**大写** hex（实测 `00000000:AEA9`）。测试端口刻意选 **44713 = `AEA9`（含字母）**：若用 39217 = `0x9911`，大小写写错也能通过 —— 证伪测试必须让机制的错误方向可见。
- `cmd &` 是 exec 会话的子进程，**exec 返回就被带走**。必须 `setsid`。第一版测试正是这样「探针没问题、被测服务不存在」。

`Port` 探测的两级实现（bash `/dev/tcp` 优先，退回 `/proc/net/tcp{,6}` 监听表）与 `URL`（guest `curl`/`wget`，无 HTTP 客户端则 `*UnsupportedProbeError`）都由真引擎确认。「未就绪」不是错误；非 2xx 也只算未就绪。

同时把「档位不匹配」提到 `Diagnose`：docker provider 有意不声明 `CapBackground`（`ReadyProbe` 语义上要求常驻进程），所以它**在 Request 层无法服务任何探针**。这现在是 `Diagnose` 的一行输出（带 remedy），而不是等到某次依赖探针的服务启动失败才发现。离线侧新增 `TestDiagnoseStatesWhetherReadinessIsPossible` 与 `TestDiagnoseNamesTheBackgroundGap`（后者用一个刻意不带 `CapBackground` 的真实 provider 走进另一分支，不是用散文断言）。

**3. TTL / idle —— 真引擎坐实**

`TestRealExpiredSessionReclaimsTheContainer` 在真引擎上问两个问题：`env.Close`（核心在 session 过期时走的路径）之后容器**是否还在跑**；过期是否发出 `EventSessionExpired` 且后续 run 不再假装环境还在。引擎状态一律经 `ContainerRunning` 查询，与 `ImagePresent` 同一先例 —— Apple CLI 的 `inspect` **不接受 `--format`**，测试里裸拼 docker 形状会把健康容器读成「未运行」。

顺带消灭一个新的静默空转：把 provider 启动窗口从 `Timeouts.Connect` 改接到 `Timeouts.Provision` 之后，`Connect` 成了**有文档、零消费者**的字段。按同类标准补上 `probeBounded`：探针必须有了自己的窗口（此前 daemon 挂住就无限挂住），并且所有权与关闭分开 —— 探测**已绑定**的 provider 绝不能把它关掉，否则句柄被探针废掉；只有「只是看一眼」的候选走 `probeAndClose`。

**4. `runsc` / gVisor —— 本机如实缺席**

`TestGVisorTierIsExercisedOrExplicitlyAbsent` 是**可见 SKIP**，并说明原因：Apple `container info` 不回答 runtime 列表（`exit status 64`）。这不是失败，但必须让它显形 —— 否则一套静静跳过的测试会让设计的 syscall-filtered 档位看起来「有实现且有覆盖」，而本机只有 container 一档。

档位实现为 **provider 的属性而非第二个 provider**：`Config.Runtime` / `ProviderConfig.Raw["runtime"]`（如 `"runsc"`），经由 `createArgs` 落到 `--runtime`。同一个 CLI、同一套生命周期词汇，隔离强度不同。`Probe` 也带上同一 runtime —— 探引擎默认档而去 create 另一档，会把「引擎可用」报成一个根本起不来的配置可用。`TestRuntimeAttributeReachesCreate` 钉住两处，撤掉 `Runtime` 传参即变红。

**诊断缺陷（真门跑出的副产品，已修 + 证伪）**

显式指定一个**从未注册**的 provider，旧报错是 `no registered provider met the required isolation` —— 把读者送去爬隔离梯子，而真正的问题只是名字没注册或拼错。现在 headline 说出到底在找谁，并把「先注册再 Open」的 remedy 带上。`TestUnknownProviderNamesTheProvider` 刻意断言 `UnavailableError.Reason` **headline 本身**：`Error()` 会附上所有 attempt，名字出现在整串里与否跟正确性无关，断言整串会连旧文案一起放过。

**验证**：离线 `go test -race ./pkg/hno/sandbox/...` 绿；真引擎门 **9 PASS / 0 FAIL / 1 可见 SKIP**；两个并发进程各 9/0/1，`hno.sandbox.managed` 残留 **0**；覆盖率核心 70.8% → **72.6%**，docker provider 57.9% → **59.6%**。本轮新增/调整 19 个测试，其中 5 个反向证伪均在破坏机制时变红（含一次自我纠正：第一版 `TestRecordedInputSurvivesRebuild` 在关掉机制时仍绿，据此把 materialise 重放改为经核心 staging + 文件服务校验，之后关掉机制可读回 `file not found`）。
