package sandbox

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// FailureClass is the first decision of every error path: classify first, then
// decide who retries, what gets retried, and from which state. Untyped errors
// invite "retry three times regardless of cause", which hides bugs, repeats
// side effects and burns compute (§16.1).
type FailureClass uint8

const (
	// ClassUnknown is the default for errors the core cannot classify. It is
	// never auto-retried: unknown means a human looks at it.
	ClassUnknown FailureClass = iota
	// ClassUserCode is the payload failing on its own terms (traceback,
	// assertion, command not found). Never re-run automatically: the fix is
	// new code, which belongs to the model.
	ClassUserCode
	// ClassDenied is a policy or approval refusal. Re-running the same request
	// produces the same refusal.
	ClassDenied
	// ClassPermanent is a configuration, capability or version problem. No
	// amount of retrying helps; it needs an install or a profile change.
	ClassPermanent
	// ClassTransient is engine control-plane noise: RPC reset, REST 5xx,
	// image pull network failure, boot race.
	ClassTransient
	// ClassResource is quota or capacity pressure. Handled by queued retry with
	// its own budget, which is backpressure, not fast retry.
	ClassResource
	// ClassTimeout means the deadline fired. The outcome is *unknown*: the
	// payload may have finished, half-finished, or committed side effects.
	ClassTimeout
	// ClassLost means the environment died mid-run (OOM, engine crash, host
	// restart). Recovery needs a rebuild or a checkpoint, not a blind retry.
	ClassLost
)

// String names the class for logs and metrics.
func (c FailureClass) String() string {
	switch c {
	case ClassUserCode:
		return "user_code"
	case ClassDenied:
		return "denied"
	case ClassPermanent:
		return "permanent"
	case ClassTransient:
		return "transient"
	case ClassResource:
		return "resource"
	case ClassTimeout:
		return "timeout"
	case ClassLost:
		return "lost"
	default:
		return "unknown"
	}
}

// RetryableByClass reports whether the class may ever be repeated without new
// information. It is a property of the class, not of the caller's mood:
// user-code failures and denials are structurally un-retryable (§16.1).
func (c FailureClass) RetryableByClass() bool {
	switch c {
	case ClassTransient, ClassResource:
		return true
	default:
		return false
	}
}

// Action is what the caller should do next. It exists so "retryable" is never
// a bare boolean: the useful answer to a model is *how* to proceed (§16.6).
type Action uint8

const (
	// ActionUnset is the zero value.
	ActionUnset Action = iota
	// ActionModelFix sends stderr back to the model as new information.
	ActionModelFix
	// ActionAwaitBackoff waits for the retry budget to allow another attempt.
	ActionAwaitBackoff
	// ActionRequestEgressApproval asks for a host to be allowlisted.
	ActionRequestEgressApproval
	// ActionRequestApproval asks a human to approve the request as declared.
	ActionRequestApproval
	// ActionReinstall points at an environment fix command.
	ActionReinstall
	// ActionRestoreCheckpoint rolls back to a checkpoint before retrying.
	ActionRestoreCheckpoint
	// ActionRebuild re-renders the recorded spec before retrying.
	ActionRebuild
	// ActionUseInstallAPI tells the model the sanctioned way to change the
	// environment instead of raw shell (D-16).
	ActionUseInstallAPI
	// ActionAskHuman escalates because the outcome is uncertain or unknown.
	ActionAskHuman
	// ActionGiveUp stops: the retry budget or breaker said no.
	ActionGiveUp
	// ActionContinue proceeds; the failure was FailOpen.
	ActionContinue
)

// String names the action for tool output consumed by models.
func (a Action) String() string {
	switch a {
	case ActionModelFix:
		return "model_fix"
	case ActionAwaitBackoff:
		return "await_backoff"
	case ActionRequestEgressApproval:
		return "request_egress_approval"
	case ActionRequestApproval:
		return "request_approval"
	case ActionReinstall:
		return "reinstall"
	case ActionRestoreCheckpoint:
		return "restore_checkpoint"
	case ActionRebuild:
		return "rebuild"
	case ActionUseInstallAPI:
		return "use_install_api"
	case ActionAskHuman:
		return "ask_human"
	case ActionGiveUp:
		return "give_up"
	case ActionContinue:
		return "continue"
	default:
		return "unset"
	}
}

// BudgetState reports what is left, so a caller can distinguish "try again now"
// from "the breaker is open, stop".
type BudgetState struct {
	AttemptsLeft  int
	BackoffAfter  time.Duration
	WallClockLeft time.Duration
	BreakerOpen   bool
}

