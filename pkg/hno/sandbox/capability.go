package sandbox

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Capability is a single feature a provider can opt into. The core never
// assumes an engine supports anything: it checks bits, and a missing bit turns
// into a *CapabilityError instead of a silent host-side fallback (D-05).
type Capability uint32

const (
	CapUnknown Capability = 1 << iota
	// CapSession keeps an environment alive across Exec calls (the reason the
	// micro-VM tier exists at all).
	CapSession
	// CapFileBridge lets the execution environment and the file service share
	// one workspace root. Without it Files() is refused for that session.
	CapFileBridge
	// CapStreaming delivers output incrementally instead of only at exit.
	CapStreaming
	// CapBackground supports detached runs plus Wait/Logs on a RunRef.
	CapBackground
	// CapQueryStatus can report the terminal state of a run that was already
	// submitted. Without it, a disconnected Exec stays ClassTimeout/uncertain
	// and may never be retried automatically (§16.1).
	CapQueryStatus
	// CapSnapshot exports and restores environment state inside one engine.
	CapSnapshot
	// CapClone forks a session cheaply (copy-on-write) for fan-out attempts.
	CapClone
	// CapRebuild can re-render a session from its recorded spec.
	CapRebuild
	// CapPTY attaches an interactive terminal / kernel to a session.
	CapPTY
	// CapPortForward exposes a guest port to the host.
	CapPortForward
	// CapEgressAllowlist enforces a network allowlist rather than all-or-nothing.
	CapEgressAllowlist
	// CapSecretSubstitution injects secrets at a host-side proxy so they never
	// enter the guest. Hosted tier only by default (D-08).
	CapSecretSubstitution
	// CapIdleSuspend lets the engine park an idle session and resume it.
	CapIdleSuspend
	// CapHostedTrust marks the environment as somebody else's machine: audit,
	// retention and egress leave the local boundary.
	CapHostedTrust
	// CapAudit emits per-run records for external ingestion.
	CapAudit
)

var capabilityNames = map[Capability]string{
	CapUnknown:            "unknown",
	CapSession:            "session",
	CapFileBridge:         "files",
	CapStreaming:          "streaming",
	CapBackground:         "background",
	CapQueryStatus:        "query-status",
	CapSnapshot:           "snapshot",
	CapClone:              "clone",
	CapRebuild:            "rebuild",
	CapPTY:                "pty",
	CapPortForward:        "port-forward",
	CapEgressAllowlist:    "egress-allowlist",
	CapSecretSubstitution: "secret-substitution",
	CapIdleSuspend:        "idle-suspend",
	CapHostedTrust:        "hosted-trust",
	CapAudit:              "audit",
}

// String names a single capability bit. An unnamed bit renders as its hex
// value rather than silently printing a number as text, so a mistake in the
// table above is visible instead of looking like a corrupt message.
func (c Capability) String() string {
	if name, ok := capabilityNames[c]; ok {
		return name
	}
	return "capability(0x" + strconv.FormatUint(uint64(c), 16) + ")"
}

// CapabilitySet is the immutable feature list a provider publishes at probe
// time. It is a value type so a snapshot of it can be embedded in Status and
// in every audit record without aliasing provider state.
type CapabilitySet uint32

// Capabilities builds a set from a list of bits.
func Capabilities(caps ...Capability) CapabilitySet {
	var out CapabilitySet
	for _, c := range caps {
		out |= CapabilitySet(c)
	}
	return out
}

// Has reports whether every bit in want is present. Zero want is vacuously
// true, which keeps "no requirement" checks free of special cases.
func (s CapabilitySet) Has(want Capability) bool { return s&CapabilitySet(want) == CapabilitySet(want) }

// Missing returns the bits in want that s does not carry, for error messages
// that name the exact gap instead of "unsupported".
func (s CapabilitySet) Missing(want Capability) Capability { return want &^ Capability(s) }

// Add returns a set extended with extra bits. Providers build their set once;
// mutation is intentionally not offered.
func (s CapabilitySet) Add(extra Capability) CapabilitySet { return s | CapabilitySet(extra) }

