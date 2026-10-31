package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/fnv"
	"strings"
	"sync"
	"time"
)

// Sandbox is a handle to one bound provider. It is the entry point of the
// package and the only object that owns policy, observer, sink, budget and
// audit state. One handle per process is the intended usage: it holds a probe
// cache and a live session registry (§5.2).
type Sandbox struct {
	cfg Config

	provider Provider
	name     string
	caps     CapabilitySet
	iso      Isolation

	mu       sync.RWMutex
	sessions map[string]*Session
	// approvals is the register of suspended requests awaiting a human answer. It lives
	// on the handle because a one-shot run can be suspended too, and whoever holds the id
	// has to be able to answer it. Guarded by mu.
	approvals *approvalBook
	// snapMarks records how far each snapshot's spec had been satisfied.
	snapMarks map[string]snapshotMark
	createdAt time.Time
	closed    bool

	probeAt    time.Time
	probeErr   error
	breaker    Breaker
	storm      *StormGuard
	journal    JournalStore
	audit      AuditSink
	closeOnce  sync.Once
	lastReport atomicReport
}

// atomicReport keeps the most recent terminal report for Status diagnostics.
type atomicReport struct {
	mu   sync.Mutex
	last *Report
}

func (a *atomicReport) set(r *Report) {
	a.mu.Lock()
	a.last = r
	a.mu.Unlock()
}

func (a *atomicReport) get() *Report {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.last
}

// Open builds a handle, probes the provider, and negotiates capabilities. It
// either returns a usable handle or a typed error naming every provider it
// tried and the command that fixes it (D-05).
func Open(ctx context.Context, opts ...Option) (*Sandbox, error) {
	cfg := Config{}
	for _, o := range opts {
		if o != nil {
			o(&cfg)
		}
	}
	if err := cfg.resolve(); err != nil {
		return nil, fmt.Errorf("sandbox: resolve profile: %w", err)
	}
	sb := &Sandbox{
		approvals: newApprovalBook(),
		cfg:       cfg,
		sessions:  map[string]*Session{},
		createdAt: time.Now().UTC(),
		journal:   cfg.Journal,
		audit:     cfg.audit,
		breaker:   cfg.Breaker,
		storm:     newStormGuard(cfg.Storm),
	}
	if err := sb.bind(ctx); err != nil {
		return nil, err
	}
	return sb, nil
}