// Failure is the machine-readable answer to "what just happened". It travels
// next to the Go error, never instead of it: swallowing the error is the bug
// this type exists to kill (§16.6, toolkit.go today).
type Failure struct {
	Class        FailureClass
	Retryable    bool
	NextAction   Action
	Hint         string
	Fixes        []string
	BlockedHosts []string
	Attempt      int
	Budget       BudgetState
	// Cause is the underlying typed error.
	Cause error
}

// Error implements error so a Failure can be returned directly when there is no
// richer typed error to attach.
func (f *Failure) Error() string {
	if f == nil {
		return "<nil failure>"
	}
	msg := "sandbox failure"
	if f.Cause != nil {
		msg = f.Cause.Error()
	}
	if f.Hint != "" {
		return fmt.Sprintf("%s [%s]: %s", msg, f.Class, f.Hint)
	}
	return fmt.Sprintf("%s [%s]", msg, f.Class)
}

// Unwrap exposes the cause to errors.Is/errors.As.
func (f *Failure) Unwrap() error {
	if f == nil {
		return nil
	}
	return f.Cause
}

// classifier is implemented by every typed error in this package, which is how
// ClassOf stays open for provider-specific errors without a registry.
type classifier interface {
	Class() FailureClass
}

// ClassOf extracts the failure class of an error, defaulting to ClassUnknown.
// It is the hook that plugs into the framework's existing retry predicate
// (graph.RetryConfig.ShouldRetry) instead of a second backoff engine (§16.2).
func ClassOf(err error) FailureClass {
	var c classifier
	if errors.As(err, &c) {
		return c.Class()
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return ClassTimeout
	case errors.Is(err, context.Canceled):
		return ClassDenied
	}
	return ClassUnknown
}

// AsFailure converts any error in this package into a Failure, filling class
// defaults so no call site has to re-derive them.
func AsFailure(err error) *Failure {
	if err == nil {
		return nil
	}
	var f *Failure
	if errors.As(err, &f) && f.Class != ClassUnknown {
		out := *f
		out.Cause = err
		return &out
	}
	class := ClassOf(err)
	f = &Failure{Class: class, Cause: err, Retryable: class.RetryableByClass()}
	switch class {
	case ClassUserCode:
		f.NextAction = ActionModelFix
	case ClassDenied:
		f.NextAction = ActionRequestApproval
	case ClassPermanent:
		f.NextAction = ActionReinstall
		f.Fixes = remediation(err)
	case ClassTransient:
		f.NextAction = ActionAwaitBackoff
	case ClassResource:
		f.NextAction = ActionAwaitBackoff
		f.Fixes = remediation(err)
	case ClassTimeout:
		f.NextAction = ActionAskHuman
		f.Retryable = false // outcome unknown until effect class and status query resolve it (§16.3)
	case ClassLost:
		f.NextAction = ActionRebuild
		f.Retryable = false // rebuild is a decision, not an automatic repeat (§16.4)
		if blocked := blockedHosts(err); len(blocked) > 0 {
			f.BlockedHosts = blocked
			f.NextAction = ActionRequestEgressApproval
		}
	default:
		f.NextAction = ActionAskHuman
	}
	var e effectCarrer
	if errors.As(err, &e) {
		f.Retryable = f.Retryable && e.effectForRetry()
	}
	return f
}

// effectCarrer lets an error advertise the effect class it was produced under.
// EffectExternal disables automatic retry even for transient classes (§16.3).
type effectCarrer interface {
	effectForRetry() bool
}

// remediation extracts copy-pasteable fixes from typed errors.
func remediation(err error) []string {
	var fixes fixer
	if errors.As(err, &fixes) {
		return fixes.Remediation()
	}
	return nil
}

// fixer is implemented by errors that know how to repair themselves.
type fixer interface {
	Remediation() []string
}

func blockedHosts(err error) []string {
	var bh blockedHostReporter
	if errors.As(err, &bh) {
		return bh.Blocked()
	}
	return nil
}

type blockedHostReporter interface {
	Blocked() []string
}

// --- typed errors -------------------------------------------------------

// Attempt records one provider's failure during resolution, so an unavailable
// runtime explains itself instead of printing one line (D-05).
type Attempt struct {
	Provider  string
	Isolation Isolation
	Formation Formation
	Reason    string
	Fix       string
	Fixes     []string
}

// UnavailableError means no provider could satisfy the ask. Fixes are
// commands, not apologies.
type UnavailableError struct {
	Tried  []Attempt
	Want   Isolation
	Reason string
	Fixes  []string
}