// Strings lists the capability names, sorted for stable diagnostics.
func (s CapabilitySet) Strings() []string {
	var out []string
	for c := Capability(1); c != 0; c <<= 1 {
		if s.Has(c) {
			out = append(out, capabilityNames[c])
		}
	}
	return out
}

// String renders the set compactly for log lines.
func (s CapabilitySet) String() string { return strings.Join(s.Strings(), ",") }

// --- capability claim verification -------------------------------------

// interfaceGated records, per capability, the optional interface the core needs in
// order to honour it, and the check that says whether a provider carries it.
//
// The set came from reading the core's own assertions rather than from the enum:
// every place that refuses a call with *CapabilityError also performs a type
// assertion, and those two facts have to agree. This table is where they are made
// to agree.
//
// A capability the core consults without an interface (CapSession, CapRebuild,
// CapStreaming, CapEgressAllowlist) is a behavioural promise verified elsewhere —
// CapStreaming by measured chunk delivery, for instance. A capability carrying an
// interface, by contrast, has a proof available at bind time, so a provider can be
// caught before it runs anything.
type interfaceGated struct {
	// Iface names the interface in errors, so a provider author reads a name and
	// not a reflect dump.
	Iface string
	// Holds reports whether the provider implements it.
	Holds func(Provider) bool
}

var interfaceGatedCaps = []struct {
	Cap Capability
	Req interfaceGated
}{
	{CapFileBridge, interfaceGated{"FileBridge", func(p Provider) bool { _, ok := p.(FileBridge); return ok }}},
	{CapSnapshot, interfaceGated{"Snapshotter", func(p Provider) bool { _, ok := p.(Snapshotter); return ok }}},
	{CapClone, interfaceGated{"Cloner", func(p Provider) bool { _, ok := p.(Cloner); return ok }}},
	{CapBackground, interfaceGated{"Attacher", func(p Provider) bool { _, ok := p.(Attacher); return ok }}},
}

// VerifyClaims checks a provider's declared capabilities against the interfaces the
// core requires to honour them, and returns one error per contradiction.
//
// It exists because a capability set is a provider asserting about itself, and the
// audit found the assertion unreliable in both directions: a declared bit with no
// implementation behind it, and a bit the core never consumes. The first is
// catchable here, and catching it at bind time is the difference between "Open
// refused" and "the caller discovers it on their 400th run, three weeks later".
//
// Called by Open. It is exported for provider authors who want to fail their own
// unit tests rather than the core's.
func (cs CapabilitySet) VerifyClaims(p Provider) []error {
	if p == nil {
		return []error{errors.New("sandbox: capability verification on a nil provider")}
	}
	var problems []error
	for _, e := range interfaceGatedCaps {
		if !cs.Has(e.Cap) {
			continue
		}
		if e.Req.Holds(p) {
			continue
		}
		problems = append(problems, &UnimplementedCapabilityError{
			Provider:   p.Name(),
			Capability: e.Cap,
			Iface:      e.Req.Iface,
			Declared:   cs,
		})
	}
	return problems
}

// UnimplementedCapabilityError reports a provider that declares a capability the
// core cannot service because the backing interface is absent.
//
// This is a different fact from "the provider does not have that capability", and
// conflating them produced an error message that contradicted itself: the caller
// was told to look for a capability the same message listed as present, and the
// remedy (pick another engine) was wrong — the engine is right, its implementation
// is missing.
type UnimplementedCapabilityError struct {
	Provider   string
	Capability Capability
	Iface      string
	Declared   CapabilitySet
}

func (e *UnimplementedCapabilityError) Error() string {
	return fmt.Sprintf("sandbox: provider %q declares %q but does not implement %s, so the core cannot honour the claim (declared: %s)",
		e.Provider, e.Capability, e.Iface, strings.Join(e.Declared.Strings(), ","))
}

// Remediation names the fix, which is on the provider side.
func (e *UnimplementedCapabilityError) Remediation() []string {
	return []string{
		fmt.Sprintf("implement %s on the provider, or stop declaring %q", e.Iface, e.Capability),
		"the capability is not missing: choosing a different engine will not help",
	}
}

// Class keeps this permanent: a missing method is not transient.
func (e *UnimplementedCapabilityError) Class() FailureClass { return ClassPermanent }

// --- capability semantics registry -------------------------------------

