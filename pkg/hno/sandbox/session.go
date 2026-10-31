package sandbox

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// Session is a live, stateful environment bound to a caller-supplied id. The
// id is decided by the business object that owns the conversation, a workflow
// run, or a ticket — never randomly — because "resuming a suspended workflow"
// means "getting the same box back" (§14.4).
type Session struct {
	mu sync.Mutex

	sb    *Sandbox
	id    string
	env   Environment
	spec  SessionSpec
	state SessionState

	image     string
	isolation Isolation
	provider  string
	workspace *WorkspaceSource
	// ctx is the handle context captured at adoption, used for session-scoped
	// events when no call context exists yet.
	ctx context.Context

	// lineage identifies the state this environment carries, and inherited is the
	// highest recorded step whose effects are part of that state. Both exist so a
	// replay decision can be made from a fact about the box instead of a guess about
	// the step: see D-20.
	lineage   string
	inherited int
	// stateSource names how this environment got the state it has, for reports.
	stateSource string

	// approved holds the ids this session has a live grant for, which is how the
	// pipeline discharges an obligation instead of suspending the same request again.
	approved []string

	runs      int
	queued    int
	lastSeen  time.Time
	createdAt time.Time // absolute-lifetime anchor for Timeouts.SessionTTL

	lock       chan struct{} // capacity from Limits.SessionMaxConcurrentRuns
	snapshots  []SnapshotHandle
	drift      []string
	markedLost bool
	closed     bool
}

// ID is the session identifier the caller supplied.
func (s *Session) ID() string { return s.id }

// Status snapshots the session state for dashboards and readiness checks.
// StateSource reports how the live environment acquired its state.
func (s *Session) StateSource() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stateSource == "" {
		return "base image"
	}
	return s.stateSource
}

// Status returns the observable session state.
func (s *Session) Status() SessionStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	expires, _ := s.deadlineLocked(time.Now())
	return SessionStatus{
		ID:           s.id,
		State:        s.state,
		Provider:     s.provider,
		Isolation:    s.isolation,
		Image:        s.image,
		EnvID:        s.envIDLocked(),
		ActiveRuns:   s.runs,
		QueuedRuns:   s.queued,
		LastActiveAt: s.lastSeen,
		ExpiresAt:    expires,
		SpecSteps:    len(s.spec.Steps),
		Snapshots:    len(s.snapshots),
		Drifted:      len(s.drift) > 0,
	}
}

// envIDLocked returns the engine-side identifier; callers hold s.mu.
func (s *Session) envIDLocked() string {
	if s.env == nil {
		return ""
	}
	return s.env.ID()
}

// Exec runs one request inside this session. It is the stateful counterpart of
// Sandbox.Run: same policy chain, same failure classes, same artefact rules,
// plus the workspace and installed tools the previous rounds produced (§5.3).
func (s *Session) Exec(ctx context.Context, req Request, opts ...RunOption) (*Report, error) {
	ro := resolveRunOptions(opts, &s.sb.cfg)
	if err := s.reserve(ctx, ro.QueueIfBusy); err != nil {
		return nil, err
	}
	defer s.release()
	rep, err := s.sb.execute(ctx, s, req, ro)
	if err != nil {
		return rep, err
	}
	if rep != nil && rep.Lost {
		// The environment died mid-run; the session must not keep claiming to
		// be ready afterwards (§15).
		s.markLost(err)
	}
	return rep, nil
}

// RunProject executes a multi-phase project inside this session: one workspace,
// ordered phases, one artefact set (§13.2).
func (s *Session) RunProject(ctx context.Context, proj Project, opts ...RunOption) (*ProjectResult, error) {
	return s.sb.runProject(ctx, s, proj, resolveRunOptions(opts, &s.sb.cfg))
}

// Files returns the workspace file view of this session's environment. A
// provider without CapFileBridge gets a *CapabilityError rather than a host-side
// fallback (§5, D-05).
func (s *Session) Files() (FileService, error) {
	if err := s.checkAlive(ctx0()); err != nil {
		return nil, err
	}
	bridge, ok := s.sb.provider.(FileBridge)
	if !s.sb.caps.Has(CapFileBridge) || !ok {
		return nil, &CapabilityError{Provider: s.provider, Missing: CapFileBridge, Wanted: s.sb.caps}
	}
	return bridge.Files(context.Background(), s.env)
}