// bind selects, probes and negotiates with a provider, applying the fallback
// ladder only when the profile allowed it.
func (sb *Sandbox) bind(ctx context.Context) error {
	candidates, err := sb.candidates()
	if err != nil {
		return err
	}
	var tried []Attempt
	for _, name := range candidates {
		prov, berr := DefaultRegistry.build(ProviderConfig{
			Name:      name,
			Image:     sb.cfg.Image,
			Formation: sb.cfg.Formation,
			// Provision is the boot window; Connect is how long to reach a
			// daemon. Feeding the latter into container create/start capped
			// startup at 3s on a busy engine.
			Timeout: Duration(sb.cfg.Timeouts.Provision),
			Labels:  labelAttrs(sb.cfg.Attrs),
			// Raw carries the engine-specific half of WithAttrs. Without it the
			// field exists in the contract but nothing can ever reach a provider,
			// which leaves an engine with no way to be configured except through
			// process globals (the pre-registration PATH-scanning habit D-03
			// removed).
			Raw: sb.cfg.Attrs,
		})
		if berr != nil {
			tried = appendAttempt(tried, name, berr)
			continue
		}
		pctx, cancel := context.WithTimeout(ctx, sb.cfg.Timeouts.Probe)
		perr := prov.Probe(pctx)
		cancel()
		if perr != nil {
			tried = appendAttempt(tried, name, perr)
			_ = prov.Close(ctx)
			continue
		}
		iso := prov.Isolation()
		caps := prov.Capabilities()
		if !iso.AtLeast(sb.cfg.MinIsolation) {
			tried = append(tried, Attempt{Provider: name, Isolation: iso, Reason: fmt.Sprintf("isolation %s below required %s", iso, sb.cfg.MinIsolation), Fix: "choose a provider that meets the floor or lower WithMinimumIsolation",
				Cause: &IsolationError{Provider: name, Want: sb.cfg.MinIsolation, Got: iso}})
			_ = prov.Close(ctx)
			continue
		}
		if sb.cfg.Egress.Mode == EgressAllowlist && !caps.Has(CapEgressAllowlist) {
			tried = append(tried, Attempt{Provider: name, Isolation: iso, Reason: "provider cannot enforce an egress allowlist", Fix: "use a provider with egress control or run with EgressNone",
				Cause: &CapabilityError{Provider: name, Missing: CapEgressAllowlist, Wanted: caps}})
			_ = prov.Close(ctx)
			continue
		}
		// Claims are reconciled against implementations before the provider is
		// adopted. A declared capability whose backing interface is absent can never
		// work, and the alternative is a caller discovering it on their 400th run from
		// an error message that contradicts itself.
		if problems := caps.VerifyClaims(prov); len(problems) > 0 {
			var fix, reason string
			if ue, ok := problems[0].(*UnimplementedCapabilityError); ok {
				reason = fmt.Sprintf("declares %q without implementing %s", ue.Capability, ue.Iface)
				fix = fmt.Sprintf("implement %s or drop the capability from Capabilities()", ue.Iface)
			} else {
				reason = problems[0].Error()
				fix = "fix the provider's capability declaration"
			}
			tried = append(tried, Attempt{Provider: name, Isolation: iso, Reason: reason, Fix: fix, Cause: problems[0]})
			_ = prov.Close(ctx)
			continue
		}
		sb.provider, sb.name, sb.caps, sb.iso = prov, name, caps, iso
		sb.probeAt = time.Now()
		sb.emit(ctx, Event{Kind: EventResolved, Provider: name, Isolation: iso, Detail: map[string]any{"capabilities": caps.Strings()}})
		if len(tried) > 0 && len(sb.cfg.Observers) == 0 && sb.cfg.AllowFallback {
			// D-13: fallback without observers is rejected in Validate, so
			// reaching here means the primary failed outright.
			return &UnavailableError{Tried: tried, Want: sb.cfg.MinIsolation}
		}
		return nil
	}
	// The headline names what actually stopped the build. "No provider met the required
	// isolation" was reported even when no provider was ever found to ask, which sent
	// readers to the isolation ladder over a missing registration or a typo in
	// WithProvider. The first recorded attempt carries the real reason, so it leads.
	reason := "no registered provider met the required isolation"
	if len(tried) > 0 && tried[0].Reason != "" {
		reason = fmt.Sprintf("no provider could be used for %q", strings.Join(candidates, ", "))
	}
	if !sb.cfg.AllowFallback {
		reason += "; fallback is disabled (FailFast per D-13)"
	}
	un := &UnavailableError{Tried: tried, Want: sb.cfg.MinIsolation, Reason: reason}
	for _, a := range tried {
		if a.Fix != "" {
			un.Fixes = append(un.Fixes, a.Fix)
		}
		if a.Reason != "" && strings.Contains(a.Reason, "not registered") {
			un.Fixes = append(un.Fixes, fmt.Sprintf("register provider %q before Open, or name one that is registered", a.Provider))
		}
	}
	return un
}

// candidates lists provider names to try, honouring the explicit choice, the
// allow list, registry order, and finally the fallback ladder.
func (sb *Sandbox) candidates() ([]string, error) {
	if sb.cfg.Provider != "" {
		return []string{sb.cfg.Provider}, nil
	}
	names := DefaultRegistry.Names()
	if len(sb.cfg.Allowed) > 0 {
		var out []string
		for _, name := range names {
			for _, want := range sb.cfg.Allowed {
				if name == want {
					out = append(out, name)
				}
			}
		}
		if len(out) == 0 {
			return nil, &UnavailableError{Want: sb.cfg.MinIsolation, Reason: "no allowed provider is registered", Tried: attemptsOf(sb.cfg.Allowed)}
		}
		return out, nil
	}
	return names, nil
}

