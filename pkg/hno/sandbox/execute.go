package sandbox

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"time"
)

// execute is the single path every run takes, whether it came from Run,
// Session.Exec or a graph node. One path means one set of rules: validate,
// policy, budget, prepare, run, collect, audit (§6.3, §6.4).
func (sb *Sandbox) execute(ctx context.Context, sess *Session, req Request, ro resolvedRunOptions) (*Report, error) {
	if sess != nil && req.ID == "" {
		req.ID = newRunID()
	}
	rc := sb.buildRunContext(sess, req, ro)
	if err := req.Validate(); err != nil {
		return nil, err
	}

	// 0. Budget shape: reject before provisioning or approval UI spend
	// anything on a request the core cannot honour (§13.1).
	if err := sb.checkShape(rc.Request); err != nil {
		return nil, err
	}
	if err := sb.checkCapabilities(rc.Request); err != nil {
		return nil, err
	}

	// 1. Policies first: nothing is provisioned before the veto pass, so a
	// denied request costs no boot (§6.3).
	tightened, err := sb.runPolicies(ctx, rc)
	if err != nil {
		rep := deniedReport(sb, rc, err, time.Now())
		sb.recordAudit(ctx, rc, rep, err)
		return rep, err
	}
	if tightened != nil {
		if err := (*tightened).Validate(); err != nil {
			return nil, fmt.Errorf("policy produced an invalid request: %w", err)
		}
		rc.Request = tightened
	}

	// 1b. Lockdown: environment changes must go through the declared spec
	// (§17.3). Checked after policies so a policy veto wins, and before any
	// provisioning so a refusal costs nothing.
	if err := sb.checkLockdown(sess, rc.Request); err != nil {
		rep := deniedReport(sb, rc, err, time.Now())
		sb.recordAudit(ctx, rc, rep, err)
		return rep, err
	}

	// 2. Idempotency gate: a replayed key returns the recorded outcome instead
	// of running twice (§16.3).
	if hit, entry, err := sb.journalGate(ctx, rc); err != nil {
		return nil, err
	} else if hit {
		rep := &Report{RunID: entry.RunID, SessionID: rc.SessionID, Republished: true, RepublishedRunID: entry.RunID,
			ExitCode: 0, Isolation: sb.iso, Provider: sb.name, Duration: 0}
		return rep, nil
	}

	// 3. Storm guard and breaker: both reject before spending (§16.5).
	if err := sb.guardAttempt(rc); err != nil {
		return nil, err
	}

	// 4. Environment. A session that lost its box either rebuilds or reports
	// stale; it never silently provisions a third state (§15).
	env, err := sb.environmentFor(ctx, sess, rc)
	if err != nil {
		return nil, err
	}
	if sess != nil {
		sess.sb = sb
	}

	// 5. Stream fan-out for this run, including sinks bound per-run.
	fan := sb.newFanout(rc.RunID, rc.Sinks...)

	// 5b. Stage declared inputs. Doing this here — inside the one pipeline, before
	// any engine runs a payload — is what makes "the files I declared are the files
	// that were there" true for every provider instead of being each engine's own
	// best effort (§13.1).
	if serr := sb.stageInputs(ctx, env, rc.Request); serr != nil {
		// Staging failures take the same exit as a denial: a populated report, a
		// typed error, and the outcome committed, so nothing that declared inputs
		// can fail silently half-materialised.
		rep := deniedReport(sb, rc, serr, time.Now())
		sb.commitOutcome(ctx, rc, rep, serr)
		sb.recordAudit(ctx, rc, rep, serr)
		return rep, serr
	}

	// 6. Run. Events bracket the execution so a dashboard can see a phase even
	// when it fails (§6.3).
	sb.emit(ctx, Event{Kind: EventPhaseStarted, RunID: rc.RunID, SessionID: rc.SessionID, Detail: map[string]any{"kind": rc.Kind.String(), "runtime": rc.Request.Runtime}})
	defer sb.emit(ctx, Event{Kind: EventPhaseFinished, RunID: rc.RunID, SessionID: rc.SessionID})
	runSpec := RunSpec{
		RunID:      rc.RunID,
		SessionID:  rc.SessionID,
		Request:    *rc.Request,
		Workspace:  rc.Request.Workspace,
		MountMode:  rc.Request.Mode,
		Budget:     rc.Budget,
		Limits:     rc.Limits,
		Streaming:  fan.enabled() && sb.caps.Has(CapStreaming),
		Background: rc.Request.Background,
	}
	start := time.Now()
	var repCleanup *CleanupError
	rep, runErr := env.Exec(ctx, runSpec, fan)
	if sess == nil {
		// A one-shot shape must never leave an idle environment behind (§6.4).
		defer func() {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sb.cfg.Timeouts.Cleanup)
			defer cancel()
			if cerr := env.Close(cleanupCtx); cerr != nil {
				sb.reportFault(cleanupCtx, "cleanup", env.ID(), cerr)
				repCleanup = &CleanupError{Provider: sb.name, Residuals: []string{env.ID()}, SweeperID: "run"}
			}
			sb.emit(cleanupCtx, Event{Kind: EventCleaned, RunID: rc.RunID, SessionID: rc.SessionID, Provider: sb.name})
		}()
	}
	rep = sb.normalizeReport(rep, runErr, rc, start)
	sb.accountStreaming(rep, fan)

	// 7. Artefacts are collected even when a phase failed, so "the build broke
	// but the test report is the point" is expressible (§13.3).
	if collectErr := sb.collect(ctx, rc, env, rep); collectErr != nil && runErr == nil {
		rep.Failure = AsFailure(collectErr)
		runErr = collectErr
	}

	// 8. Journal commit, breaker accounting, audit and events.
	sb.commitOutcome(ctx, rc, rep, runErr)
	sb.recordAudit(ctx, rc, rep, runErr)
	fan.close()
	if repCleanup != nil && runErr == nil {
		runErr = repCleanup
	}
	return rep, runErr
}