// Install records a toolchain change in the spec and applies it live. Recording
// is the point: an environment change that is not declared does not survive the
// next rebuild, which is the difference between "works until retry" and
// "reproducible" (§17.1, D-11).
func (s *Session) Install(ctx context.Context, pkg PackageSpec) error {
	step := Step{Kind: StepInstall, Tool: string(pkg.Tool), Args: pkg.requirementArgs(), Argv: pkg.installCommand(), Digest: pkg.Fingerprint()}
	if err := s.record(step); err != nil {
		return err
	}
	req := pkg.toRequest()
	if err := s.runStateful(ctx, req, "install "+pkg.String()); err != nil {
		return err
	}
	return nil
}

// runStateful executes a request whose promise is about state, not about a result:
// Install and Setup record steps that later rebuilds will faithfully re-run.
//
// These two cannot use the ordinary run contract, where a nonzero exit is the payload's
// own outcome reported in the Report with a nil error. That is right for "run my code
// and tell me what happened" and wrong for "make this dependency exist": a step that
// exited 7 did not make anything exist, yet it had been recorded as replayable state and
// returned nil, so the caller proceeded and every future rebuild re-ran a command known
// to fail. The exit code is the only evidence here, and the step must not be recorded
// unless it says the work happened.
func (s *Session) runStateful(ctx context.Context, req Request, what string) error {
	rep, err := s.Exec(ctx, req)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if rep == nil {
		return fmt.Errorf("%s: provider returned no report", what)
	}
	if rep.ExitCode != 0 {
		detail := strings.TrimSpace(rep.Stderr)
		if detail == "" {
			detail = strings.TrimSpace(rep.Stdout)
		}
		if detail == "" {
			detail = "no output captured"
		} else if len(detail) > failureDetailBytes {
			detail = "..." + detail[len(detail)-failureDetailBytes:]
		}
		s.dropLastStep()
		return &StepReplayError{
			Step:     s.lastRecordedStep(),
			Cause:    fmt.Errorf("%s exited %d: %s", what, rep.ExitCode, detail),
			Replayed: 0,
		}
	}
	return nil
}

// dropLastStep removes the step just recorded by the caller, because a step that failed
// is not replayable state: recording it means every rebuild faithfully re-runs a known
// failure and reports the resulting problem as the caller's own.
func (s *Session) dropLastStep() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.spec.Steps) == 0 {
		return
	}
	s.spec.Steps = s.spec.Steps[:len(s.spec.Steps)-1]
}

// lastRecordedStep returns the step most recently appended, for error reporting.
func (s *Session) lastRecordedStep() Step {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.spec.Steps) == 0 {
		return Step{}
	}
	return s.spec.Steps[len(s.spec.Steps)-1]
}

// Setup runs a declared script and records it as replayable state.
func (s *Session) Setup(ctx context.Context, script string) error {
	step := Step{Kind: StepSetup, Script: script, Digest: hashString(script)}
	if err := s.record(step); err != nil {
		return err
	}
	if err := s.runStateful(ctx, Shell(script), "setup step"); err != nil {
		return err
	}
	return nil
}

// RecordInput declares a workspace file so a rebuild materialises it again.
func (s *Session) RecordInput(ctx context.Context, in Input) error {
	src, err := s.replayableInput(in)
	if err != nil {
		return err
	}
	step := Step{Kind: StepMaterialize, Path: in.Path, Digest: inputDigest(in), Input: src}
	if err := s.record(step); err != nil {
		return err
	}
	// Recording is not the same as writing. The file has to be in this environment too,
	// or a session that only records never has its input.
	req := Request{Inputs: []Input{*src}}
	if len(src.Bytes) == 0 && src.HostPath == "" && src.Reader == nil {
		req = Request{Inputs: []Input{in}}
	}
	if err := s.runStateful(ctx, req, "record input "+in.Path); err != nil {
		s.dropLastStep()
		return err
	}
	return nil
}