// Status reports what this handle actually provides (§5.2).
func (sb *Sandbox) Status() Status {
	sb.mu.RLock()
	sessions := len(sb.sessions)
	sb.mu.RUnlock()
	return Status{
		Provider:     sb.name,
		Isolation:    sb.iso,
		Capabilities: sb.caps,
		Image:        sb.cfg.Image,
		Formation:    sb.cfg.Formation,
		Sessions:     sessions,
		StartedAt:    sb.createdAt,
		Limits:       sb.cfg.Limits,
	}
}

// Run executes one request on a fresh environment. One-shot semantics stay the
// default so today's callers do not pay for sessions they do not use (§5.3).
func (sb *Sandbox) Run(ctx context.Context, req Request, opts ...RunOption) (*Report, error) {
	ro := resolveRunOptions(opts, &sb.cfg)
	return sb.execute(ctx, nil, req, ro)
}

// RunProject executes a project on a session created for it, releasing the
// session afterwards unless the caller pinned an id (§6.4).
func (sb *Sandbox) RunProject(ctx context.Context, proj Project, opts ...RunOption) (*ProjectResult, error) {
	ro := resolveRunOptions(opts, &sb.cfg)
	id := ro.RunID
	if id == "" {
		id = "project-" + newRunID()
	}
	// RunProject creates the session it runs on, so it says so: with the unknown-id refusal
	// now honoured, an implicit "just boot it" would fail for a pinned id nobody has seen.
	sess, err := sb.Session(ctx, id, WithCreateIfMissing(true))
	if err != nil {
		return nil, err
	}
	res, err := sb.runProject(ctx, sess, proj, ro)
	if ro.RunID == "" {
		_ = sess.Release(ctx)
	}
	return res, err
}

// SessionOption tunes session acquisition. Options only narrow behaviour; they
// can never lower the handle's isolation or capability requirements (§5.3).
type SessionOption func(*sessionConfig)

// sessionConfig is the resolved form of SessionOption.
type sessionConfig struct {
	Image           string
	IdleTTL         time.Duration
	CreateIfMissing bool
	Replay          bool
	Expose          []PortMap
}

// WithSessionExpose publishes guest ports when this session's environment is created.
//
// Port mapping is a property of the substrate, not of a run: the engine learns about it
// at create, so this is the only place a caller can ask. A Request.Expose that a session
// did not publish is refused with *ExposeConflictError rather than dropped.
func WithSessionExpose(maps ...PortMap) SessionOption {
	return func(c *sessionConfig) { c.Expose = append([]PortMap(nil), maps...) }
}

// WithSessionImage overrides the handle image for this session.
func WithSessionImage(image string) SessionOption {
	return func(c *sessionConfig) { c.Image = image }
}

// WithSessionIdleTTL overrides the idle timeout; zero keeps the profile value.
func WithSessionIdleTTL(d time.Duration) SessionOption {
	return func(c *sessionConfig) { c.IdleTTL = d }
}

// WithCreateIfMissing allows an unknown id to be provisioned. Without it, an
// unknown id is a *StaleSessionError — the safer default for a resuming
// workflow, which usually wants to know the box is gone rather than pay for a
// fresh boot (§5.3).
func WithCreateIfMissing(allow bool) SessionOption {
	return func(c *sessionConfig) { c.CreateIfMissing = allow }
}

// WithSessionReplay rebuilds a known id whose environment is gone (§17.2).
func WithSessionReplay(allow bool) SessionOption {
	return func(c *sessionConfig) { c.Replay = allow }
}