// capabilitySemantics records, for every bit in the enum, what the core actually
// does with it. The point is that a capability name is a promise to callers, and
// "the provider declared it" and "the behaviour follows" are two separate facts.
//
// Found by enumeration rather than by reading one call site: CapAudit,
// CapIdleSuspend, CapQueryStatus, CapPTY and CapHostedTrust are declared or
// documented in places, yet no core code path consults them, so a caller that checked
// `caps.Has(CapAudit)` received a yes that produced no behaviour. That is the same
// defect class as an unbackable streaming claim, one level up: the vocabulary existed
// without the semantics.
//
// Entries with an empty Owner are the honest record of "not implemented", and
// TestEveryCapabilityHasDeclaredSemantics keeps the table from silently lagging the
// enum as bits are added.
type capabilitySemantics struct {
	// Owner names the core mechanism that consults the bit, or "" when nothing does.
	Owner string
	// Note explains a non-obvious boundary.
	Note string
}

var capabilitySemanticsTable = map[Capability]capabilitySemantics{
	CapSession:            {Owner: "Session/Exec path; refuses session reuse without it"},
	CapFileBridge:         {Owner: "Session.Files and workspace staging", Note: "gated on the FileBridge interface"},
	CapStreaming:          {Owner: "measured at the fanout, not trusted: Report.StreamingContract"},
	CapBackground:         {Owner: "detached runs and reattach", Note: "gated on the Attacher interface; ReadyProbe is evaluated on detached runs"},
	CapQueryStatus:        {Owner: "", Note: "no core path consults it; the docs mention it, the code gates on CapBackground"},
	CapSnapshot:           {Owner: "Session.Checkpoint/Rollback", Note: "gated on the Snapshotter interface"},
	CapClone:              {Owner: "Session.CloneAs", Note: "gated on the Cloner interface"},
	CapRebuild:            {Owner: "RebuildFromSpec path and drift detection"},
	CapPTY:                {Owner: "", Note: "StreamPTY exists; nothing enforces or provides it"},
	CapPortForward:        {Owner: "SessionSpec.Expose is carried into EnvSpec at create; a run-level mismatch raises ExposeConflictError", Note: "port mapping is a create-time property of the substrate, so Request.Expose alone can only reuse what the session published"},
	CapEgressAllowlist:    {Owner: "Open refuses an allowlist posture without it"},
	CapSecretSubstitution: {Owner: "egress secret substitution requests"},
	CapIdleSuspend:        {Owner: "", Note: "nothing suspends an idle environment on this bit"},
	CapHostedTrust:        {Owner: "", Note: "the isolation rank and Formation carry the real distinction"},
	CapAudit:              {Owner: "", Note: "audit records are written whenever a sink exists, independent of this bit"},
}

// Unconsumed reports the declared bits the core has no behaviour for.
//
// A provider may declare them truthfully — the engine might honour them on its own
// side — but the caller needs to know that no core behaviour follows, because
// otherwise a capability check reads as a guarantee. Reported through Preflight and
// Diagnose rather than refused at Open: rejecting a working provider would be a
// regression, and inventing semantics for these bits is design work, not a fix.
// allCapabilities lists every bit the enum defines, in declaration order. It exists
// so the semantics table can be checked for completeness: a new bit that is not
// registered here fails a test rather than becoming another silent word.
var allCapabilities = []Capability{
	CapSession, CapFileBridge, CapStreaming, CapBackground, CapQueryStatus,
	CapSnapshot, CapClone, CapRebuild, CapPTY, CapPortForward,
	CapEgressAllowlist, CapSecretSubstitution, CapIdleSuspend, CapHostedTrust, CapAudit,
}

func (cs CapabilitySet) Unconsumed() []Capability {
	var out []Capability
	for _, c := range allCapabilities {
		if !cs.Has(c) {
			continue
		}
		sem, ok := capabilitySemanticsTable[c]
		if ok && sem.Owner == "" {
			out = append(out, c)
		}
	}
	return out
}

// SemanticsOf names what the core does with a capability, for diagnostics and for
// provider authors. It reports false for a bit outside the enum.
func SemanticsOf(c Capability) (capabilitySemantics, bool) {
	s, ok := capabilitySemanticsTable[c]
	return s, ok
}