// buildRunContext assembles the immutable hook payload for one execution.
func (sb *Sandbox) buildRunContext(sess *Session, req Request, ro resolvedRunOptions) *RunContext {
	runID := req.ID
	if runID == "" {
		runID = newRunID()
	}
	kind := RunKindOnce
	if req.Background {
		kind = RunKindBackground
	}
	if sess != nil {
		kind = RunKindPhase
	}
	rc := &RunContext{
		RunID:    runID,
		Kind:     kind,
		Caller:   ro.Caller,
		Request:  &req,
		Want:     sb.cfg.MinIsolation,
		Provider: sb.name,
		Budget:   sb.resolveBudget(req, ro),
		Limits:   ro.Limits,
		Sinks:    ro.Sinks,
		Attrs:    mergeMaps(sb.cfg.Attrs, ro.Attrs),
	}
	if sess != nil {
		rc.SessionID = sess.id
	}
	return rc
}

// resolveBudget merges request, run option and profile budgets.
func (sb *Sandbox) resolveBudget(req Request, ro resolvedRunOptions) Budget {
	b := Budget{
		SetupTimeout:   firstPositive(req.Budget.SetupTimeout, ro.Budget.SetupTimeout, sb.cfg.Timeouts.Setup),
		ExecTimeout:    firstPositive(req.Budget.ExecTimeout, req.Timeout, ro.Budget.ExecTimeout, sb.cfg.Timeouts.Exec),
		OutputLimit:    firstPositive64(req.Budget.OutputLimit, ro.Budget.OutputLimit, sb.cfg.Limits.OutputMaxBytes),
		WorkspaceLimit: firstPositive64(req.Budget.WorkspaceLimit, ro.Budget.WorkspaceLimit, sb.cfg.Limits.WorkspaceMaxBytes),
		CostLimit:      req.Budget.CostLimit,
	}
	return b
}

// checkCapabilities refuses a request that needs a feature the bound provider
// does not declare. Silently ignoring an unsupported field is how a sandbox
// ends up "running" without the isolation the caller thought they asked for
// (D-05).
func (sb *Sandbox) checkCapabilities(req *Request) error {
	if req == nil {
		return nil
	}
	have := sb.caps
	if len(req.Expose) > 0 && !have.Has(CapPortForward) {
		return &CapabilityError{Provider: sb.name, Missing: CapPortForward, Wanted: have}
	}
	if req.Background && !have.Has(CapBackground) {
		return &CapabilityError{Provider: sb.name, Missing: CapBackground, Wanted: have}
	}
	if len(req.Outputs) > 0 && !have.Has(CapFileBridge) {
		return &CapabilityError{Provider: sb.name, Missing: CapFileBridge, Wanted: have}
	}
	if req.Workspace != nil && req.Workspace.Kind == SourceDir && !have.Has(CapFileBridge) {
		return &CapabilityError{Provider: sb.name, Missing: CapFileBridge, Wanted: have}
	}
	if req.Egress.Mode == EgressAllowlist && !have.Has(CapEgressAllowlist) {
		return &CapabilityError{Provider: sb.name, Missing: CapEgressAllowlist, Wanted: have}
	}
	if len(req.Egress.SecretRefs) > 0 && !have.Has(CapSecretSubstitution) {
		return &CapabilityError{Provider: sb.name, Missing: CapSecretSubstitution, Wanted: have}
	}
	return nil
}