// replayableInput keeps the part of an Input that a later rebuild can read again.
//
// A Reader is single use: recording it would produce a step whose content no rebuild can
// reproduce, which is the promise this used to break. Inline bytes are kept as they are;
// a host path is read now, so the recorded step holds the bytes that existed at record
// time rather than whatever the path happens to contain on the next attempt.
func (s *Session) replayableInput(in Input) (*Input, error) {
	if in.Path == "" {
		return nil, fmt.Errorf("record input requires a path")
	}
	if in.Reader != nil {
		return nil, &UnrecoverableInputError{Path: in.Path, Cause: "a Reader cannot be replayed: it has already been consumed by the time the session records it", Fix: "pass Bytes, or HostPath with AllowHostPaths enabled, so a rebuild can read the same content again"}
	}
	if in.Bytes != nil {
		out := in
		out.Reader = nil
		return &out, nil
	}
	if in.HostPath != "" {
		raw, err := os.ReadFile(in.HostPath)
		if err != nil {
			return nil, fmt.Errorf("sandbox: record input %q: cannot read %q for replay: %w", in.Path, in.HostPath, err)
		}
		mode := in.Mode
		return &Input{Path: in.Path, Bytes: raw, Mode: mode, Executable: in.Executable}, nil
	}
	return nil, fmt.Errorf("sandbox: record input %q has no content", in.Path)
}

// Phase runs one named phase, recording start/finish for downstream resumption.
func (s *Session) Phase(ctx context.Context, name string, req Request, opts ...RunOption) (*Report, error) {
	if name == "" {
		return nil, fmt.Errorf("phase requires a name")
	}
	s.sb.emit(ctx, Event{Kind: EventPhaseStarted, SessionID: s.id, Phase: name, Provider: s.provider, Isolation: s.isolation})
	rep, err := s.Exec(ctx, req, opts...)
	ev := Event{Kind: EventPhaseFinished, SessionID: s.id, Phase: name, Provider: s.provider, Isolation: s.isolation}
	if rep != nil {
		ev.RunID = rep.RunID
		ev.Exit = rep.ExitCode
		ev.Duration = rep.Duration
	}
	s.sb.emit(ctx, ev)
	return rep, err
}

// Checkpoint saves the environment state for a later rollback (§17, D-14).
func (s *Session) Checkpoint(ctx context.Context, name string) (SnapshotHandle, error) {
	if err := s.checkAlive(ctx); err != nil {
		return SnapshotHandle{}, err
	}
	snap, ok := s.sb.provider.(Snapshotter)
	if !s.sb.caps.Has(CapSnapshot) || !ok {
		return SnapshotHandle{}, &CapabilityError{Provider: s.provider, Missing: CapSnapshot, Wanted: s.sb.caps}
	}
	handle, err := snap.Snapshot(ctx, s.env, name)
	if err != nil {
		return SnapshotHandle{}, fmt.Errorf("sandbox: snapshot %q: %w", name, err)
	}
	if handle.SessionID == "" {
		handle.SessionID = s.id
	}
	// Record how far the spec had been satisfied at the moment this snapshot was taken.
	// A later Restore can then tell which steps the restored box really contains;
	// without the mark it must replay everything rather than guess.
	s.mu.Lock()
	lin, upto, src := s.lineage, len(s.spec.Steps)-1, s.stateSource
	if upto < 0 {
		// Nothing had been satisfied yet, so the snapshot captured an untouched box.
		lin = ""
	}
	s.mu.Unlock()
	s.sb.rememberSnapshotMark(handle, snapshotMark{lineage: lin, upto: upto, source: src})
	s.mu.Lock()
	s.snapshots = append(s.snapshots, handle)
	if keep := s.sb.cfg.Limits.SnapshotKeep; keep > 0 && len(s.snapshots) > keep {
		drop := s.snapshots[:len(s.snapshots)-keep]
		s.snapshots = s.snapshots[len(s.snapshots)-keep:]
		s.mu.Unlock()
		for _, old := range drop {
			if derr := snap.DeleteSnapshot(ctx, old); derr != nil {
				s.sb.emit(ctx, Event{Kind: EventHookFault, SessionID: s.id, Detail: map[string]any{"stage": "snapshot-retention", "error": derr.Error()}})
			}
		}
		s.mu.Lock()
	}
	s.mu.Unlock()
	s.sb.emit(ctx, Event{Kind: EventCheckpointed, SessionID: s.id, Provider: s.provider, Detail: map[string]any{"name": name, "bytes": handle.Bytes}})
	return handle, nil
}

