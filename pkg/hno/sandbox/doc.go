// Package sandbox is the stateful sandbox runtime core of HNO.
//
// It replaces the one-shot "open an executor, run a payload, throw it away"
// shape with a single core that owns policy, capability negotiation, session
// lifecycle, workspace materialisation, artefact collection and failure
// classification, while engines (BoxLite micro-VM, gVisor, containers, hosted
// providers such as E2B) live behind Provider implementations in subpackages.
//
// Dependency rule: this package must never import an engine SDK, a container
// runtime, or pkg/hno/tools/run. Providers depend on this package, not the
// other way round. The core knows only two things about an engine: the
// isolation level it claims and the capability bits it publishes.
//
// Design traceability lives in docs/design/sandbox-runtime-architecture.html;
// comments citing "D-nn" refer to the decision ledger in its §12.1, so a
// reader can trace any contract back to the decision that fixed it.
//
// The contract layer ships with an in-memory provider (package sandboxtest)
// that satisfies the same assertions real engines must pass, which is how the
// pipeline is tested without a hypervisor on the machine (D-18 batch B1).
package sandbox