// checkShape rejects payloads the core cannot express within Limits. It runs
// before any hook so a refusal is cheap and never reaches an engine.
func (sb *Sandbox) checkShape(req *Request) error {
	if req == nil {
		return errors.New("sandbox: nil request")
	}
	lim := sb.cfg.Limits
	if int64(len(req.Code)) > lim.OutputMaxBytes {
		return &ArtifactError{Spec: ArtifactSpec{Path: "inline code", MaxBytes: lim.OutputMaxBytes}, Reason: "too-large", Found: int64(len(req.Code)), Limit: lim.OutputMaxBytes}
	}
	for p, file := range req.Files {
		if int64(len(file.Content)) > lim.InlineFileMaxBytes {
			return fmt.Errorf("sandbox: inline file %q is %d bytes, over the %d byte inline limit; use Inputs or Workspace (§13.4)", p, len(file.Content), lim.InlineFileMaxBytes)
		}
	}
	if len(req.Env) > lim.EnvMaxEntries {
		return fmt.Errorf("sandbox: %d env entries exceed the limit %d", len(req.Env), lim.EnvMaxEntries)
	}
	if int64(len(req.Inputs)) > int64(lim.EnvMaxEntries)*1024 {
		return fmt.Errorf("sandbox: %d inputs exceed the handle's fan-out bound", len(req.Inputs))
	}
	return nil
}

// runPolicies executes the chain and returns a request to use in place of the
// caller's, when a policy tightened it.
func (sb *Sandbox) runPolicies(ctx context.Context, rc *RunContext) (*Request, error) {
	var current *Request
	// The baseline is the caller's own request: any hook that widens privilege
	// — through Tighten or by mutating rc.Request — is caught here, not trusted
	// to be well behaved (§6.3).
	baseline := privilegeHash(rc.Request)
	for i, p := range sb.cfg.Policies {
		name := policyName(i, p)
		target := rc.Request
		if current != nil {
			target = current
		}
		probe := *rc
		probe.Request = target
		dec, err := callPolicy(ctx, p, &probe)
		if err != nil {
			// A broken policy fails closed: we cannot let an unchecked request
			// through because the guard crashed (§6.3, §7).
			sb.emit(ctx, Event{Kind: EventHookFault, RunID: rc.RunID, SessionID: rc.SessionID, Detail: map[string]any{"hook": name, "kind": "policy", "error": err.Error()}})
			return nil, &HookFaultError{Kind: "policy", Name: name, Detail: err.Error()}
		}
		after := privilegeHash(rc.Request)
		if current != nil {
			after = privilegeHash(current)
		}
		if after > baseline {
			return nil, &DeniedError{Policy: name, Reason: "a policy may only tighten a request, never widen it", Rule: "policy.tighten-only"}
		}
		switch dec.Action {
		case ActDeny:
			rule := dec.Rule
			if rule == "" {
				rule = dec.Reason
			}
			return nil, &DeniedError{Policy: name, Reason: dec.Reason, Rule: rule}
		case ActRequireApproval:
			approval := dec.Approval
			if approval == nil {
				approval = &Approval{ID: rc.RunID, Reason: dec.Reason}
			}
			if approval.ID == "" {
				approval.ID = rc.RunID
			}
			return nil, &ApprovalPendingError{ApprovalID: approval.ID, Reason: dec.Reason, Requested: describeRequest(rc.Request)}
		case ActAllow:
			if dec.Tighten != nil {
				updated := rc.Request.clone()
				if target != nil {
					updated = target.clone()
				}
				before := privilegeHash(&updated)
				dec.Tighten(&updated)
				if after := privilegeHash(&updated); after > before {
					return nil, &DeniedError{Policy: policyLabel(name, dec.Policy), Reason: "policy loosened the request; tightening only", Rule: "policy.tighten-only"}
				}
				current = &updated
			}
		default:
			return nil, &DeniedError{Policy: policyLabel(name, dec.Policy), Reason: "policy returned an undecided action", Rule: "policy.action"}
		}
	}
	return current, nil
}