// Restore rolls the environment back to a checkpoint (§16.4).
func (s *Session) Restore(ctx context.Context, handle SnapshotHandle) error {
	if err := s.checkAlive(ctx); err != nil {
		return err
	}
	snap, ok := s.sb.provider.(Snapshotter)
	if !s.sb.caps.Has(CapSnapshot) || !ok {
		return &CapabilityError{Provider: s.provider, Missing: CapSnapshot, Wanted: s.sb.caps}
	}
	if err := snap.Restore(ctx, s.env, handle); err != nil {
		return fmt.Errorf("sandbox: restore %q: %w", handle.Name, err)
	}
	s.mu.Lock()
	// A restored box holds the state the snapshot captured, so only steps recorded up
	// to the checkpoint can be assumed present. Without a recorded mark the safe
	// direction is to inherit nothing: an unknown watermark must never authorize a
	// skip.
	if mark, ok := s.sb.snapshotMarkFor(handle); ok {
		s.lineage = mark.lineage
		s.inherited = mark.upto
		s.stateSource = "snapshot:" + handle.Name
	} else {
		s.inherited = -1
		s.stateSource = "snapshot:" + handle.Name + " (no step mark; replaying all)"
	}
	s.mu.Unlock()
	s.sb.emit(ctx, Event{Kind: EventRestored, SessionID: s.id, Provider: s.provider, Detail: map[string]any{"name": handle.Name}})
	return nil
}

// CloneAs forks the session for a parallel attempt. On a CoW engine this costs
// near nothing; on an engine without CapClone it is a *CapabilityError, never a
// silent shared-directory write (§15).
func (s *Session) CloneAs(ctx context.Context, as string) (*Session, error) {
	if err := s.checkAlive(ctx); err != nil {
		return nil, err
	}
	clone, ok := s.sb.provider.(Cloner)
	if !s.sb.caps.Has(CapClone) || !ok {
		return nil, &CapabilityError{Provider: s.provider, Missing: CapClone, Wanted: s.sb.caps}
	}
	if as == "" {
		return nil, fmt.Errorf("sandbox: clone requires a session id")
	}
	env, err := clone.Clone(ctx, s.env, as)
	if err != nil {
		return nil, fmt.Errorf("sandbox: clone %q: %w", as, err)
	}
	s.mu.Lock()
	spec := s.spec.Clone()
	parentLin := s.lineage
	s.mu.Unlock()
	child := s.sb.adopt(as, env, spec)
	// A clone carries whatever the parent's box had, so every step the parent had
	// satisfied is present in the child. That is the condition under which a replay may
	// skip work, so the child takes the parent's state lineage and a watermark covering
	// the parent's recorded steps instead of starting from nothing (D-20).
	child.mu.Lock()
	child.lineage = parentLin
	child.inherited = len(spec.Steps) - 1
	child.stateSource = "clone:" + s.id
	child.mu.Unlock()
	s.sb.emit(ctx, Event{Kind: EventCloned, SessionID: s.id, RunID: as, Provider: s.provider})
	return child, nil
}

// Rebuild re-renders the recorded spec into a fresh environment (§17.2).
func (s *Session) Rebuild(ctx context.Context) (*RebuildReport, error) {
	if !s.sb.caps.Has(CapRebuild) {
		return nil, &CapabilityError{Provider: s.provider, Missing: CapRebuild, Wanted: s.sb.caps}
	}
	return s.sb.rebuild(ctx, s)
}

