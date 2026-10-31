package sandbox

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"time"
)

// rebuild re-renders the recorded spec into a fresh environment and points the
// session at it. It exists because the default retry mode is RebuildFromSpec:
// the only honest way to retry a stateful sandbox is to rebuild it from what
// was declared (§16.4, §17.2).
func (sb *Sandbox) rebuild(ctx context.Context, sess *Session) (*RebuildReport, error) {
	if !sb.caps.Has(CapRebuild) {
		return nil, &CapabilityError{Provider: sb.name, Missing: CapRebuild, Wanted: sb.caps}
	}
	started := time.Now()
	sess.mu.Lock()
	spec := sess.spec.Clone()
	workspace := cloneSource(sess.workspace)
	sess.mu.Unlock()

	// A previous environment is retired first: a rebuild never leaves two live
	// boxes under one id (§15).
	sess.mu.Lock()
	old := sess.env
	sess.env = nil
	sess.state = StateProvisioning
	sess.mu.Unlock()
	if old != nil {
		cctx, cancel := context.WithTimeout(context.Background(), sb.cfg.Timeouts.Cleanup)
		_ = old.Close(cctx)
		cancel()
	}

	env, err := sb.newEnvironment(ctx, sess.id, spec)
	if err != nil {
		sess.mu.Lock()
		sess.state = StateLost
		sess.mu.Unlock()
		return nil, err
	}
	sb.setSessionWorkspace(sess, workspace)

	rep := &RebuildReport{SessionID: sess.id, Duration: time.Since(started)}
	for _, step := range spec.Steps {
		res := StepResult{Step: step}
		if cached, ok := sb.stepCached(step); ok && cached {
			res.Cached = true
			rep.Cached++
			continue
		}
		stepStart := time.Now()
		if err := sb.replayStep(ctx, env, step); err != nil {
			res.Err = err
			rep.Replayed = append(rep.Replayed, res)
			_ = env.Close(ctx)
			return rep, &StepReplayError{Step: step, Cause: err, Replayed: len(rep.Replayed), Cached: rep.Cached}
		}
		res.Elapsed = time.Since(stepStart)
		rep.Replayed = append(rep.Replayed, res)
		sb.rememberStep(step)
	}
	if dr := sb.detectDrift(ctx, env, spec); len(dr) > 0 {
		rep.Drift = dr
		sess.mu.Lock()
		sess.drift = append(sess.drift, dr...)
		sess.mu.Unlock()
		sb.emit(ctx, Event{Kind: EventDriftDetected, SessionID: sess.id, Detail: map[string]any{"items": dr}})
	}
	sess.mu.Lock()
	sess.env = env
	sess.state = StateReady
	sess.lastSeen = time.Now()
	sess.mu.Unlock()
	rep.Duration = time.Since(started)
	sb.emit(ctx, Event{Kind: EventRebuilt, SessionID: sess.id, Detail: map[string]any{"replayed": len(rep.Replayed), "cached": rep.Cached, "dropped": rep.Dropped}})
	return rep, nil
}

// replayStep executes one recorded step against a live environment.
func (sb *Sandbox) replayStep(ctx context.Context, env Environment, step Step) error {
	var req Request
	switch step.Kind {
	case StepInstall:
		if len(step.Argv) > 0 {
			req = Request{Argv: append([]string(nil), step.Argv...)}
		} else {
			req = Request{Argv: append([]string{step.Tool}, step.Args...)}
		}
	case StepSetup:
		req = Shell(step.Script)
	case StepEnv:
		req = Request{Runtime: "shell", Code: fmt.Sprintf("printf %%s %q", step.Digest)}
	case StepMaterialize, StepWorkspaceDelta:
		if step.Source == nil {
			return fmt.Errorf("materialize step %q has no source", step.Path)
		}
		req = Request{Runtime: "shell", Code: "test -e " + step.Path, Workspace: step.Source}
	default:
		return fmt.Errorf("cannot replay step kind %s", step.Kind)
	}
	_, err := env.Exec(ctx, RunSpec{
		RunID:     newRunID(),
		SessionID: env.ID(),
		Request:   req,
		MountMode: sb.cfg.Limits.DefaultMountMode,
		Budget:    Budget{ExecTimeout: sb.cfg.Timeouts.Exec, SetupTimeout: sb.cfg.Timeouts.Setup, OutputLimit: sb.cfg.Limits.OutputMaxBytes},
		Limits:    sb.cfg.Limits,
	}, nil)
	return err
}

