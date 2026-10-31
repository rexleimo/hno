package sandbox

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"
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

	env, prov, err := sb.newEnvironment(ctx, sess.id, spec, spec.Expose)
	if err != nil {
		sess.mu.Lock()
		sess.state = StateLost
		sess.mu.Unlock()
		return nil, err
	}
	sb.setSessionWorkspace(sess, workspace)

	// Describe the box that has just been built, not the session that asked for it.
	// Under this mode the new environment is rendered from the base image, so it
	// inherited nothing whoever its caller used to be.
	rep := &RebuildReport{SessionID: sess.id, Duration: time.Since(started), StateSource: prov.source}
	if err := sb.replayRecorded(ctx, env, spec, rep, prov); err != nil {
		_ = env.Close(ctx)
		sess.mu.Lock()
		sess.state = StateLost
		sess.mu.Unlock()
		return rep, err
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
	// The provenance the creator returned has to land on the session, not just steer the
	// replay above. adoptWorkspace minted a placeholder lineage for a box this handle had
	// not traced; leaving it in place meant a rebuilt session kept reporting a lineage it
	// never had, which is the D-20 drift in another spot: the watermark that stops a step
	// cache skipping work the new box never ran, and any promise that binds to a specific
	// environment, were both reading the placeholder.
	sess.lineage, sess.inherited, sess.stateSource = prov.lineage, prov.upto, prov.source
	sess.mu.Unlock()
	rep.Duration = time.Since(started)
	sb.emit(ctx, Event{Kind: EventRebuilt, SessionID: sess.id, Detail: map[string]any{"replayed": len(rep.Replayed), "cached": rep.Cached, "dropped": rep.Dropped}})
	return rep, nil
}

// replayRecorded re-applies the steps a session recorded onto a freshly created
// environment.
//
// This is the single owner of replay. It exists because the acquisition path and the
// rebuild path both need it, and because duplicating it inside a provider is how the
// work ends up done twice or, worse, never: EnvSpec.ReplayFrom was documented as the
// channel for it, the core never filled the field, and a provider loop over it was
// therefore unreachable while the resumed session still reported itself Ready.
func (sb *Sandbox) replayRecorded(ctx context.Context, env Environment, spec SessionSpec, rep *RebuildReport, prov boxProvenance) error {
	lin, upto := prov.lineage, prov.upto
	for _, step := range spec.Steps {
		res := StepResult{Step: step}
		// A cached hit may skip work only when this environment actually carries the
		// state the step produced. That is the whole of D-20 option B, and it is decided
		// from a fact about the box rather than from the step's content: a box built from
		// the base image has inherited nothing, so nothing can be skipped and the
		// workflow's dependencies are guaranteed to be reinstalled.
		if cached, ok := sb.stepCached(lin, upto, step); ok && cached {
			res.Cached = true
			rep.Cached++
			continue
		}
		stepStart := time.Now()
		if err := sb.replayStep(ctx, env, step); err != nil {
			res.Err = err
			rep.Replayed = append(rep.Replayed, res)
			return &StepReplayError{Step: step, Cause: err, Replayed: len(rep.Replayed), Cached: rep.Cached}
		}
		res.Elapsed = time.Since(stepStart)
		rep.Replayed = append(rep.Replayed, res)
		sb.rememberStep(lin, step)
	}
	return nil
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
		// Replaying a materialise step means putting the file back. Asserting it
		// already exists cannot be the whole of it: in a box rendered from the base
		// image nothing exists yet, which is why every rebuild of a session that had
		// recorded an input failed here.
		return sb.replayContent(ctx, env, step)
	default:
		return fmt.Errorf("cannot replay step kind %s", step.Kind)
	}
	rep, err := env.Exec(ctx, RunSpec{
		RunID:     newRunID(),
		SessionID: env.ID(),
		Request:   req,
		MountMode: sb.cfg.Limits.DefaultMountMode,
		Budget:    Budget{ExecTimeout: sb.cfg.Timeouts.Exec, SetupTimeout: sb.cfg.Timeouts.Setup, OutputLimit: sb.cfg.Limits.OutputMaxBytes},
		Limits:    sb.cfg.Limits,
	}, nil)
	if err != nil {
		return err
	}
	// A step that exits nonzero did not happen, whatever the provider returned for it.
	// Providers report a failing payload as a Report plus a nil error -- correctly,
	// because a nonzero exit is the user's result, not the sandbox's fault -- so a
	// replay that only checks err would stamp a failed install as satisfied and then
	// cache that conclusion. Same class as the phase status bug fixed in B2a, at the
	// one call site that still had it.
	if rep != nil && rep.ExitCode != 0 {
		detail := strings.TrimSpace(rep.Stderr)
		if detail == "" {
			detail = strings.TrimSpace(rep.Stdout)
		}
		if len(detail) > failureDetailBytes {
			detail = "..." + detail[len(detail)-failureDetailBytes:]
		}
		if detail == "" {
			detail = "no output captured"
		}
		return fmt.Errorf("step exited %d: %s", rep.ExitCode, detail)
	}
	return nil
}