// Drift reports divergence between the live environment and its recorded spec.
// Detection is best-effort: it is an alert, not a guarantee (§17.3).
func (s *Session) Drift(ctx context.Context) (*DriftReport, error) {
	s.mu.Lock()
	items := append([]string(nil), s.drift...)
	s.mu.Unlock()
	return &DriftReport{SessionID: s.id, Items: items, DetectedAt: time.Now().UTC(), Strict: s.sb.cfg.StrictLockdown}, nil
}

// Release returns the session to the pool: the id and spec survive, the
// environment does not (§5.3).
func (s *Session) Release(ctx context.Context) error {
	s.mu.Lock()
	if s.state.Terminal() {
		s.mu.Unlock()
		return nil
	}
	s.state = StateReleasing
	env := s.env
	s.mu.Unlock()
	var err error
	if env != nil {
		err = env.Close(ctx)
	}
	s.mu.Lock()
	s.env = nil
	s.state = StateReleased
	s.closed = true
	s.mu.Unlock()
	s.sb.forget(s.id)
	// Releasing also drops the engine-side record: an environment nobody can
	// resume is residue, and "am I sure nothing was left behind" must have a
	// cheap answer (§15).
	if store, ok := s.sb.provider.(SessionStore); ok {
		if err := store.DeleteSession(ctx, s.id); err != nil {
			s.sb.reportFault(ctx, "cleanup", s.id, err)
		}
	}
	s.sb.emit(ctx, Event{Kind: EventCleaned, SessionID: s.id, Provider: s.provider})
	if err != nil {
		return fmt.Errorf("sandbox: release session %q: %w", s.id, err)
	}
	return nil
}

// Destroy removes the session, its spec and its checkpoints. It is the
// irreversible one, so it is separate from Release (§5.3).
func (s *Session) Destroy(ctx context.Context) error {
	if err := s.Release(ctx); err != nil {
		return err
	}
	if store := s.sb.cfg.Sessions; store != nil {
		if err := store.DeleteSession(ctx, s.id); err != nil {
			return fmt.Errorf("sandbox: destroy session record %q: %w", s.id, err)
		}
	}
	snap, ok := s.sb.provider.(Snapshotter)
	if ok {
		s.mu.Lock()
		handles := append([]SnapshotHandle(nil), s.snapshots...)
		s.snapshots = nil
		s.mu.Unlock()
		for _, h := range handles {
			if err := snap.DeleteSnapshot(ctx, h); err != nil {
				return fmt.Errorf("sandbox: delete snapshot %q: %w", h.Name, err)
			}
		}
	}
	return nil
}

// Reattach resumes output of a detached run and returns its terminal report
// once known (§15).
func (s *Session) Reattach(ctx context.Context, ref RunRef) (*Report, error) {
	att, ok := s.sb.provider.(Attacher)
	if !s.sb.caps.Has(CapBackground) || !ok {
		return nil, &CapabilityError{Provider: s.provider, Missing: CapBackground, Wanted: s.sb.caps}
	}
	state, rep, err := att.Status(ctx, ref)
	if err != nil {
		return nil, fmt.Errorf("sandbox: query run %s: %w", ref.ID, err)
	}
	switch state {
	case RunStateDone, RunStateFailed:
		return rep, nil
	case RunStateUncertain:
		return rep, &UncertainOutcomeError{RunID: ref.ID, SessionID: s.id, Effect: EffectExternal, Suggestion: "engine cannot confirm terminal state; do not retry automatically"}
	default:
		if err := att.Attach(ctx, ref, s.sb.newFanout(ref.ID)); err != nil {
			return nil, fmt.Errorf("sandbox: reattach run %s: %w", ref.ID, err)
		}
		state, rep, err = att.Status(ctx, ref)
		if err != nil {
			return nil, fmt.Errorf("sandbox: query run %s after attach: %w", ref.ID, err)
		}
		return rep, nil
	}
}