// stepCached consults the replay cache. Caching is step-level, so a changed
// source file does not invalidate an expensive install (§17.2).
func (sb *Sandbox) stepCached(step Step) (bool, bool) {
	if sb.cfg.StepCache == nil {
		return false, false
	}
	key := step.Key()
	_, ok, err := sb.cfg.StepCache.Load(context.Background(), key)
	if err != nil {
		sb.emit(context.Background(), Event{Kind: EventHookFault, Detail: map[string]any{"cache": "step", "error": err.Error()}})
		return false, false
	}
	return true, ok
}

// rememberStep records a replayed step as cached.
func (sb *Sandbox) rememberStep(step Step) {
	if sb.cfg.StepCache == nil {
		return
	}
	entry := JournalEntry{Key: step.Key(), RunID: "step", State: RunStateDone, PayloadHash: step.Digest}
	if err := sb.cfg.StepCache.Save(context.Background(), entry); err != nil {
		sb.emit(context.Background(), Event{Kind: EventHookFault, Detail: map[string]any{"cache": "step", "error": err.Error()}})
	}
}

// detectDrift compares the live environment against the spec's expectation. It
// is an alert, not a guarantee: only lockdown enforce makes rebuild equivalence
// provable (§17.3).
func (sb *Sandbox) detectDrift(ctx context.Context, env Environment, spec SessionSpec) []string {
	fs, err := sb.filesOf(ctx, env)
	if err != nil {
		return nil // no file bridge means drift cannot be observed, not that it is absent
	}
	var out []string
	for _, step := range spec.Steps {
		if step.Kind != StepMaterialize && step.Kind != StepWorkspaceDelta {
			continue
		}
		if _, err := fs.Stat(ctx, step.Path); err != nil {
			out = append(out, fmt.Sprintf("%s missing after rebuild", step.Path))
		}
	}
	return out
}

// setSessionWorkspace records the workspace of a session.
func (sb *Sandbox) setSessionWorkspace(sess *Session, src *WorkspaceSource) {
	sess.mu.Lock()
	sess.workspace = cloneSource(src)
	sess.mu.Unlock()
}

// runProject executes a declared project inside a session: workspace once,
// phases in order, artefacts collected even on partial failure (§13.2).
func (sb *Sandbox) runProject(ctx context.Context, sess *Session, proj Project, ro resolvedRunOptions) (*ProjectResult, error) {
	if err := proj.Validate(); err != nil {
		return nil, err
	}
	started := time.Now()
	result := &ProjectResult{RunID: proj.ID, SessionID: sess.id}
	if result.RunID == "" {
		result.RunID = "project-" + newRunID()
	}
	resume := proj.ResumeFrom != ""
	for _, phase := range proj.Phases {
		if resume {
			if phase.Name != proj.ResumeFrom {
				result.Phases = append(result.Phases, PhaseReport{Name: phase.Name, Status: ProjectAborted})
				continue
			}
			resume = false
		}
		req := phase.Run.clone()
		if len(proj.Env) > 0 {
			req.Env = append(append([]string(nil), proj.Env...), req.Env...)
		}
		if proj.Egress.Mode != EgressUnset {
			req.Egress = tightenEgress(proj.Egress, req.Egress)
		}
		if req.Timeout == 0 && phase.Timeout > 0 {
			req.Timeout = phase.Timeout
		}
		if req.Workspace == nil {
			req.Workspace = cloneSource(proj.From)
			req.Mode = resolveMount(proj.Mode, sb.cfg.Limits.DefaultMountMode)
		}
		rep, err := sb.execute(ctx, sess, req, ro)
		// A report that contradicts itself is worse than a missing one: an earlier
		// version stamped Status "succeeded" while ExitCode said 1, because a user
		// code failure is data rather than an error and the status was derived only
		// from the error. Status therefore comes from the report's own outcome.
		pr := PhaseReport{Name: phase.Name, Status: ProjectSucceeded}
		if rep != nil {
			pr.RunID = rep.RunID
			pr.ExitCode = rep.ExitCode
			pr.Duration = rep.Duration
			pr.Truncated = rep.Truncated
			pr.Stdout = rep.Stdout
			pr.Stderr = rep.Stderr
			if rep.Failed() {
				pr.Status = ProjectFailed
			}
		}
		if err != nil {
			pr.Err = err
			pr.Status = ProjectFailed
			result.Failure = AsFailure(err)
		}
		if rep != nil && rep.Failure != nil {
			result.Failure = rep.Failure
		}
		if result.Failure == nil && rep != nil && rep.Failed() {
			// describeFailure already folds in the phase's captured output, and the
			// report this failure came from has the same Stderr the phase carries.
			result.Failure = &Failure{Class: ClassUserCode, NextAction: ActionModelFix, Cause: errors.New(describeFailure(rep))}
		}
		result.Phases = append(result.Phases, pr)
		if err != nil || (rep != nil && rep.Failed()) {
			if phase.Required {
				result.FailedPhase = phase.Name
				result.ExitCode = pr.ExitCode
				result.Status = ProjectFailed
				result.NextAction = projectNextAction(result.Failure)
				break
			}
			result.Status = ProjectPartial
			result.NextAction = ActionContinue
		}
	}
	if result.Status != ProjectFailed {
		if err := sb.collectProjectArtifacts(ctx, sess, proj, result); err != nil {
			result.Status = ProjectFailed
			result.Failure = AsFailure(err)
			result.NextAction = projectNextAction(result.Failure)
		}
	}
	if result.Status == ProjectUnset {
		result.Status = ProjectSucceeded
		result.NextAction = ActionContinue
	}
	result.Duration = time.Since(started)
	return result, nil
}

