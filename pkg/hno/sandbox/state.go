package sandbox

import (
	"time"
)

// SessionState is the lifecycle position of a session (§6.2). The core owns the
// transitions; a provider only reports the facts that cause them.
type SessionState uint8

const (
	// StateUnknown is the zero value.
	StateUnknown SessionState = iota
	// StateNew means the id is reserved and nothing is provisioned yet.
	StateNew
	// StateProvisioning means image pull or disk creation is in flight.
	StateProvisioning
	// StateBooting means the environment exists but is not yet ready.
	StateBooting
	// StateReady means the session accepts work.
	StateReady
	// StateBusy means a run holds the session lock.
	StateBusy
	// StateSuspended means the environment is parked to reclaim resources.
	StateSuspended
	// StateLost means the underlying environment died; the id and recorded
	// spec may still be usable for a rebuild (§15).
	StateLost
	// StateReleasing means cleanup is running. Terminal for new work.
	StateReleasing
	// StateReleased means the session is gone.
	StateReleased
	// StateCorrupted means the recorded spec could not be replayed; human
	// input is required, not automatic retry (§6.2).
	StateCorrupted
)

var sessionStateNames = map[SessionState]string{
	StateUnknown:      "unknown",
	StateNew:          "new",
	StateProvisioning: "provisioning",
	StateBooting:      "booting",
	StateReady:        "ready",
	StateBusy:         "busy",
	StateSuspended:    "suspended",
	StateLost:         "lost",
	StateReleasing:    "releasing",
	StateReleased:     "released",
	StateCorrupted:    "corrupted",
}

// String names the state for Status, events and StateError messages.
func (s SessionState) String() string { return sessionStateNames[s] }

// AcceptsExec reports whether work may be submitted in this state.
func (s SessionState) AcceptsExec() bool {
	return s == StateReady || s == StateBusy
}

// Resumable reports whether Exec may trigger a rebuild from the recorded spec.
func (s SessionState) Resumable() bool {
	return s == StateLost || s == StateSuspended
}

// Terminal reports whether the session can never serve work again.
func (s SessionState) Terminal() bool {
	return s == StateReleased || s == StateCorrupted
}

// SessionStatus is the observable status of a session.
type SessionStatus struct {
	ID           string
	State        SessionState
	Provider     string
	Isolation    Isolation
	Image        string
	EnvID        string
	ActiveRuns   int
	QueuedRuns   int
	InflightReqs int
	LastActiveAt time.Time
	ExpiresAt    time.Time
	// SpecSteps counts recorded spec steps so a caller can see whether the
	// environment is reproducible at all (§17.1).
	SpecSteps int
	// Snapshots counts retained checkpoints.
	Snapshots int
	// Drifted reports divergence from the recorded spec; detection is
	// best-effort, never a safety boundary (§17.3).
	Drifted bool
}
