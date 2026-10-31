package sandbox

import (
	"fmt"
	"strings"
)

// Isolation names the strength of the boundary a provider claims. It is an
// ordered enum, deliberately not a number: the earlier draft used "L1/L2/L3"
// for both architecture layers and isolation strength and the ambiguity cost a
// review round (D-01).
type Isolation uint8

const (
	// IsolationUnknown is the zero value. Anything that reaches a decision
	// point with this value is a bug in the caller, not a weak sandbox.
	IsolationUnknown Isolation = iota
	// IsolationNone runs directly on the host. Development only; it is never
	// selected by a profile that asks for a boundary.
	IsolationNone
	// IsolationContainer shares the host kernel with namespace and cgroup
	// isolation (podman/docker).
	IsolationContainer
	// IsolationSyscallFiltered shares the host kernel but intercepts syscalls
	// (gVisor runsc, bwrap+seccomp+landlock).
	IsolationSyscallFiltered
	// IsolationMicroVM runs in a hypervisor-managed virtual machine with its
	// own kernel (BoxLite).
	IsolationMicroVM
	// IsolationMicroVMHosted is a micro-VM supplied by a hosted provider
	// (E2B). Equal strength to IsolationMicroVM, but the machine, its disk
	// and its network are somebody else's.
	IsolationMicroVMHosted
)

// String renders the stable name used in profiles, events and audit records.
// The numeric value is never part of the public vocabulary.
func (i Isolation) String() string {
	switch i {
	case IsolationNone:
		return "none"
	case IsolationContainer:
		return "container"
	case IsolationSyscallFiltered:
		return "syscall-filtered"
	case IsolationMicroVM:
		return "microvm"
	case IsolationMicroVMHosted:
		return "microvm-hosted"
	case IsolationUnknown:
		return "unknown"
	default:
		return fmt.Sprintf("isolation(%d)", uint8(i))
	}
}

// Rank orders isolation levels by strength. Hosted micro-VMs rank equal to
// local ones: the boundary is as strong, the trust boundary is elsewhere, and
// that difference is expressed by CapHostedTrust, not by a weaker rank.
func (i Isolation) Rank() int {
	switch i {
	case IsolationNone:
		return 0
	case IsolationContainer:
		return 1
	case IsolationSyscallFiltered:
		return 2
	case IsolationMicroVM, IsolationMicroVMHosted:
		return 3
	default:
		return -1
	}
}

// AtLeast reports whether i meets or exceeds want. Unknown never satisfies
// anything, so a provider that forgot to declare cannot pass a minimum check.
func (i Isolation) AtLeast(want Isolation) bool {
	return i.Rank() >= want.Rank() && want != IsolationUnknown
}

// ParseIsolation accepts the profile-facing spelling of a level. Hyphens,
// underscores and case are tolerated because profiles arrive from YAML and CLI
// flags as often as from Go code.
func ParseIsolation(s string) (Isolation, error) {
	key := strings.ToLower(strings.TrimSpace(s))
	key = strings.ReplaceAll(key, "_", "-")
	switch key {
	case "", "any", "auto":
		return IsolationUnknown, nil
	case "none", "host":
		return IsolationNone, nil
	case "container", "oci":
		return IsolationContainer, nil
	case "syscall-filtered", "gvisor", "runsc":
		return IsolationSyscallFiltered, nil
	case "microvm", "vm":
		return IsolationMicroVM, nil
	case "microvm-hosted", "hosted", "e2b":
		return IsolationMicroVMHosted, nil
	default:
		return IsolationUnknown, fmt.Errorf("unknown isolation level %q (want none, container, syscall-filtered, microvm or microvm-hosted)", s)
	}
}