// collectProjectArtifacts gathers a project's declared outputs from the session
// workspace. Collection runs even when a phase failed: the test report is often
// the reason the phase was run (§13.3).
func (sb *Sandbox) collectProjectArtifacts(ctx context.Context, sess *Session, proj Project, result *ProjectResult) error {
	if len(proj.Artifacts) == 0 {
		return nil
	}
	env, err := sb.environmentFor(ctx, sess, &RunContext{SessionID: sess.id, Request: &Request{}})
	if err != nil {
		return err
	}
	fs, err := sb.filesOf(ctx, env)
	if err != nil {
		return err
	}
	c := newCollector(fs, sb.cfg.Limits)
	total := int64(0)
	for _, spec := range proj.Artifacts {
		found, size, err := c.collectOne(ctx, spec, func(a Artifact) {
			result.Artifacts = append(result.Artifacts, a)
		})
		if err != nil {
			return err
		}
		total += size
		if !found && spec.Required {
			return &ArtifactError{Spec: spec, Reason: "missing"}
		}
	}
	if limit := sb.cfg.Limits.ArtifactTotalMaxBytes; limit > 0 && total > limit {
		return &ArtifactError{Spec: ArtifactSpec{Path: "*", MaxBytes: limit}, Reason: "too-large", Found: total, Limit: limit}
	}
	sb.emit(ctx, Event{Kind: EventArtifactsCollected, SessionID: sess.id, RunID: result.RunID, Detail: map[string]any{"count": len(result.Artifacts)}})
	return nil
}

// projectNextAction maps a failure to the concrete continuation (§16.6).
func projectNextAction(f *Failure) Action {
	if f == nil {
		return ActionContinue
	}
	switch f.Class {
	case ClassLost:
		return ActionRebuild
	case ClassTimeout:
		return ActionRestoreCheckpoint
	case ClassUserCode:
		return ActionModelFix
	case ClassDenied:
		return ActionRequestApproval
	default:
		return f.NextAction
	}
}

// resolveMount applies the profile default and keeps MountShare explicit.
func resolveMount(asked, def MountMode) MountMode {
	if asked == MountUnknown || asked == 0 {
		return def
	}
	if asked == MountShare && def != MountShare {
		return def
	}
	return asked
}

// LeaseSweeper is implemented by providers that can find orphaned environments
// left by a crash (§15).
type LeaseSweeper interface {
	Sweep(ctx context.Context) ([]string, error)
}

// Check is one diagnosis line: what was expected, what was found, and the
// single command that changes the latter into the former (§5.3).
type Check struct {
	Env     string `json:"env"`
	Want    string `json:"want"`
	Got     string `json:"got"`
	OK      bool   `json:"ok"`
	Fix     string `json:"fix,omitempty"`
	Elapsed string `json:"elapsed,omitempty"`
}

// DiagReport is the machine-readable form of "why can't this run here?".
type DiagReport struct {
	Checks  []Check   `json:"checks"`
	Reached Isolation `json:"reached"`
	Fixes   []string  `json:"fixes"`
	Summary string    `json:"summary"`
}