// callPolicy invokes one policy with panic recovery.
func callPolicy(ctx context.Context, p Policy, rc *RunContext) (dec Decision, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v\n%s", r, debug.Stack())
		}
	}()
	return p.Decide(ctx, rc)
}

// journalGate consults the idempotency journal.
func (sb *Sandbox) journalGate(ctx context.Context, rc *RunContext) (bool, *JournalEntry, error) {
	key := rc.Request.IdempotencyKey
	if key == "" {
		return false, nil, nil
	}
	if sb.cfg.Journal == nil {
		return false, nil, fmt.Errorf("%w (key %q)", ErrJournalMissing, key)
	}
	entry, found, err := sb.cfg.Journal.Load(ctx, key)
	if err != nil {
		return false, nil, fmt.Errorf("sandbox: load idempotency key %q: %w", key, err)
	}
	if !found {
		if err := sb.saveJournal(ctx, key, rc, RunStateSubmitted, ""); err != nil {
			return false, nil, err
		}
		return false, nil, nil
	}
	switch entry.State {
	case RunStateDone:
		if entry.PayloadHash != "" && entry.PayloadHash != payloadHash(rc.Request) {
			return false, nil, &IdempotencyConflictError{Key: key, OriginalRun: entry.RunID, PayloadHash: entry.PayloadHash}
		}
		return true, &entry, nil
	case RunStateUncertain:
		return false, nil, &UncertainOutcomeError{RunID: entry.RunID, SessionID: entry.SessionID, Effect: rc.Request.Effect,
			Suggestion: "journal holds an unconfirmed submission; confirm with the engine before retrying"}
	case RunStateSubmitted:
		// A live submission with the same key is a conflict, not a retry.
		return false, nil, &IdempotencyConflictError{Key: key, OriginalRun: entry.RunID, PayloadHash: payloadHash(rc.Request)}
	default:
		return false, nil, sb.saveJournal(ctx, key, rc, RunStateSubmitted, "")
	}
}

// saveJournal writes one journal record.
func (sb *Sandbox) saveJournal(ctx context.Context, key string, rc *RunContext, state RunState, resultRef string) error {
	if sb.cfg.Journal == nil || key == "" {
		return nil
	}
	entry := JournalEntry{Key: key, RunID: rc.RunID, SessionID: rc.SessionID, State: state, PayloadHash: payloadHash(rc.Request), ResultRef: resultRef, UpdatedAt: time.Now().UTC()}
	if err := sb.cfg.Journal.Save(ctx, entry); err != nil {
		return fmt.Errorf("sandbox: save idempotency key %q: %w", key, err)
	}
	return nil
}

// guardAttempt applies the breaker and the storm guard.
func (sb *Sandbox) guardAttempt(rc *RunContext) error {
	if sb.breaker != nil && !sb.breaker.Allow() {
		sb.emit(context.Background(), Event{Kind: EventQuotaBlocked, RunID: rc.RunID, Detail: map[string]any{"reason": ErrBreakerOpen.Error()}})
		return ErrBreakerOpen
	}
	if sb.storm != nil {
		if err := sb.storm.allow(rc.Request.IdempotencyKey, rc.SessionID); err != nil {
			sb.emit(context.Background(), Event{Kind: EventQuotaBlocked, RunID: rc.RunID, SessionID: rc.SessionID, Detail: map[string]any{"reason": err.Error()}})
			return err
		}
	}
	return nil
}