// Wait blocks until a detached run finishes or ctx ends (§5.3).
func (s *Session) Wait(ctx context.Context, ref RunRef, timeout time.Duration) (*Report, error) {
	deadline := time.Now().Add(timeout)
	if timeout <= 0 {
		deadline = time.Now().Add(s.sb.cfg.Timeouts.Exec)
	}
	att, ok := s.sb.provider.(Attacher)
	if !ok || !s.sb.caps.Has(CapBackground) {
		return nil, &CapabilityError{Provider: s.provider, Missing: CapBackground, Wanted: s.sb.caps}
	}
	for {
		state, rep, err := att.Status(ctx, ref)
		if err != nil {
			return nil, fmt.Errorf("sandbox: wait on run %s: %w", ref.ID, err)
		}
		switch state {
		case RunStateDone, RunStateFailed:
			return rep, nil
		case RunStateUncertain:
			return rep, &UncertainOutcomeError{RunID: ref.ID, SessionID: s.id, Effect: EffectExternal, Suggestion: "outcome cannot be confirmed"}
		}
		if time.Now().After(deadline) {
			return nil, &ReadinessError{RunID: ref.ID, Probe: ReadyProbe{Deadline: timeout}, LastSeen: state.String()}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// deadlineLocked reports when this session stops being usable and which limit decided
// it. Both halves of the documented keep-alive are honoured here: SessionTTL is an
// absolute lifetime measured from creation, and Idle is measured from the last
// activity. A zero duration disables that half instead of expiring instantly, because
// "unset" and "expire immediately" are different claims.
//
// Before this, Timeouts.Idle had no consumer anywhere and SessionTTL only produced an
// ExpiresAt that nothing acted on, so the promised "expire then reclaim" never
// happened: an untouched session kept its environment for the life of the process
// (D-09, §197).
func (s *Session) deadlineLocked(now time.Time) (time.Time, string) {
	var best time.Time
	cause := ""
	if ttl := s.sb.cfg.Timeouts.SessionTTL; ttl > 0 {
		if d := s.createdAt.Add(ttl); best.IsZero() || d.Before(best) {
			best, cause = d, "session-ttl"
		}
	}
	if idle := s.sb.cfg.Timeouts.Idle; idle > 0 {
		if d := s.lastSeen.Add(idle); best.IsZero() || d.Before(best) {
			best, cause = d, "idle"
		}
	}
	return best, cause
}

// expiredAtLocked reports whether the keep-alive has run out at now.
func (s *Session) expiredAtLocked(now time.Time) bool {
	deadline, _ := s.deadlineLocked(now)
	return !deadline.IsZero() && !now.Before(deadline)
}

// ExpireNow releases the session as though its keep-alive had run out. It exists for
// an operator who wants the host resource back immediately, and for tests, which must
// not sleep to prove a timeout works.
func (s *Session) ExpireNow(ctx context.Context) error {
	cause := s.CauseOfExpiry()
	return s.expire(ctx, cause)
}

// CauseOfExpiry names the limit that fired or would fire: "session-ttl", "idle", or
// "keep-alive" when neither is configured.
func (s *Session) CauseOfExpiry() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, cause := s.deadlineLocked(time.Now())
	if cause == "" {
		return "keep-alive"
	}
	return cause
}

// expire releases the environment and moves the session to StateReleased. The recorded
// spec survives: an expired session is not a corrupted one, and the same id may be
// provisioned again, which is what makes it safe to hand control to the TTL at all.
func (s *Session) expire(ctx context.Context, cause string) error {
	s.mu.Lock()
	if s.state == StateReleased || s.state == StateCorrupted {
		s.mu.Unlock()
		return nil
	}
	env := s.env
	s.env = nil
	s.state = StateReleased
	s.mu.Unlock()

	if env != nil {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.sb.cfg.Timeouts.Cleanup)
		err := env.Close(cctx)
		cancel()
		if err != nil {
			s.sb.emit(ctx, Event{Kind: EventHookFault, SessionID: s.id, Provider: s.provider,
				Detail: map[string]any{"stage": "expire-cleanup", "error": err.Error()}})
		}
	}
	s.sb.emit(ctx, Event{Kind: EventSessionExpired, SessionID: s.id, Provider: s.provider,
		Detail: map[string]any{"cause": cause}})
	return nil
}

// checkAlive is the gate for operations that reach the environment without going
// through reserve. Expiry is enforced rather than merely reported: a session that says
// it is usable after its keep-alive ran out is the same silent-no-op class as a config
// key nobody reads.
func (s *Session) checkAlive(ctx context.Context) error {
	s.mu.Lock()
	expired := s.expiredAtLocked(time.Now())
	state := s.state
	s.mu.Unlock()
	if !expired {
		return nil
	}
	_ = s.expire(ctx, s.CauseOfExpiry())
	return expiredStateError(s.id, state)
}

// expiredStateError is the single wording for "the keep-alive ran out", so an operator
// sees the same remedy wherever the gate fires.
func expiredStateError(id string, from SessionState) *StateError {
	return &StateError{
		SessionID: id,
		State:     from,
		Action:    "use session",
		Allowed:   []SessionState{StateReady, StateBusy},
		Reason:    "keep-alive ran out; the environment was released and the recorded spec was kept",
		Fix:       "call Session again with WithCreateIfMissing to re-provision from the recorded spec, or WithTimeouts to raise Idle/SessionTTL",
	}
}

// reserve takes the session lock, honouring the configured lock mode (§15).
func (s *Session) reserve(ctx context.Context, queue bool) error {
	s.mu.Lock()
	// Expiry is decided under the same lock that admits work, so a run cannot slip in
	// against an environment that is being released.
	if s.expiredAtLocked(time.Now()) {
		state := s.state
		s.mu.Unlock()
		s.sb.expireApprovals(time.Now())
		_ = s.expire(ctx, s.CauseOfExpiry())
		return expiredStateError(s.id, state)
	}
	switch {
	case s.closed:
		s.mu.Unlock()
		return &StateError{SessionID: s.id, State: s.state, Action: "exec"}
	case s.state == StateLost, s.state == StateSuspended:
		// Resumable() promises that a run may re-provision the environment from the
		// recorded spec, and ensureEnvironment implements exactly that. Refusing here
		// anyway made the promise undeliverable: the rebuild branch had no caller at all,
		// so a session that lost its box could never run again on a handle configured with
		// the default RebuildFromSpec. Admit the run only when the rebuild can actually
		// happen; otherwise the refusal stays, and says why.
		if s.sb.canRebuild() {
			break
		}
		s.mu.Unlock()
		return &StaleSessionError{
			SessionID: s.id, Provider: s.provider,
			Cause: fmt.Errorf("auto-rebuild needs RetryMode %s and capability %s; current mode is %q",
				RebuildFromSpec, CapRebuild, s.sb.cfg.RetryMode),
		}
	case s.state.Terminal():
		s.mu.Unlock()
		return &StateError{SessionID: s.id, State: s.state, Action: "exec"}
	}
	s.queued++
	s.mu.Unlock()

	if queue {
		select {
		case s.lock <- struct{}{}:
		case <-ctx.Done():
			s.mu.Lock()
			s.queued--
			s.mu.Unlock()
			return ctx.Err()
		}
	} else {
		select {
		case s.lock <- struct{}{}:
		default:
			s.mu.Lock()
			s.queued--
			s.mu.Unlock()
			return &StateError{SessionID: s.id, State: StateBusy, Action: "exec (session busy; use WithQueueIfBusy or CloneAs)"}
		}
	}
	s.mu.Lock()
	s.queued--
	s.runs++
	if s.state == StateReady {
		s.state = StateBusy
	}
	s.lastSeen = time.Now()
	s.mu.Unlock()
	return nil
}

// release gives back the session lock.
func (s *Session) release() {
	<-s.lock
	s.mu.Lock()
	s.runs--
	if !s.closed && s.state == StateBusy {
		s.state = StateReady
	}
	s.lastSeen = time.Now()
	s.mu.Unlock()
}

// record appends a replayable step and persists the session record.
func (s *Session) record(step Step) error {
	s.mu.Lock()
	next := s.spec.Clone()
	if err := next.Add(step); err != nil {
		s.mu.Unlock()
		return err
	}
	s.spec = next
	s.mu.Unlock()
	return s.sb.persistSession(s)
}

// markLost moves the session into StateLost after a mid-run environment death.
func (s *Session) markLost(cause error) {
	s.mu.Lock()
	if !s.closed {
		s.state = StateLost
	}
	s.mu.Unlock()
	s.sb.emit(context.Background(), Event{Kind: EventCleaned, SessionID: s.id, Provider: s.provider, Detail: map[string]any{"lost": fmt.Sprint(cause)}})
}

// noteDrift records an observed divergence for Drift() reporting.
func (s *Session) noteDrift(item string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.drift = append(s.drift, item)
}

// Occupy marks the session busy without starting a run. It exists for tooling
// and tests that need deterministic contention instead of a race (§6.4).
func (s *Session) Occupy() bool {
	select {
	case s.lock <- struct{}{}:
		s.mu.Lock()
		s.state = StateBusy
		s.mu.Unlock()
		return true
	default:
		return false
	}
}

// Vacate undoes an Occupy. Calling it without a matching Occupy is harmless.
func (s *Session) Vacate() {
	select {
	case <-s.lock:
		s.mu.Lock()
		if s.state == StateBusy {
			s.state = StateReady
		}
		s.mu.Unlock()
	default:
	}
}

// MarkLost simulates engine loss (crash, eviction, TTL reclaim) so callers can
// exercise the rebuild path without killing a real VM (§15). It is an ops and
// test hook, not a normal control-flow tool.
func (s *Session) MarkLost(cause error) {
	s.mu.Lock()
	s.state = StateLost
	s.env = nil
	s.markedLost = true
	s.mu.Unlock()
	s.sb.emit(s.ctxIfSet(), Event{Kind: EventRebuildWillDropFiles, SessionID: s.id, Detail: map[string]any{"cause": errString(cause)}})
}

// ctxIfSet returns the captured context or a fresh background one.
func (s *Session) ctxIfSet() context.Context {
	if s.ctx != nil {
		return s.ctx
	}
	return context.Background()
}

// workspaceLocked returns the session's declared working tree; callers hold
// s.mu. A session keeps one workspace across phases, which is what makes
// "install once, reuse all phases" possible (§13.2).
func (s *Session) workspaceLocked() *WorkspaceSource { return s.workspace }

// Workspace reports the session's declared working tree.
func (s *Session) Workspace() *WorkspaceSource {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneSource(s.workspace)
}

// SnapshotReport describes retained checkpoints.
type SnapshotReport struct {
	SessionID string
	Handles   []SnapshotHandle
	Orphans   []string
}

// Snapshots lists retained checkpoints of this session.
func (s *Session) Snapshots() []SnapshotHandle {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]SnapshotHandle(nil), s.snapshots...)
}