// Diagnose probes the environment and returns actionable findings. It reports
// failures as data, not as an error, so a broken install still gets a usable
// answer instead of one opaque line (D-05).
func (sb *Sandbox) Diagnose(ctx context.Context) (*DiagReport, error) {
	report := &DiagReport{Reached: sb.iso}
	report.Checks = append(report.Checks, Check{Env: "provider", Want: sb.cfg.Provider, Got: sb.name, OK: sb.provider != nil})
	report.Checks = append(report.Checks, Check{Env: "formation", Want: sb.cfg.Formation.String(), Got: sb.cfg.Formation.String(), OK: true})
	report.Checks = append(report.Checks, Check{Env: "isolation", Want: sb.cfg.MinIsolation.String(), Got: sb.iso.String(), OK: sb.iso.AtLeast(sb.cfg.MinIsolation)})
	if !sb.iso.AtLeast(sb.cfg.MinIsolation) {
		report.Fixes = append(report.Fixes, fmt.Sprintf("require isolation %s or lower the profile floor", sb.cfg.MinIsolation))
	}
	caps := sb.caps.Strings()
	report.Checks = append(report.Checks, Check{Env: "capabilities", Want: "non-empty", Got: fmt.Sprint(caps), OK: len(caps) > 0})
	if err := sb.provider.Probe(ctx); err != nil {
		report.Checks = append(report.Checks, Check{Env: "probe", Want: "ok", Got: err.Error(), OK: false, Fix: firstFix(err)})
		if fix := firstFix(err); fix != "" {
			report.Fixes = append(report.Fixes, fix)
		}
	} else {
		report.Checks = append(report.Checks, Check{Env: "probe", Want: "ok", Got: "ok", OK: true})
	}
	if sb.cfg.AllowFallback && len(sb.cfg.Observers) == 0 {
		report.Checks = append(report.Checks, Check{Env: "fallback-observer", Want: "observer registered", Got: "none", OK: false, Fix: "add WithObserver: a downgrade that nobody reports is a silent isolation loss"})
		report.Fixes = append(report.Fixes, "register an observer before enabling fallback tiers")
	}
	sb.observeHealth(ctx, report)
	report.Summary = summarizeChecks(report.Checks)
	return report, nil
}

// observeHealth appends provider-specific checks for the things that only show
// up at runtime: orphaned sessions, leaked snapshots, quota headroom.
func (sb *Sandbox) observeHealth(ctx context.Context, report *DiagReport) {
	if sweeper, ok := sb.provider.(LeaseSweeper); ok {
		orphans, err := sweeper.Sweep(ctx)
		switch {
		case err != nil:
			report.Checks = append(report.Checks, Check{Env: "leases", Want: "no orphans", Got: err.Error(), OK: false})
		case len(orphans) > 0:
			report.Checks = append(report.Checks, Check{Env: "leases", Want: "no orphans", Got: fmt.Sprint(len(orphans)) + " orphaned", OK: false, Fix: "hno sandbox prune --orphans"})
			report.Fixes = append(report.Fixes, "hno sandbox prune --orphans")
		default:
			report.Checks = append(report.Checks, Check{Env: "leases", Want: "no orphans", Got: "0", OK: true})
		}
	}
	sb.mu.RLock()
	live := len(sb.sessions)
	sb.mu.RUnlock()
	report.Checks = append(report.Checks, Check{Env: "sessions", Want: "< quota", Got: fmt.Sprint(live), OK: true})
}

// firstFix pulls the first remediation command out of an error.
func firstFix(err error) string {
	var f fixer
	if errors.As(err, &f) {
		fixes := f.Remediation()
		if len(fixes) > 0 {
			return fixes[0]
		}
	}
	return ""
}

// summarizeChecks describes the overall diagnosis in one line.
func summarizeChecks(checks []Check) string {
	bad := 0
	for _, c := range checks {
		if !c.OK {
			bad++
		}
	}
	if bad == 0 {
		return fmt.Sprintf("all %d checks passed", len(checks))
	}
	return fmt.Sprintf("%d of %d checks failed; see fixes", bad, len(checks))
}

// hookGuard recovers a panic raised inside any extension point and converts it
// to a fault error, so one broken hook cannot take down an agent run (§7).
func hookGuard(name string, fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = &HookFaultError{Kind: name, Name: name, Detail: fmt.Sprintf("panic: %v\n%s", r, debug.Stack())}
		}
	}()
	return fn()
}

// Reclaimer is the operator-facing view of a provider's orphan inventory.
type Reclaimer interface {
	// Sweep reports engine-held environments this handle no longer owns.
	Sweep(ctx context.Context) ([]string, error)
	// Prune deletes what Sweep reported and returns the ids removed.
	Prune(ctx context.Context) ([]string, error)
}

// Reclaimer exposes the provider's orphan reclamation, when the provider has one.
//
// Diagnose can reach a sweeper, but that couples "reclaim what leaked" to "run a
// full health report". Cleanup truth is supposed to be observable and actionable on
// its own (§17.1), and a restart leaves residue that no in-process map can see, so
// this is what an operator or a cron job calls.
//
// It reports false for a provider that cannot enumerate engine-side residue, which
// is a capability answer, not an error: claiming a sweep exists when it does not
// would let a caller believe leaked environments were reclaimed.
func (sb *Sandbox) Reclaimer() (Reclaimer, bool) {
	sb.mu.Lock()
	provider := sb.provider
	sb.mu.Unlock()
	if provider == nil {
		return nil, false
	}
	r, ok := provider.(Reclaimer)
	return r, ok
}
