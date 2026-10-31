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