// Session binds an id to a live environment: reuse it if alive, rebuild it if
// the id is known but the environment is gone, provision it otherwise. This is
// the only call that pays boot cost (§5.3) and it is why a business session id,
// not a random one, is the key.
func (sb *Sandbox) Session(ctx context.Context, id string, opts ...SessionOption) (*Session, error) {
	var opt sessionConfig
	for _, fn := range opts {
		if fn != nil {
			fn(&opt)
		}
	}
	if id == "" {
		return nil, errors.New("sandbox: session requires an id")
	}
	if !sb.caps.Has(CapSession) {
		return nil, &CapabilityError{Provider: sb.name, Missing: CapSession, Wanted: sb.caps}
	}
	sb.mu.Lock()
	existing, held := sb.sessions[id]
	sb.mu.Unlock()
	// carriedSpec is the recorded spec of a released session in this process. It has to
	// survive into the replacement, or re-acquiring an expired id yields an empty box:
	// a store-less handle has no SessionRecord to replay from, yet the spec was never
	// lost -- only the environment was.
	var carriedSpec *SessionSpec
	if held {
		st := existing.Status()
		if !st.State.Terminal() {
			return existing, nil
		}
		if st.State == StateCorrupted {
			return nil, &StateError{SessionID: id, State: st.State, Action: "reuse session",
				Reason: "the recorded spec could not be replayed; retrying reproduces the same failure",
				Fix:    "inspect and correct SessionSpec, or Destroy the id and create it anew"}
		}
		// A released session is gone, which is exactly what CreateIfMissing promises to
		// handle: the id may come back because the recorded spec survived. Refusing here
		// would make the remedy named by the expiry error false, and would turn "the TTL
		// is the safety net" into a one-way door.
		if !opt.CreateIfMissing {
			return nil, &StateError{SessionID: id, State: st.State, Action: "reuse session",
				Reason: "the session was released and its environment is gone; the recorded spec is still kept",
				Fix:    "pass WithCreateIfMissing to re-provision the id from that spec"}
		}
		sb.mu.Lock()
		// Re-check under the lock: another caller may already have replaced it.
		if cur, still := sb.sessions[id]; still && cur != existing {
			sb.mu.Unlock()
			return cur, nil
		}
		delete(sb.sessions, id)
		sb.mu.Unlock()
		sp := existing.Spec()
		if len(sp.Steps) > 0 || len(sp.Env) > 0 || len(sp.Expose) > 0 {
			carriedSpec = &sp
		}
	}

	rec, found, err := sb.loadRecord(ctx, id)
	if err != nil {
		return nil, err
	}
	if !found && !opt.CreateIfMissing && !opt.Replay {
		// An unknown id with no store record is a caller bug or a stale resume, and the
		// refusal has to happen either way. It used to be guarded on a session store being
		// configured, so on a store-less handle the option was a silent no-op that booted a
		// fresh environment and handed it back as if it were the session being resumed: a
		// workflow that believed it was continuing a project would find an empty box and
		// attribute the missing state to its own code. Whether a store exists changes only
		// what can be said about the cause, not whether to refuse.
		cause := errors.New("no session record for this id; use WithCreateIfMissing to start fresh")
		if sb.cfg.Sessions == nil {
			cause = errors.New("this handle has no session store, so an unknown id cannot be verified as a resumption; use WithCreateIfMissing to start fresh")
		}
		return nil, &StaleSessionError{SessionID: id, Provider: sb.name, Cause: cause}
	}

	spec := SessionSpec{Base: sb.cfg.Image, MountMode: sb.cfg.Limits.DefaultMountMode}
	if rec != nil {
		spec = rec.Spec
	} else if carriedSpec != nil {
		// The persisted record is the authority when one exists; in memory is the next
		// best thing, and better than starting from nothing.
		spec = *carriedSpec
	}
	if opt.Image != "" {
		spec.Base = opt.Image
	}
	if len(opt.Expose) > 0 {
		spec.Expose = append([]PortMap(nil), opt.Expose...)
	}
	env, prov, err := sb.newEnvironment(ctx, id, spec, spec.Expose)
	if err != nil {
		return nil, err
	}
	sess := sb.adopt(id, env, spec)
	// Adopt mints a lineage for reporting; the provenance the creator returned is what
	// the replay decision uses, so the two cannot drift apart.
	sess.mu.Lock()
	sess.lineage, sess.inherited, sess.stateSource = prov.lineage, prov.upto, prov.source
	sess.mu.Unlock()
	// A spec with steps means this environment is a reconstruction, not the box that
	// ran them. Replaying is the difference between "resumed" and "empty and claiming
	// to be ready": without it every install and setup a workflow recorded is gone, and
	// the first failure gets attributed to the caller's code.
	// inheritedSpec is true when the spec above came from a session that already ran
	// these steps -- from the persisted record, or from a released session's in-memory
	// spec. Either way the environment in hand is new and has executed none of them.
	if (rec != nil || carriedSpec != nil) && len(spec.Steps) > 0 {
		rep := &RebuildReport{SessionID: id, StateSource: prov.source}
		if rerr := sb.replayRecorded(ctx, env, spec, rep, prov); rerr != nil {
			cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sb.cfg.Timeouts.Cleanup)
			_ = env.Close(cctx)
			cancel()
			sess.mu.Lock()
			sess.state = StateLost
			sess.mu.Unlock()
			return nil, rerr
		}
		sb.emit(ctx, Event{Kind: EventRebuilt, SessionID: id, Provider: sb.name, Detail: map[string]any{"replayed": len(rep.Replayed), "cached": rep.Cached, "resume": true}})
	}
	return sess, nil
}

