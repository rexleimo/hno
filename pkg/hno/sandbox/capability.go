package sandbox

import (
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