// environmentFor resolves the execution substrate for a run.
func (sb *Sandbox) environmentFor(ctx context.Context, sess *Session, rc *RunContext) (Environment, error) {
	if sess == nil {
		env, err := sb.newEnvironment(ctx, "", SessionSpec{Base: sb.cfg.Image, MountMode: rc.Request.Mode})
		if err != nil {
			return nil, err
		}
		return closerOnce{Environment: env, sb: sb}, nil
	}
	sess.mu.Lock()
	state, env := sess.state, sess.env
	sess.mu.Unlock()
	switch {
	case env != nil && state.AcceptsExec():
		return env, nil
	case state.Resumable() && sb.cfg.RetryMode == RebuildFromSpec && sb.caps.Has(CapRebuild):
		if _, err := sb.rebuild(ctx, sess); err != nil {
			return nil, err
		}
		sess.mu.Lock()
		out := sess.env
		sess.mu.Unlock()
		if out == nil {
			return nil, &StaleSessionError{SessionID: sess.id, Provider: sb.name, Cause: errors.New("rebuild produced no environment")}
		}
		return out, nil
	case state.Resumable():
		return nil, &StaleSessionError{SessionID: sess.id, Provider: sb.name, Cause: errors.New("environment is gone; rebuild requires CapRebuild and RetryMode rebuild-from-spec")}
	case state.Terminal():
		return nil, &StateError{SessionID: sess.id, State: state, Action: "exec"}
	default:
		return nil, &StateError{SessionID: sess.id, State: state, Action: "exec", Allowed: []SessionState{StateReady, StateBusy}}
	}
}

// closerOnce adapts a one-shot environment so its resources are returned after
// a single run (§6.4: one-shot shape must not leak).
type closerOnce struct {
	Environment
	sb *Sandbox
}

// Close releases the ephemeral environment.
func (c closerOnce) Close(ctx context.Context) error { return c.Environment.Close(ctx) }

// normalizeReport fills the fields a provider may leave empty, so a report is
// always safe to log (§5 Run contract).
func (sb *Sandbox) normalizeReport(rep *Report, err error, rc *RunContext, start time.Time) *Report {
	if rep == nil {
		rep = &Report{ExitCode: -1}
	}
	rep.RunID = firstNonEmpty(rep.RunID, rc.RunID)
	rep.SessionID = firstNonEmpty(rep.SessionID, rc.SessionID)
	rep.Provider = firstNonEmpty(rep.Provider, sb.name)
	if rep.Isolation == IsolationUnknown {
		rep.Isolation = sb.iso
	}
	if rep.Duration == 0 {
		rep.Duration = time.Since(start)
	}
	if err != nil {
		switch {
		case errors.Is(err, context.DeadlineExceeded):
			rep.TimedOut = true
		case errors.Is(err, context.Canceled):
			rep.Canceled = true
		}
		var lost *LostError
		if errors.As(err, &lost) {
			rep.Lost = true
		}
	}
	if rep.Failure == nil && rep.Failed() {
		rep.Failure = classifyReport(sb, rep, err)
	} else if rep.Failure != nil && rep.Failure.Class == ClassUserCode {
		// A provider may state that user code failed — it owns that fact. It must
		// not own the wording: providers each composed "exit code 1", which
		// overrode the core's describeFailure and left the model with an exit
		// number instead of the compiler message it needs to fix anything (§16.4).
		// The class stays as the provider set it; the cause is the core's.
		rep.Failure.Cause = errors.New(describeFailure(rep))
	}
	sb.lastReport.set(rep)
	return rep
}

// classifyReport derives a Failure for a run that produced a bad report without
// a Go error, which is exactly the case the old toolkit turned into nil (§16.6).
func classifyReport(sb *Sandbox, rep *Report, err error) *Failure {
	if err != nil {
		f := AsFailure(err)
		f.Attempt = 1
		return f
	}
	switch {
	case rep.Lost:
		return &Failure{Class: ClassLost, NextAction: ActionRebuild, Cause: &LostError{RunID: rep.RunID, SessionID: rep.SessionID, Provider: rep.Provider}}
	case rep.TimedOut:
		return &Failure{Class: ClassTimeout, NextAction: ActionAskHuman, Hint: "outcome unknown: the run may have committed effects"}
	case rep.Canceled:
		return &Failure{Class: ClassDenied, NextAction: ActionGiveUp, Cause: context.Canceled}
	default:
		return &Failure{Class: ClassUserCode, NextAction: ActionModelFix, Cause: errors.New(describeFailure(rep))}
	}
}

// deniedReport builds the report returned alongside a refusal, so audit still
// sees identity, isolation and duration even for a run that never happened.
func deniedReport(sb *Sandbox, rc *RunContext, err error, started time.Time) *Report {
	return &Report{
		RunID:     rc.RunID,
		SessionID: rc.SessionID,
		ExitCode:  -1,
		Provider:  sb.name,
		Isolation: sb.iso,
		Duration:  time.Since(started),
		Failure:   AsFailure(err),
	}
}