// loadRecord fetches a persisted session record when a store is configured.
func (sb *Sandbox) loadRecord(ctx context.Context, id string) (*SessionRecord, bool, error) {
	if sb.cfg.Sessions == nil {
		return nil, false, nil
	}
	rec, ok, err := sb.cfg.Sessions.LoadSession(ctx, id)
	if err != nil {
		return nil, false, fmt.Errorf("sandbox: load session %q: %w", id, err)
	}
	if !ok {
		return nil, false, nil
	}
	recCopy := rec
	return &recCopy, true, nil
}

// pickImage resolves the image for a new environment.
func (sb *Sandbox) pickImage(opt sessionConfig, rec *SessionRecord) string {
	if opt.Image != "" {
		return opt.Image
	}
	if rec != nil && rec.Image != "" {
		return rec.Image
	}
	return sb.cfg.Image
}

// newEnvironment provisions the substrate for a session.
// boxProvenance records what a freshly created environment inherited. The function that
// creates the box is the only place that can say, because it is the only one that knows
// how the box was made.
//
// This exists so a replay decision is taken from a fact about the container rather than
// from a lookup that can miss it: an earlier version recovered the lineage by scanning
// the live sessions for a matching environment, and a rebuild nils the session's
// environment before replaying, so the scan always came back empty and the step cache
// silently never recorded anything (D-20).
type boxProvenance struct {
	lineage string
	// upto is the highest recorded step whose effects the box already carries; -1 for
	// a box rendered from the base image, which carries none.
	upto   int
	source string
}

// scratchBox describes a box rendered from the base image: it inherits nothing, so every
// recorded step has to run in it.
func scratchBox() boxProvenance {
	return boxProvenance{lineage: "gen:" + newRunID(), upto: -1, source: "base image"}
}

func (sb *Sandbox) newEnvironment(ctx context.Context, id string, spec SessionSpec, expose []PortMap) (Environment, boxProvenance, error) {
	sb.emit(ctx, Event{Kind: EventProvisioning, SessionID: id, Provider: sb.name, Isolation: sb.iso})
	pctx, cancel := context.WithTimeout(ctx, sb.cfg.Timeouts.Provision)
	defer cancel()
	env, err := sb.provider.NewEnvironment(pctx, EnvSpec{
		SessionID: id,
		Image:     spec.Base,
		Isolation: sb.iso,
		Spec:      spec,
		MountMode: sb.cfg.Limits.DefaultMountMode,
		Budget:    Budget{SetupTimeout: sb.cfg.Timeouts.Setup, ExecTimeout: sb.cfg.Timeouts.Exec, OutputLimit: sb.cfg.Limits.OutputMaxBytes, WorkspaceLimit: sb.cfg.Limits.WorkspaceMaxBytes},
		Limits:    sb.cfg.Limits,
		Labels:    labelAttrs(sb.cfg.Attrs),
		Expose:    append([]PortMap(nil), expose...),
	})
	if err != nil {
		// The remedy has to be something the caller can actually do from inside a Go
		// program: no sandbox CLI ships today, so pointing at one would turn an
		// actionable hint into a dead end.
		fix := "run Sandbox.Diagnose(ctx), or hno sandbox doctor from a shell, to compare the provider, isolation floor, and capabilities before retrying"
		return nil, boxProvenance{}, &ProvisionError{Provider: sb.name, Stage: "create environment", Cause: err, Fix: fix}
	}
	sb.emit(ctx, Event{Kind: EventReady, SessionID: id, Provider: sb.name, Isolation: sb.iso})
	// The provider rendered this box from spec.Base. There is no path today in which
	// newEnvironment restores a snapshot, so the honest answer is that it carries
	// nothing; a state-inheriting creation must return its own provenance here.
	return env, scratchBox(), nil
}