// stepCached consults the replay cache. Caching is step-level, so a changed
// source file does not invalidate an expensive install (§17.2).
func (sb *Sandbox) stepCached(lineage string, inherited int, step Step) (bool, bool) {
	if sb.cfg.StepCache == nil || lineage == "" {
		return false, false
	}
	// The watermark is the decisive half. A fresh box inherits nothing (inherited < 0),
	// so no entry under any lineage can justify skipping it.
	if step.Sequence > inherited {
		return false, false
	}
	key := stepCacheKey(lineage, step)
	_, ok, err := sb.cfg.StepCache.Load(context.Background(), key)
	if err != nil {
		sb.emit(context.Background(), Event{Kind: EventHookFault, Detail: map[string]any{"cache": "step", "error": err.Error()}})
		return false, false
	}
	return true, ok
}

// rememberStep records a replayed step as cached.
func (sb *Sandbox) rememberStep(lineage string, step Step) {
	if sb.cfg.StepCache == nil || lineage == "" {
		return
	}
	entry := JournalEntry{Key: stepCacheKey(lineage, step), RunID: "step", State: RunStateDone, PayloadHash: step.Digest}
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
	// Declared is not delivered. Two separate facts get reported here because they
	// need different fixes: a bit the core has no behaviour for is a vocabulary
	// problem, while a bit with no interface behind it is a provider bug.
	if dead := sb.caps.Unconsumed(); len(dead) > 0 {
		names := make([]string, 0, len(dead))
		for _, c := range dead {
			names = append(names, c.String())
		}
		report.Checks = append(report.Checks, Check{
			Env:  "capabilities-without-behaviour",
			Want: "none",
			Got:  fmt.Sprint(names),
			OK:   false,
			Fix:  "these bits are declared but no core path acts on them; do not rely on them, see SemanticsOf",
		})
		report.Fixes = append(report.Fixes, fmt.Sprintf("capabilities %v are declared but unimplemented in the core — treat them as absent", names))
	}
	// A readiness probe needs somewhere to look. Pattern probes read the core's own output
	// tail and need nothing from the engine, but a Port or URL probe has to be answered
	// inside the sandbox, and a provider that cannot keep a process running cannot serve
	// any probe at all. Reporting that here saves a caller from discovering it on the run
	// that was supposed to depend on the probe.
	if !sb.caps.Has(CapBackground) {
		report.Checks = append(report.Checks, Check{
			Env:  "readiness",
			Want: "detached runs so a probe can be answered",
			Got:  "provider declares no background support",
			OK:   true,
			Fix:  "Request.ReadyProbe will be refused on this provider; gate on a finished run instead, or pick a provider with background support",
		})
	} else if _, ok := sb.provider.(ReadinessProber); !ok {
		report.Checks = append(report.Checks, Check{
			Env:  "readiness",
			Want: "Port or URL probes answerable in-guest",
			Got:  "provider is not a ReadinessProber",
			OK:   true,
			Fix:  "only Pattern probes can be answered here; a Port or URL probe returns UnsupportedProbeError",
		})
	}
	// A suspended approval is work that is not running and resources that are not free.
	// It is reported here because a caller staring at a stalled workflow needs to be able
	// to tell "the engine is slow" from "somebody has not answered yet".
	if n := sb.approvals.outstanding(); n > 0 {
		report.Checks = append(report.Checks, Check{
			Env:  "approvals",
			Want: "none pending",
			Got:  fmt.Sprintf("%d suspended request(s)", n),
			OK:   false,
			Fix:  "list them with Session.Awaiting() and answer with Approve/Deny, or widen WithTimeouts(Timeouts{ApprovalHold: ...})",
		})
		report.Fixes = append(report.Fixes, fmt.Sprintf("%d approval(s) are holding work with nobody to answer them", n))
	}
	if err := probeBounded(ctx, sb.provider, sb.cfg.Timeouts.Connect); err != nil {
		report.Checks = append(report.Checks, Check{Env: "probe", Want: "ok", Got: err.Error(), OK: false, Fix: firstFix(err)})
		if fix := firstFix(err); fix != "" {
			report.Fixes = append(report.Fixes, fix)
		}
	} else {
		report.Checks = append(report.Checks, Check{Env: "probe", Want: "ok", Got: "ok", OK: true})
	}
	// The image question, separate from the engine question above (D-36). A handle bound
	// to an image the engine cannot start is not healthy, and "probe: ok" never meant it
	// was -- the probe deliberately avoids pulling, so an absent image was invisible here
	// and only surfaced as a failed run.
	if img := sb.cfg.Image; img != "" && sb.provider != nil {
		q, canAsk := sb.provider.(ImageQueryable)
		switch {
		case !canAsk:
			report.Checks = append(report.Checks, Check{
				Env:  "image",
				Want: "presence answerable by the provider",
				Got:  fmt.Sprintf("provider %q answers no image query", sb.name),
				OK:   false,
				Fix:  fmt.Sprintf("implement sandbox.ImageQueryable on provider %q, or bind WithImage to an image that provider can verify; an unverifiable image is not a passing check", sb.name),
			})
			report.Fixes = append(report.Fixes, fmt.Sprintf("provider %q cannot say whether image %q exists, so this report cannot vouch for it", sb.name, img))
		default:
			present, err := q.ImageAvailable(ctx, img)
			switch {
			case err != nil:
				report.Checks = append(report.Checks, Check{
					Env:  "image",
					Want: "answerable",
					Got:  "unknown: " + err.Error(),
					OK:   false,
					Fix:  fmt.Sprintf("the engine would not answer for image %q; check registry access or credentials, then re-run Diagnose", img),
				})
				report.Fixes = append(report.Fixes, fmt.Sprintf("image %q could not be verified: %v", img, err))
			case !present:
				report.Checks = append(report.Checks, Check{
					Env:  "image",
					Want: "present in the local store",
					Got:  fmt.Sprintf("absent: %s", img),
					OK:   false,
					Fix:  fmt.Sprintf("pull or build %s in this engine, or point WithImage at an image it already has; nothing will start until then", img),
				})
				report.Fixes = append(report.Fixes, fmt.Sprintf("image %s is not available in this engine", img))
			default:
				report.Checks = append(report.Checks, Check{
					Env:  "image",
					Want: "present in the local store",
					Got:  fmt.Sprintf("present: %s", img),
					OK:   true,
				})
			}
		}
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
			report.Checks = append(report.Checks, Check{Env: "leases", Want: "no orphans", Got: fmt.Sprint(len(orphans)) + " orphaned", OK: false, Fix: "reclaim them through the handle: r, ok := sb.Reclaimer(); r.Prune(ctx) deletes what it reports, or r.Sweep(ctx) first to look before deleting; from a shell, run hno sandbox prune to list and hno sandbox prune --yes to delete"})
			report.Fixes = append(report.Fixes, "call Sandbox.Reclaimer() and Prune the leaked environments; Sweep reports them without deleting, or run hno sandbox prune (add --yes to delete)")
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

// stepCacheKey scopes a satisfied step to the state lineage that carries its effect.
//
// Step content alone is not enough to decide anything: the same pip install satisfied in
// one container says nothing about another, and a digest-keyed cache let one session's
// step suppress a different session's install (measured: cached=1 replayed=0 with an
// empty box). Binding the entry to the lineage that carries the effect is what turns a
// cache hit back into evidence.
func stepCacheKey(lineage string, step Step) string {
	return lineage + "|" + step.Key()
}

// snapshotMark records how far a session's spec had been satisfied when a snapshot was
// taken, which is what makes a later Restore able to say which steps its box contains.
type snapshotMark struct {
	lineage string
	upto    int
	source  string
}

// rememberSnapshotMark stores the mark for one handle.
func (sb *Sandbox) rememberSnapshotMark(handle SnapshotHandle, mark snapshotMark) {
	if handle.ID == "" {
		handle.ID = handle.SessionID + "/" + handle.Name
	}
	sb.mu.Lock()
	defer sb.mu.Unlock()
	if sb.snapMarks == nil {
		sb.snapMarks = map[string]snapshotMark{}
	}
	sb.snapMarks[handle.ID] = mark
}

// snapshotMarkFor looks a mark up by handle id, then by session and name.
func (sb *Sandbox) snapshotMarkFor(handle SnapshotHandle) (snapshotMark, bool) {
	sb.mu.Lock()
	defer sb.mu.Unlock()
	if len(sb.snapMarks) == 0 {
		return snapshotMark{}, false
	}
	if m, ok := sb.snapMarks[handle.ID]; ok {
		return m, true
	}
	return snapshotMark{}, false
}

// verifyStaged confirms a re-materialised path is readable through the provider's own
// file service. A provider without a bridge cannot be checked this way, and that is
// reported rather than assumed away.
func (sb *Sandbox) verifyStaged(ctx context.Context, env Environment, path string) error {
	bridge, ok := sb.provider.(FileBridge)
	if !ok || !sb.caps.Has(CapFileBridge) {
		return &CapabilityError{Provider: sb.name, Missing: CapFileBridge, Wanted: sb.caps}
	}
	files, ferr := bridge.Files(ctx, env)
	if ferr != nil {
		return fmt.Errorf("sandbox: rebuild cannot verify %q: %w", path, ferr)
	}
	info, err := files.Stat(ctx, path)
	if err != nil {
		return fmt.Errorf("rebuild placed %q but it cannot be read back: %w", path, err)
	}
	if info.Size == 0 {
		return fmt.Errorf("rebuild placed %q but the file is empty", path)
	}
	return nil
}

// replayContent puts a recorded materialise step back into the environment.
//
// Two decisions live here. Staging goes through the core pipeline because a provider is
// not required to interpret Request.Inputs in its Exec at all — asking the engine to "run"
// a content-only step wrote nothing. And when the content is placed and read back through
// the provider's own file service, the step is satisfied: inventing a follow-up shell
// command would test the engine's shell grammar instead of the promise that the file is
// there, and would give a provider without a shell a way to fail a replay that worked.
func (sb *Sandbox) replayContent(ctx context.Context, env Environment, step Step) error {
	switch {
	case step.Input != nil:
		in := *step.Input
		if in.Path == "" {
			in.Path = step.Path
		}
		if err := sb.stageInputs(ctx, env, &Request{Inputs: []Input{in}}); err != nil {
			return fmt.Errorf("re-materialise %q: %w", in.Path, err)
		}
		return sb.verifyStaged(ctx, env, in.Path)
	case step.Source != nil:
		if err := sb.stageInputs(ctx, env, &Request{Workspace: step.Source}); err != nil {
			return fmt.Errorf("re-materialise workspace: %w", err)
		}
		return nil
	default:
		return &UnrecoverableInputError{
			Path:  step.Path,
			Cause: "the recorded step keeps no content a rebuild can write back",
			Fix:   "record the input with Bytes or HostPath; a step holding only a path cannot survive a rebuild",
		}
	}
}