func (e *UnavailableError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "no sandbox provider satisfies isolation %s", e.Want)
	if e.Reason != "" {
		fmt.Fprintf(&b, ": %s", e.Reason)
	}
	for _, a := range e.Tried {
		fmt.Fprintf(&b, "\n  - %s: %s", a.Provider, a.Reason)
		if a.Fix != "" {
			fmt.Fprintf(&b, " (fix: %s)", a.Fix)
		}
	}
	return b.String()
}

// Class marks provider unavailability as permanent: it is an install problem.
func (e *UnavailableError) Class() FailureClass { return ClassPermanent }

// Remediation lists every fix the error carries: the handle-level ones first,
// then each attempt's. A caller that gives up with "no provider" and no command
// is the reason this type exists (§5.3).
func (e *UnavailableError) Remediation() []string {
	var out []string
	out = append(out, e.Fixes...)
	for _, a := range e.Tried {
		if a.Fix != "" {
			out = append(out, a.Fix)
		}
		out = append(out, a.Fixes...)
	}
	return dedupeStrings(out)
}

// dedupeStrings preserves order while removing repeats.
func dedupeStrings(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

// CapabilityError names the missing capability instead of degrading silently
// (D-05). A provider that cannot bridge files must say so, not touch the host
// disk.
type CapabilityError struct {
	Provider string
	Missing  Capability
	Wanted   CapabilitySet
}

func (e *CapabilityError) Error() string {
	return fmt.Sprintf("provider %q lacks capability %q (has: %s)", e.Provider, e.Missing, strings.Join(e.Wanted.Strings(), ","))
}

// Class marks capability gaps as permanent.
func (e *CapabilityError) Class() FailureClass { return ClassPermanent }

// IsolationError reports a level mismatch between ask and achievement.
type IsolationError struct {
	Provider string
	Want     Isolation
	Got      Isolation
}

func (e *IsolationError) Error() string {
	return fmt.Sprintf("provider %q offers isolation %s, need at least %s", e.Provider, e.Got, e.Want)
}

// Class marks isolation shortfalls as permanent for this provider.
func (e *IsolationError) Class() FailureClass { return ClassPermanent }

// QuotaError reports admission rejection with the numbers attached.
type QuotaError struct {
	Scope   string // tenant, provider, host
	Limit   int64
	Active  int64
	WaitFor time.Duration
}

func (e *QuotaError) Error() string {
	return fmt.Sprintf("sandbox quota %s exhausted: %d/%d in use", e.Scope, e.Active, e.Limit)
}

// Class marks quota pressure as a resource failure.
func (e *QuotaError) Class() FailureClass { return ClassResource }

// HostWatermarkError reports the host refusing more load (§15).
type HostWatermarkError struct {
	Resource string // memory, disk, pid
	Free     int64
	Want     int64
}

func (e *HostWatermarkError) Error() string {
	return fmt.Sprintf("host cannot provide sandbox: %s has %d bytes free, need %d", e.Resource, e.Free, e.Want)
}

// Class marks host pressure as a resource failure.
func (e *HostWatermarkError) Class() FailureClass { return ClassResource }

// ProvisionError covers image pull, disk creation and boot failures, with the
// fix command inline (D-05).
type ProvisionError struct {
	Provider string
	Stage    string
	Cause    error
	Fix      string
}

func (e *ProvisionError) Error() string {
	msg := fmt.Sprintf("provider %q failed while %s", e.Provider, e.Stage)
	if e.Cause != nil {
		msg += ": " + e.Cause.Error()
	}
	return msg
}

// Unwrap exposes the underlying engine error.
func (e *ProvisionError) Unwrap() error { return e.Cause }

// Class treats provisioning as transient: retry costs boot time, but a fresh
// attempt usually works; persistent failure surfaces through the breaker.
func (e *ProvisionError) Class() FailureClass { return ClassTransient }

// Remediation returns the doctor command when present.
func (e *ProvisionError) Remediation() []string {
	if e.Fix == "" {
		return nil
	}
	return []string{e.Fix}
}

// DeniedError is a policy veto, carrying which policy said no and why.
type DeniedError struct {
	Policy string
	Reason string
	Rule   string
}

func (e *DeniedError) Error() string {
	return fmt.Sprintf("denied by policy %q: %s", e.Policy, e.Reason)
}

// Class marks denials as un-retryable.
func (e *DeniedError) Class() FailureClass { return ClassDenied }

// ApprovalPendingError suspends execution until a human answers; it is a
// distinct type so a suspended workflow is never mistaken for a failure.
type ApprovalPendingError struct {
	ApprovalID string
	Reason     string
	Requested  string
}

func (e *ApprovalPendingError) Error() string {
	return fmt.Sprintf("approval %s pending for %q: %s", e.ApprovalID, e.Requested, e.Reason)
}

// Class marks approval waits as denied-but-resumable.
func (e *ApprovalPendingError) Class() FailureClass { return ClassDenied }

// HookFaultError is a broken extension, not a payload failure. It is never
// swallowed and never retried: a panicking policy is a bug in the application.
type HookFaultError struct {
	Kind    string // policy, observer, sink
	Name    string
	Detail  string
	PanicAt string
}

func (e *HookFaultError) Error() string {
	return fmt.Sprintf("%s hook %q faulted: %s", e.Kind, e.Name, e.Detail)
}

// Class marks hook faults as permanent.
func (e *HookFaultError) Class() FailureClass { return ClassPermanent }

// PathError is a confinement violation; "we would have escaped the root" is
// said out loud instead of cleaned up silently.
type PathError struct {
	Path   string
	Root   string
	Reason string
}

func (e *PathError) Error() string {
	return fmt.Sprintf("path %q rejected for sandbox root %q: %s", e.Path, e.Root, e.Reason)
}

// Class marks confinement violations as permanent.
func (e *PathError) Class() FailureClass { return ClassPermanent }

// StaleSessionError means the session id is known but its environment is gone
// (§6.2). The caller decides between rebuild and give up.
type StaleSessionError struct {
	SessionID string
	Provider  string
	Cause     error
}

func (e *StaleSessionError) Error() string {
	msg := fmt.Sprintf("session %q no longer has a live environment", e.SessionID)
	if e.Cause != nil {
		msg += ": " + e.Cause.Error()
	}
	return msg
}

// Unwrap exposes the underlying cause.
func (e *StaleSessionError) Unwrap() error { return e.Cause }

// Class marks stale sessions as lost environments.
func (e *StaleSessionError) Class() FailureClass { return ClassLost }

// LostError is the mid-run death of an environment (§15).
type LostError struct {
	RunID     string
	SessionID string
	Provider  string
	Cause     error
	Collected []string // artefacts already persisted and still readable
}

func (e *LostError) Error() string {
	msg := fmt.Sprintf("sandbox environment lost during run %s (session %s, provider %s)", e.RunID, e.SessionID, e.Provider)
	if e.Cause != nil {
		msg += ": " + e.Cause.Error()
	}
	return msg
}

// Unwrap exposes the underlying cause.
func (e *LostError) Unwrap() error { return e.Cause }

// Class marks it as lost.
func (e *LostError) Class() FailureClass { return ClassLost }

// UncertainOutcomeError is the honest answer when a run was submitted, the
// connection died, and the engine cannot report terminal state. Automatic retry
// is forbidden: we do not know whether the side effect happened (§16.3).
type UncertainOutcomeError struct {
	RunID      string
	SessionID  string
	Effect     EffectClass
	Suggestion string
}

func (e *UncertainOutcomeError) Error() string {
	return fmt.Sprintf("outcome of run %s is unknown and effect is %s: %s", e.RunID, e.Effect, e.Suggestion)
}

// Class marks uncertain outcomes as lost-environment class.
func (e *UncertainOutcomeError) Class() FailureClass { return ClassLost }

// effectForRetry refuses automatic retry for anything with external effect.
func (e *UncertainOutcomeError) effectForRetry() bool { return e.Effect == EffectPure }

// RetryBudgetError means the storm guard said stop (§16.5).
type RetryBudgetError struct {
	Scope    string // run, session, idempotency key
	Attempts int
	Window   time.Duration
}

func (e *RetryBudgetError) Error() string {
	return fmt.Sprintf("retry budget exhausted for %s: %d attempts within %s", e.Scope, e.Attempts, e.Window)
}

// Class marks budget exhaustion as a resource failure.
func (e *RetryBudgetError) Class() FailureClass { return ClassResource }

// IdempotencyConflictError protects the journal: the same key with different
// payload is a bug, not a retry.
type IdempotencyConflictError struct {
	Key         string
	OriginalRun string
	PayloadHash string
}

func (e *IdempotencyConflictError) Error() string {
	return fmt.Sprintf("idempotency key %q was already used by run %s with a different payload (%s)", e.Key, e.OriginalRun, e.PayloadHash)
}

// Class marks conflicts as permanent.
func (e *IdempotencyConflictError) Class() FailureClass { return ClassPermanent }

// ArtifactError covers required-but-missing and oversize artefacts (D-17).
type ArtifactError struct {
	Spec   ArtifactSpec
	Reason string // missing, too-large, unreadable
	Found  int64
	Limit  int64
}

func (e *ArtifactError) Error() string {
	switch e.Reason {
	case "too-large":
		return fmt.Sprintf("artefact %q is %d bytes over the %d byte limit", e.Spec.Path, e.Found, e.Limit)
	case "unreadable":
		return fmt.Sprintf("artefact %q cannot be read: %s", e.Spec.Path, e.Reason)
	default:
		return fmt.Sprintf("required artefact %q was not produced", e.Spec.Path)
	}
}

// Class marks artefact failures as permanent: the work has to change, not the
// retry count.
func (e *ArtifactError) Class() FailureClass { return ClassPermanent }

// ReadinessError means a background service never became usable (§15).
type ReadinessError struct {
	RunID    string
	Probe    ReadyProbe
	LastSeen string
}

func (e *ReadinessError) Error() string {
	return fmt.Sprintf("run %s never became ready: probe=%+v last=%q", e.RunID, e.Probe, e.LastSeen)
}

// Class marks readiness failure as user code: the service is the payload.
func (e *ReadinessError) Class() FailureClass { return ClassUserCode }

// VersionSkewError refuses to start when guest and SDK disagree (§15).
type VersionSkewError struct {
	Provider string
	Guest    string
	SDK      string
	Wanted   string
}

func (e *VersionSkewError) Error() string {
	return fmt.Sprintf("provider %q version skew: guest %s, sdk %s, expected %s", e.Provider, e.Guest, e.SDK, e.Wanted)
}

// Class marks skew as permanent until something is reinstalled.
func (e *VersionSkewError) Class() FailureClass { return ClassPermanent }

// CleanupError reports resources that survived Close (§15). It is reported, not
// hidden, because a leaked VM looks exactly like "the sandbox is slow".
type CleanupError struct {
	Provider  string
	Residuals []string
	SweeperID string
}

func (e *CleanupError) Error() string {
	return fmt.Sprintf("provider %q left %d resource(s) behind (%s): %s", e.Provider, len(e.Residuals), e.SweeperID, strings.Join(e.Residuals, ", "))
}

// Class marks cleanup residue as permanent until swept.
func (e *CleanupError) Class() FailureClass { return ClassPermanent }

// StateError reports an action illegal for the current lifecycle state (§6.2).
type StateError struct {
	SessionID string
	State     SessionState
	Action    string
	Allowed   []SessionState
}

func (e *StateError) Error() string {
	return fmt.Sprintf("session %q in state %s cannot %s (valid: %s)", e.SessionID, e.State, e.Action, joinStates(e.Allowed))
}

// Class marks illegal transitions as permanent: the caller must change state
// first, which is a code fix.
func (e *StateError) Class() FailureClass { return ClassPermanent }

// DriftError warns that the live environment diverged from its recorded spec,
// so a rebuild may behave differently (§17.3). It is an error type so it can be
// returned, but it never blocks by default.
type DriftError struct {
	SessionID string
	Items     []string
	Strict    bool
}

func (e *DriftError) Error() string {
	return fmt.Sprintf("session %q drifted from its spec in %d place(s): %s", e.SessionID, len(e.Items), strings.Join(e.Items, ", "))
}

// Class marks drift as permanent when strict, otherwise it is not an error.
func (e *DriftError) Class() FailureClass { return ClassPermanent }

// StepReplayError names the spec step that failed to re-render (§17.2).
type StepReplayError struct {
	Step     Step
	Cause    error
	Replayed int
	Cached   int
}

func (e *StepReplayError) Error() string {
	return fmt.Sprintf("replaying spec step %s (%s) failed after %d replayed / %d cached: %v", e.Step.Kind, e.Step.Key(), e.Replayed, e.Cached, e.Cause)
}

// Unwrap exposes the cause.
func (e *StepReplayError) Unwrap() error { return e.Cause }

// Class marks replay failure as provisioning-shaped: transient, because a
// rebuild usually succeeds, and the breaker catches persistent skew.
func (e *StepReplayError) Class() FailureClass { return ClassTransient }

// joinStates renders allowed states for StateError.
func joinStates(states []SessionState) string {
	if len(states) == 0 {
		// An empty allow-list still has to render, or the error reads as a
		// truncated sentence exactly when the caller needs it most.
		return "none"
	}
	names := make([]string, 0, len(states))
	for _, s := range states {
		names = append(names, s.String())
	}
	return strings.Join(names, ", ")
}