// adopt registers a live environment as a session owned by this handle.
func (sb *Sandbox) adopt(id string, env Environment, spec SessionSpec) *Session {
	return sb.adoptWorkspace(id, env, spec, nil)
}

// adoptWorkspace registers a session with its declared workspace.
func (sb *Sandbox) adoptWorkspace(id string, env Environment, spec SessionSpec, ws *WorkspaceSource) *Session {
	cap := 1
	if sb.cfg.Limits.SessionMaxConcurrentRuns > 1 {
		cap = sb.cfg.Limits.SessionMaxConcurrentRuns
	}
	sess := &Session{
		sb:  sb,
		id:  id,
		env: env,
		// Overwritten by the creator's provenance wherever that is known; these are the
		// safe values for a box this handle has not traced (D-20).
		lineage:     "gen:" + newRunID(),
		inherited:   -1,
		stateSource: "base image",
		spec:        spec.Clone(),
		state:       StateReady,
		image:       spec.Base,
		isolation:   sb.iso,
		provider:    sb.name,
		workspace:   cloneSource(ws),
		lock:        make(chan struct{}, cap),
		lastSeen:    time.Now(),
		// The absolute lifetime is measured from here, not from the last activity:
		// Status().ExpiresAt used to take lastSeen for both, so a busy session could
		// never reach its absolute deadline at all.
		createdAt: time.Now(),
	}
	sb.mu.Lock()
	sb.sessions[id] = sess
	sb.mu.Unlock()
	sb.persistSession(sess)
	return sess
}

// forget drops a released session from the handle registry.
func (sb *Sandbox) forget(id string) {
	sb.mu.Lock()
	delete(sb.sessions, id)
	sb.mu.Unlock()
}

// persistSession writes the session record when a store is configured.
func (sb *Sandbox) persistSession(sess *Session) error {
	if sb.cfg.Sessions == nil {
		return nil
	}
	sess.mu.Lock()
	rec := SessionRecord{
		ID:           sess.id,
		Provider:     sess.provider,
		Image:        sess.image,
		Isolation:    sess.isolation,
		Spec:         sess.spec.Clone(),
		CreatedAt:    UnixTime{Seconds: sess.createdAtLocked()},
		LastActiveAt: UnixTime{Seconds: sess.lastSeen.Unix()},
		TTLSeconds:   int64(sb.cfg.Timeouts.SessionTTL.Seconds()),
	}
	sess.mu.Unlock()
	if err := sb.cfg.Sessions.SaveSession(context.Background(), rec); err != nil {
		return fmt.Errorf("sandbox: persist session %q: %w", sess.id, err)
	}
	return nil
}

// Close releases every session and the provider. The context bounds teardown
// only: cancellation never skips cleanup, which is the difference between a
// slow shutdown and leaking a VM (§5.3).
func (sb *Sandbox) Close() error {
	return sb.CloseCtx(context.Background())
}

