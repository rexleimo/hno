package sandbox

import "fmt"

// Formation describes how the sandbox engine is attached. It is orthogonal to
// Isolation: a remote sidecar can host a micro-VM, an in-process build can run
// a gVisor container. The earlier draft used one axis for both, which made the
// cost model impossible to read (D-01).
type Formation uint8

const (
	// FormationUnset resolves during open.
	FormationUnset Formation = iota
	// FormationInProc links the engine into this process: fastest path, but
	// CGO and hypervisor entitlements move into our binary and every release
	// of the engine becomes a release of ours.
	FormationInProc
	// FormationRemote talks to a locally launched engine process over a private
	// socket. This is the default delivery shape: the library stays
	// CGO_ENABLED=0 and `go install` works (§9).
	FormationRemote
	// FormationSidecar talks to an already-running long-lived daemon over a
	// socket: shared boot cache, no per-process spawn.
	FormationSidecar
	// FormationHosted talks to a provider-operated control plane over HTTPS
	// (E2B). Costs and data locality leave the machine here.
	FormationHosted
)

// String names the formation for Status and diagnostics.
func (f Formation) String() string {
	switch f {
	case FormationInProc:
		return "inproc"
	case FormationRemote:
		return "remote"
	case FormationSidecar:
		return "sidecar"
	case FormationHosted:
		return "hosted"
	default:
		return "unset"
	}
}

// IsLocal reports whether the engine runs on this machine.
func (f Formation) IsLocal() bool {
	return f == FormationInProc || f == FormationRemote || f == FormationSidecar
}

// ParseFormation accepts the config spelling of a formation.
func ParseFormation(s string) (Formation, error) {
	switch s {
	case "", "auto":
		return FormationUnset, nil
	case "inproc", "cgo", "lib":
		return FormationInProc, nil
	case "remote", "spawn":
		return FormationRemote, nil
	case "sidecar", "daemon":
		return FormationSidecar, nil
	case "hosted", "cloud":
		return FormationHosted, nil
	default:
		return FormationUnset, fmt.Errorf("unknown formation %q (want inproc, remote, sidecar or hosted)", s)
	}
}
