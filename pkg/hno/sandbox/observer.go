package sandbox

import (
	"context"
	"time"
)

// EventKind names one lifecycle fact. Observers react to these; they never
// steer execution (§7).
type EventKind uint8

const (
	// EventUnknown is the zero value.
	EventUnknown EventKind = iota
	EventProbing
	EventResolved
	EventProvisioning
	EventBooting
	EventReady
	EventStarted
	EventWorkspacePrepared
	EventPhaseStarted
	EventPhaseFinished
	EventOutput
	EventEgressDenied
	EventHeartbeat
	EventDetached
	EventReattached
	EventCollecting
	EventArtifactsCollected
	EventRebuildWillDropFiles
	EventRebuilt
	EventDriftDetected
	EventCheckpointed
	EventRestored
	EventCloned
	EventCleaned
	EventClosed
	EventRetryAttempt
	EventRetryGaveUp
	EventQuotaBlocked
	EventHookFault
)

var eventKindNames = map[EventKind]string{
	EventProbing:              "probing",
	EventResolved:             "resolved",
	EventProvisioning:         "provisioning",
	EventBooting:              "booting",
	EventReady:                "ready",
	EventStarted:              "started",
	EventWorkspacePrepared:    "workspace_prepared",
	EventPhaseStarted:         "phase_started",
	EventPhaseFinished:        "phase_finished",
	EventOutput:               "output",
	EventEgressDenied:         "egress_denied",
	EventHeartbeat:            "heartbeat",
	EventDetached:             "detached",
	EventReattached:           "reattached",
	EventCollecting:           "collecting",
	EventArtifactsCollected:   "artifacts_collected",
	EventRebuildWillDropFiles: "rebuild_will_drop_files",
	EventRebuilt:              "rebuilt",
	EventDriftDetected:        "drift_detected",
	EventCheckpointed:         "checkpointed",
	EventRestored:             "restored",
	EventCloned:               "cloned",
	EventCleaned:              "cleaned",
	EventClosed:               "closed",
	EventRetryAttempt:         "retry_attempt",
	EventRetryGaveUp:          "retry_gave_up",
	EventQuotaBlocked:         "quota_blocked",
	EventHookFault:            "hook_fault",
}

// String names the event for logs and NodeEvent bridging.
func (k EventKind) String() string {
	if n, ok := eventKindNames[k]; ok {
		return n
	}
	return "unknown"
}

// Event is one lifecycle observation. Detail carries the facts that matter to
// dashboards without forcing a schema change per new signal.
type Event struct {
	Kind      EventKind
	At        time.Time
	RunID     string
	SessionID string
	Phase     string
	Isolation Isolation
	Provider  string
	Exit      int
	Duration  time.Duration
	// Detail is a short human-readable payload; secrets never belong in it.
	Detail map[string]any
}

// Observer watches the lifecycle. A broken Observer is reported as a
// HookFaultError and execution continues: observation is not a safety
// boundary, steering is (§7).
type Observer interface {
	Observe(ctx context.Context, ev Event) error
}

// ObserverFunc adapts a function to Observer.
type ObserverFunc func(ctx context.Context, ev Event) error

// Observe implements Observer.
func (f ObserverFunc) Observe(ctx context.Context, ev Event) error { return f(ctx, ev) }