// CloseCtx closes with an explicit teardown budget.
func (sb *Sandbox) CloseCtx(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, sb.cfg.Timeouts.Cleanup)
	defer cancel()
	var errs []error
	sb.mu.Lock()
	sessions := make([]*Session, 0, len(sb.sessions))
	for _, s := range sb.sessions {
		sessions = append(sessions, s)
	}
	sb.sessions = map[string]*Session{}
	sb.closeOnce.Do(func() { sb.closed = true })
	sb.mu.Unlock()

	for _, s := range sessions {
		if err := s.Release(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	if sweeper, ok := sb.provider.(LeaseSweeper); ok && sb.caps.Has(CapSession) {
		orphans, serr := sweeper.Sweep(ctx)
		if serr != nil || len(orphans) > 0 {
			errs = append(errs, &CleanupError{Provider: sb.name, Residuals: orphans, SweeperID: "on-close"})
		}
	}
	if err := sb.provider.Close(ctx); err != nil {
		errs = append(errs, fmt.Errorf("sandbox: close provider: %w", err))
	}
	sb.emit(ctx, Event{Kind: EventClosed, Provider: sb.name})
	return errors.Join(errs...)
}

// labelAttrs converts caller attributes into provider labels, dropping values
// that are not strings so the engine never receives a surprise type.
func labelAttrs(attrs map[string]any) map[string]string {
	if len(attrs) == 0 {
		return nil
	}
	out := make(map[string]string, len(attrs))
	for k, v := range attrs {
		if s, ok := v.(string); ok {
			out[k] = s
		}
	}
	return out
}

// newRunID produces a short, sortable identifier.
func newRunID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("run-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// fixOf extracts a remediation command from an error when it knows one.
func fixOf(err error) string {
	var f fixer
	if errors.As(err, &f) {
		fixes := f.Remediation()
		if len(fixes) > 0 {
			return fixes[0]
		}
	}
	return ""
}

// attemptsOf builds Attempt records for a rejected allow list.
func attemptsOf(names []string) []Attempt {
	out := make([]Attempt, 0, len(names))
	for _, n := range names {
		out = append(out, Attempt{Provider: n, Reason: "not registered", Fix: "import the provider package for " + n})
	}
	return out
}

// hashOf is a compact non-cryptographic digest used for payload identity.
func hashOf(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	h := fnv.New64a()
	_, _ = h.Write(data)
	return fmt.Sprintf("%016x", h.Sum64())
}

// appendAttempt records why one candidate was rejected.
//
// When the rejection is itself an UnavailableError from the provider, its
// attempts are spliced in rather than dumped as a string. Re-serialising the
// inner error produced messages where the same daemon sentence appeared three
// times and the same fix twice, which a reader has to parse before noticing the
// one fact that mattered; a provider's diagnosis is structure, not prose.
func appendAttempt(tried []Attempt, name string, err error) []Attempt {
	var nested *UnavailableError
	if errors.As(err, &nested) {
		if len(nested.Tried) > 0 {
			for _, a := range nested.Tried {
				if a.Provider == "" {
					a.Provider = name
				}
				// A provider may fill Fixes without Fix. The rendered attempt only
				// shows Fix, so deriving it keeps the remedy visible after splicing.
				if a.Fix == "" {
					a.Fix = firstString(a.Fixes)
				}
				if a.Fix == "" {
					a.Fix = firstString(nested.Fixes)
				}
				tried = append(tried, a)
			}
			return tried
		}
		if nested.Reason != "" {
			return append(tried, Attempt{Provider: name, Reason: nested.Reason, Fix: firstString(nested.Fixes)})
		}
	}
	return append(tried, Attempt{Provider: name, Reason: errString(err), Fix: fixOf(err)})
}

// firstString returns the first non-empty value.
func firstString(in []string) string {
	for _, s := range in {
		if s != "" {
			return s
		}
	}
	return ""
}

// createdAtLocked is the session's absolute-lifetime anchor. Callers hold s.mu, like
// every other ...Locked helper here: persistSession already holds the session lock, so
// a locking accessor would self-deadlock on a mutex Go does not make reentrant.
func (s *Session) createdAtLocked() int64 {
	if s.createdAt.IsZero() {
		return s.lastSeen.Unix()
	}
	return s.createdAt.Unix()
}