// commitOutcome updates journal state and the breaker after a run.
func (sb *Sandbox) commitOutcome(ctx context.Context, rc *RunContext, rep *Report, err error) {
	if err != nil {
		class := ClassOf(err)
		if sb.breaker != nil && sb.cfg.BreakerPolicy.counts(class) {
			sb.breaker.Failure()
		}
		if rc.Request.IdempotencyKey != "" {
			state := RunStateFailed
			if class == ClassLost {
				state = RunStateUncertain
			}
			_ = sb.saveJournal(ctx, rc.Request.IdempotencyKey, rc, state, "")
		}
		return
	}
	if sb.breaker != nil {
		sb.breaker.Success()
	}
	if rc.Request.IdempotencyKey != "" {
		ref := ""
		if rep != nil {
			ref = rep.RunID
		}
		_ = sb.saveJournal(ctx, rc.Request.IdempotencyKey, rc, RunStateDone, ref)
	}
}

// payloadHash identifies a request's content for idempotency conflict checks.
func payloadHash(req *Request) string {
	if req == nil {
		return ""
	}
	h := shaHex(req.Runtime + "\x00" + req.Code + "\x00" + joinArg(req.Argv))
	if req.Workspace != nil {
		h += "-" + req.Workspace.Key()
	}
	return h
}

// joinArg flattens argv without importing strings in the hash path.
func joinArg(argv []string) string {
	out := ""
	for _, a := range argv {
		out += a + "\x00"
	}
	return out
}

// mergeMaps copies two attribute maps into a new one, right side winning.
func mergeMaps(a, b map[string]any) map[string]any {
	out := make(map[string]any, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

// firstNonEmpty returns the first non-empty string.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// firstPositive returns the first positive duration.
func firstPositive(vals ...time.Duration) time.Duration {
	for _, v := range vals {
		if v > 0 {
			return v
		}
	}
	return 0
}

// firstPositive64 returns the first positive size.
func firstPositive64(vals ...int64) int64 {
	for _, v := range vals {
		if v > 0 {
			return v
		}
	}
	return 0
}

// policyLabel prefers a policy's own name over the generated one.
func policyLabel(generated, explicit string) string {
	if explicit != "" {
		return explicit
	}
	return generated
}

// accountStreaming records what the provider actually delivered live, and turns a
// false streaming claim into an observable contract error.
//
// The capability bit is the provider's own assertion about itself, and the last
// engine audit found that assertion unreliable in both directions. So the core
// measures instead of trusting: if the payload produced output on a stream and no
// chunk for that stream ever arrived while the run was alive, the claim was false.
//
// It does not fail the run. The payload's result is real, and destroying a good run
// because the provider's telemetry was dishonest would make the bug harder to see,
// not easier. The caller learns it three ways: the populated counts, the typed error
// from Report.StreamingContract, and an Observer event.
func (sb *Sandbox) accountStreaming(rep *Report, fan *fanout) {
	if rep == nil {
		return
	}
	sent, _ := fan.deliveredBytes()
	var total int64
	for _, n := range sent {
		total += n
	}
	rep.OutputSent = total
	if !sb.caps.Has(CapStreaming) {
		// No claim was made, so receiving a whole buffer at exit is not a violation.
		return
	}
	var silent []string
	if len(rep.Stdout) > 0 && sent[StreamStdout] == 0 {
		silent = append(silent, StreamStdout.String())
	}
	if len(rep.Stderr) > 0 && sent[StreamStderr] == 0 {
		silent = append(silent, StreamStderr.String())
	}
	if len(silent) == 0 {
		return
	}
	violation := &StreamingContractError{
		Provider:      sb.name,
		Streams:       silent,
		DeclaredBytes: int64(len(rep.Stdout) + len(rep.Stderr)),
	}
	// The violation is deliberately kept out of Report.Failure. Failure answers
	// "why did the payload not produce its result", and a provider whose telemetry
	// lied has not changed what the code did: the run still exits 0, the artefacts
	// are still real. Turning an honest payload into a failed report was tried here
	// and immediately broke a passing project test — that is the same category error
	// as stamping a nonzero exit as success, only in the other direction.
	//
	// The claim is falsifiable through Report.StreamingContract() (data the caller
	// can check without configuring anything) and through this event (for hosts that
	// do observe).
	sb.reportFault(context.Background(), "streaming", sb.name, violation)
}