// Spec returns a copy of the recorded replayable specification.
func (s *Session) Spec() SessionSpec {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.spec.Clone()
}

// RebuildReport is the outcome of rendering the spec into a new environment
// (§17.2).
type RebuildReport struct {
	SessionID string
	Replayed  []StepResult
	Cached    int
	// StateSource says what the new environment inherited, which is the only thing
	// that can make a cached step legitimate: "base image" replays everything,
	// "clone:<id>" and "snapshot:<name>" carry the state earlier steps produced. A
	// caller comparing rebuild costs needs this, because Cached without it is
	// unexplainable.
	StateSource string
	Dropped     []string
	Drift       []string
	Duration    time.Duration
}

// HasDrift reports whether the rebuilt environment may differ from what the
// caller had before it died.
func (r *RebuildReport) HasDrift() bool { return r != nil && len(r.Drift) > 0 }

// StepResult records how one spec step resolved during replay.
type StepResult struct {
	Step    Step
	Cached  bool
	Err     error
	Elapsed time.Duration
}

// DriftReport is the divergence observation of a live environment (§17.3).
type DriftReport struct {
	SessionID  string
	Items      []string
	DetectedAt time.Time
	Strict     bool
}

// Empty reports whether nothing diverged.
func (d *DriftReport) Empty() bool { return d == nil || len(d.Items) == 0 }

// sortedItems returns drift items in a stable order for reports.
func (d *DriftReport) sortedItems() []string {
	out := append([]string(nil), d.Items...)
	sort.Strings(out)
	return out
}

// ctx0 is the context for gates that have none of their own.
func ctx0() context.Context { return context.Background() }
