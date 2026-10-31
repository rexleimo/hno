package sandbox

import (
	"fmt"
	"time"
)

// Limits are the hard numbers the core enforces before an engine ever sees a
// request. Every limit produces a typed error with the offending number in it,
// because "it failed" is not actionable (§5.2).
type Limits struct {
	// InlineFileMaxBytes bounds Request.Files entries. Project-scale data does
	// not belong inline (§13.4).
	InlineFileMaxBytes int64
	// OutputMaxBytes bounds captured stdout+stderr per run; beyond it the
	// report says Truncated instead of lying about silence.
	OutputMaxBytes int64
	// WorkspaceMaxBytes bounds materialised input.
	WorkspaceMaxBytes int64
	// ArtifactMaxBytes bounds one artefact, ArtifactTotalMaxBytes the whole set.
	ArtifactMaxBytes      int64
	ArtifactTotalMaxBytes int64
	// EnvMaxEntries bounds declared environment entries.
	EnvMaxEntries int
	// CPUCount caps the processors an environment may use. Zero means the engine
	// default, which the core never guesses upward: a provider that needs a number
	// either gets one from the profile or leaves the engine alone.
	CPUCount int
	// MemoryMB caps resident memory per environment. Zero means engine default.
	// Enforced by the engine, not by counting inside the sandbox, since a guest
	// that reports its own usage is not a limit (§16.1).
	MemoryMB int64
	// PidsLimit caps processes, which is what stops a fork bomb from consuming
	// the engine's process table. Zero means engine default.
	PidsLimit int
	// DiskMB caps the writable workspace. Zero means engine default.
	DiskMB int64
	// SessionMaxConcurrentRuns bounds overlapping Exec per session when the
	// lock mode allows parallelism.
	SessionMaxConcurrentRuns int
	// SnapshotKeep bounds retained checkpoints per session (D-14).
	SnapshotKeep int
	// SnapshotTTL bounds how long an unattached checkpoint may live.
	SnapshotTTL time.Duration
	// DefaultMountMode is applied when a request does not declare one.
	DefaultMountMode MountMode
	// Overflow is the default sink overflow policy.
	Overflow OverflowPolicy
}

// DefaultLimits is the profile-independent floor. It is deliberately conservative:
// a run that needs more says so in WithLimits, the core never guesses upward.
func DefaultLimits() Limits {
	return Limits{
		InlineFileMaxBytes:       1 << 20, // 1 MiB inline; more is a workspace
		OutputMaxBytes:           8 << 20,
		WorkspaceMaxBytes:        2 << 30,
		ArtifactMaxBytes:         64 << 20,
		ArtifactTotalMaxBytes:    256 << 20,
		EnvMaxEntries:            128,
		SessionMaxConcurrentRuns: 1, // D-06: serial by default
		SnapshotKeep:             4,
		SnapshotTTL:              time.Hour,
		DefaultMountMode:         CopySnapshot, // D-12
		Overflow:                 OverflowDrop,
	}
}

// Profile is a named preset so "production" is one word instead of fifteen
// options that can drift apart between services.
type Profile uint8

const (
	// ProfileUnset leaves everything at the resolved defaults.
	ProfileUnset Profile = iota
	// ProfileDev favours iteration speed: syscall-filtered floor, probe cache
	// on, drift detection only.
	ProfileDev
	// ProfileCI favours reproducibility: micro-VM floor, strict lockdown, no
	// fallback.
	ProfileCI
	// ProfileProduction favours isolation and hard failure: micro-VM required,
	// FailFast, no host-visible mount.
	ProfileProduction
)

// String names the profile for logs.
func (p Profile) String() string {
	switch p {
	case ProfileDev:
		return "dev"
	case ProfileCI:
		return "ci"
	case ProfileProduction:
		return "production"
	default:
		return "unset"
	}
}

// Config is the fully resolved configuration of a Sandbox handle. It is
// produced only by resolve() so no call site can hold a half-filled config.
type Config struct {
	ProfileName   Profile
	Provider      string
	Formation     Formation
	Image         string
	MinIsolation  Isolation
	Allowed       []string // provider allow-list; empty = registry order
	Egress        Egress
	Timeouts      Timeouts
	Limits        Limits
	Retry         RetryBudget
	RetryMode     RetryMode
	Lock          LockMode
	Storm         StormGuardConfig
	Breaker       Breaker
	BreakerPolicy BreakerPolicy
	Policies      []Policy
	Observers     []Observer
	Sinks         []SinkBinding
	Journal       JournalStore
	Sessions      SessionStore
	Artifacts     BlobStore
	// StepCache caches replayed spec steps. Reusing the journal contract keeps
	// the core from inventing a second storage interface (§17.2).
	StepCache JournalStore
	// audit is the compliance sink; it is configuration, not an observer.
	audit         AuditSink
	AllowFallback bool
	// FallbackExplicit records that a caller (not a profile) asked for fallback,
	// which raises the observer requirement from "drop it" to "refuse it".
	FallbackExplicit bool
	Fallback         []Isolation
	AutoCheckpoint   bool
	StrictLockdown   bool
	Attrs            map[string]any
}

// Timeouts are the handle-level knobs. They always bound, never disable; a
// zero value resolves to the documented default.
type Timeouts struct {
	Probe     time.Duration // default 5s, cached 60s (§16.5)
	Connect   time.Duration // default 3s
	Provision time.Duration // default 120s: image pull plus boot
	Exec      time.Duration // default 30s
	Setup     time.Duration // boot budget inside Exec (§16.2)
	Collect   time.Duration // default 30s
	Drain     time.Duration // post-kill output rescue, default 200ms
	// Idle releases a session this long after its last activity. Set it negative for no
	// idle limit.
	Idle time.Duration // default 15m
	// SessionTTL is the absolute lifetime, measured from creation and independent of how
	// busy the session stays. Set it negative for no absolute limit.
	SessionTTL   time.Duration // default 30m (D-09)
	Cleanup      time.Duration // default 10s
	ApprovalHold time.Duration // default 5m before a suspended run expires
}

// DefaultTimeouts returns the documented floors.
func DefaultTimeouts() Timeouts {
	return Timeouts{
		Probe:        5 * time.Second,
		Connect:      3 * time.Second,
		Provision:    120 * time.Second,
		Exec:         30 * time.Second,
		Setup:        30 * time.Second,
		Collect:      30 * time.Second,
		Drain:        200 * time.Millisecond,
		Idle:         15 * time.Minute,
		SessionTTL:   30 * time.Minute,
		Cleanup:      10 * time.Second,
		ApprovalHold: 5 * time.Minute,
	}
}

// Option configures a handle. Options are applied in order, so WithLimits after
// WithProfile wins: profile first, exceptions after.
type Option func(*Config)

// WithProfile applies a named preset before any other option.
func WithProfile(p Profile) Option { return func(c *Config) { c.ProfileName = p; applyProfile(c, p) } }

// WithProvider names the engine to use. It never discovers engines from PATH:
// what runs here is exactly what was registered (D-03).
func WithProvider(name string) Option { return func(c *Config) { c.Provider = name } }

// WithAllowedProviders restricts selection to a list, keeping a tenant pinned
// to an approved engine while the registry stays global.
func WithAllowedProviders(names ...string) Option {
	return func(c *Config) { c.Allowed = append([]string(nil), names...) }
}

// WithFormation chooses how the engine is attached.
func WithFormation(f Formation) Option { return func(c *Config) { c.Formation = f } }

// WithImage names the environment image or runtime reference.
func WithImage(image string) Option { return func(c *Config) { c.Image = image } }

// WithMinimumIsolation sets a floor that must be met. Higher is accepted; lower
// is an error, and the achieved level is recorded in every report (§5.3).
func WithMinimumIsolation(min Isolation) Option {
	return func(c *Config) {
		c.MinIsolation = min
		if min == IsolationMicroVM || min == IsolationMicroVMHosted {
			c.AllowFallback = false
		}
	}
}

// WithFallbackTier permits a downgrade to the listed levels when the primary
// provider is unavailable. Off by default; enabling it without an Observer is
// rejected by Validate (D-13).
func WithFallbackTier(levels ...Isolation) Option {
	return func(c *Config) {
		c.AllowFallback = true
		c.FallbackExplicit = true
		c.Fallback = append([]Isolation(nil), levels...)
	}
}

// WithEgress sets the handle-wide network posture.
func WithEgress(e Egress) Option { return func(c *Config) { c.Egress = e } }

// WithLimits replaces individual limit fields.
func WithLimits(l Limits) Option { return func(c *Config) { c.Limits = l } }

// WithTimeouts replaces the timeout knobs.
func WithTimeouts(t Timeouts) Option { return func(c *Config) { c.Timeouts = t } }

// WithRetryBudget bounds attempts, per-attempt and total wall clock (§16.2).
func WithRetryBudget(b RetryBudget) Option { return func(c *Config) { c.Retry = b } }

// WithRetryMode selects the default retry mode (D-11).
func WithRetryMode(m RetryMode) Option { return func(c *Config) { c.RetryMode = m } }

// WithLockMode chooses session concurrency (D-06).
func WithLockMode(m LockMode) Option { return func(c *Config) { c.Lock = m } }

// WithStormGuard enables the retry-storm guard (§16.5).
func WithStormGuard(cfg StormGuardConfig) Option { return func(c *Config) { c.Storm = cfg } }

// WithBreaker installs a provider circuit breaker.
func WithBreaker(b Breaker) Option { return func(c *Config) { c.Breaker = b } }

// WithPolicy appends a policy. Order matters: the first deny wins.
func WithPolicy(p ...Policy) Option { return func(c *Config) { c.Policies = append(c.Policies, p...) } }

// WithObserver appends an observer.
func WithObserver(o ...Observer) Option {
	return func(c *Config) { c.Observers = append(c.Observers, o...) }
}

// WithSink appends an output consumer with its own overflow contract.
func WithSink(name string, s Sink, opts ...SinkOption) Option {
	return func(c *Config) {
		b := SinkBinding{Name: name, Sink: s, Buffered: 256, OnOverflow: OverflowDrop}
		for _, o := range opts {
			o(&b)
		}
		c.Sinks = append(c.Sinks, b)
	}
}

// SinkOption tunes one sink binding.
type SinkOption func(*SinkBinding)

// WithBuffer sets the per-sink queue depth.
func WithBuffer(n int) SinkOption { return func(b *SinkBinding) { b.Buffered = n } }

// WithOverflow sets the per-sink overflow policy.
func WithOverflow(p OverflowPolicy) SinkOption { return func(b *SinkBinding) { b.OnOverflow = p } }

// WithJournal installs the idempotency store. Delivery promises require it.
func WithJournal(j JournalStore) Option { return func(c *Config) { c.Journal = j } }

// WithStepCache installs a store for replayed spec steps.
//
// Its reach is narrower than it looks, and deliberately so (D-20): a hit may skip a
// step only when the environment being built carries the state that step produced,
// which is decided from the box's state lineage and its inheritance watermark. A box
// rendered from the base image inherits nothing, so RebuildFromSpec always replays it
// in full -- correct, and the cost the mode's name already promises. The cache earns
// its keep where state really is carried: a clone of a session, or a restore to a
// checkpoint that already covered the step.
func WithStepCache(c JournalStore) Option { return func(cfg *Config) { cfg.StepCache = c } }

// WithSessionStore installs session persistence for restart recovery (§15).
func WithSessionStore(s SessionStore) Option { return func(c *Config) { c.Sessions = s } }

// WithArtifactStore installs the blob store artefacts can be persisted to.
func WithArtifactStore(b BlobStore) Option { return func(c *Config) { c.Artifacts = b } }

// WithAutoCheckpoint opts into snapshots at phase boundaries (D-14).
func WithAutoCheckpoint(on bool) Option { return func(c *Config) { c.AutoCheckpoint = on } }

// WithStrictLockdown turns environment changes outside the recorded spec into
// refusals instead of warnings (§17.3).
func WithStrictLockdown(on bool) Option { return func(c *Config) { c.StrictLockdown = on } }

// WithAttrs attaches caller metadata visible to policies.
func WithAttrs(kv map[string]any) Option {
	return func(c *Config) {
		if c.Attrs == nil {
			c.Attrs = map[string]any{}
		}
		for k, v := range kv {
			c.Attrs[k] = v
		}
	}
}

// applyProfile folds a preset into the config without clobbering fields the
// caller will set afterwards.
func applyProfile(c *Config, p Profile) {
	switch p {
	case ProfileDev:
		c.MinIsolation = IsolationContainer
		c.StrictLockdown = false
		c.Lock = LockSerial
		c.AllowFallback = true
		c.Fallback = []Isolation{IsolationSyscallFiltered, IsolationContainer}
		c.Egress = Egress{Mode: EgressNone}
		c.RetryMode = RebuildFromSpec
	case ProfileCI:
		c.MinIsolation = IsolationSyscallFiltered
		c.StrictLockdown = true
		c.Lock = LockSerial
		c.AllowFallback = false
		c.Egress = Egress{Mode: EgressNone}
		c.RetryMode = RebuildFromSpec
	case ProfileProduction:
		c.MinIsolation = IsolationMicroVM
		c.StrictLockdown = true
		c.Lock = LockSerial
		c.AllowFallback = false
		c.Egress = Egress{Mode: EgressNone}
		c.RetryMode = RebuildFromSpec
		c.AutoCheckpoint = false
	default:
	}
}

// resolve fills defaults, applies nothing destructive, and returns the config
// ready to use. It is the only path to a populated Config.
func (c *Config) resolve() error {
	if c.Timeouts == (Timeouts{}) {
		c.Timeouts = DefaultTimeouts()
	} else {
		def := DefaultTimeouts()
		if c.Timeouts.Probe == 0 {
			c.Timeouts.Probe = def.Probe
		}
		if c.Timeouts.Connect == 0 {
			c.Timeouts.Connect = def.Connect
		}
		if c.Timeouts.Provision == 0 {
			c.Timeouts.Provision = def.Provision
		}
		if c.Timeouts.Exec == 0 {
			c.Timeouts.Exec = def.Exec
		}
		if c.Timeouts.Setup == 0 {
			c.Timeouts.Setup = def.Setup
		}
		if c.Timeouts.Collect == 0 {
			c.Timeouts.Collect = def.Collect
		}
		if c.Timeouts.Drain == 0 {
			c.Timeouts.Drain = def.Drain
		}
		// A negative limit means "no limit" and normalises to the disabled zero rather
		// than to the default. Without it there was no way to ask for a session that
		// never ages: leaving the field alone was indistinguishable from not having
		// decided, which the defaults then overwrote.
		if c.Timeouts.Idle < 0 {
			c.Timeouts.Idle = 0
		} else if c.Timeouts.Idle == 0 {
			c.Timeouts.Idle = def.Idle
		}
		if c.Timeouts.SessionTTL < 0 {
			c.Timeouts.SessionTTL = 0
		} else if c.Timeouts.SessionTTL == 0 {
			c.Timeouts.SessionTTL = def.SessionTTL
		}
		if c.Timeouts.Cleanup == 0 {
			c.Timeouts.Cleanup = def.Cleanup
		}
		if c.Timeouts.ApprovalHold == 0 {
			c.Timeouts.ApprovalHold = def.ApprovalHold
		}
	}
	limits := DefaultLimits()
	if c.Limits.DefaultMountMode != 0 {
		limits.DefaultMountMode = c.Limits.DefaultMountMode
	}
	if c.Limits.Overflow != OverflowUnset {
		limits.Overflow = c.Limits.Overflow
	}
	preserve := c.Limits
	c.Limits = limits
	c.Limits = mergeLimits(c.Limits, preserve)
	if c.RetryMode == RetryUnset {
		c.RetryMode = RebuildFromSpec // D-11
	}
	if c.Lock == LockUnset {
		c.Lock = LockSerial // D-06
	}
	if c.Egress.Mode == EgressUnset {
		c.Egress.Mode = EgressNone // D-08 local default
	}
	if c.Formation == FormationUnset {
		c.Formation = FormationRemote // D-03
	}
	if c.Image == "" {
		c.Image = DefaultImage
	}
	if c.MinIsolation == IsolationUnknown {
		c.MinIsolation = IsolationContainer
	}
	if c.Retry.MaxAttempts == 0 {
		c.Retry.MaxAttempts = 2
	}
	if c.Retry.PerAttempt == 0 {
		c.Retry.PerAttempt = c.Timeouts.Setup + c.Timeouts.Exec
	}
	// Journal stays nil unless the caller installs one: an idempotency promise
	// backed by process memory is a lie across a restart, so the core refuses
	// the promise instead (§16.3).
	//
	// The step cache follows the same rule now. It used to be auto-installed with a
	// process-memory journal, which meant a default handle carried a cache that could
	// suppress a step in a container it knew nothing about -- measured as
	// cached=1 replayed=0 with an empty box, including across two different sessions
	// (D-20). Nothing is cached unless the caller installs a store, so the default
	// behaviour is the correct one: replay every recorded step.
	if c.Journal == nil {
		c.Journal = nil
	}
	for i := range c.Sinks {
		if c.Sinks[i].Buffered <= 0 {
			c.Sinks[i].Buffered = defaultSinkBuffer
		}
		if c.Sinks[i].OnOverflow == OverflowUnset {
			c.Sinks[i].OnOverflow = c.Limits.Overflow
		}
	}
	return c.Validate()
}

// mergeLimits keeps caller-provided fields over the defaults.
func mergeLimits(base, over Limits) Limits {
	// pick resolves one ceiling. A zero on either side means "not stated", and a
	// stated value may only tighten: a per-run option that widened a profile
	// limit would silently undo the profile's security decision (§5.3).
	pick := func(a, b int64) int64 {
		switch {
		case a == 0:
			return b
		case b == 0:
			return a
		case b < a:
			return b
		default:
			return a
		}
	}
	base.InlineFileMaxBytes = pick(base.InlineFileMaxBytes, over.InlineFileMaxBytes)
	base.OutputMaxBytes = pick(base.OutputMaxBytes, over.OutputMaxBytes)
	base.WorkspaceMaxBytes = pick(base.WorkspaceMaxBytes, over.WorkspaceMaxBytes)
	base.ArtifactMaxBytes = pick(base.ArtifactMaxBytes, over.ArtifactMaxBytes)
	base.ArtifactTotalMaxBytes = pick(base.ArtifactTotalMaxBytes, over.ArtifactTotalMaxBytes)
	base.EnvMaxEntries = int(pick(int64(base.EnvMaxEntries), int64(over.EnvMaxEntries)))
	base.SessionMaxConcurrentRuns = int(pick(int64(base.SessionMaxConcurrentRuns), int64(over.SessionMaxConcurrentRuns)))
	base.SnapshotKeep = int(pick(int64(base.SnapshotKeep), int64(over.SnapshotKeep)))
	base.CPUCount = int(pick(int64(base.CPUCount), int64(over.CPUCount)))
	base.MemoryMB = pick(base.MemoryMB, over.MemoryMB)
	base.PidsLimit = int(pick(int64(base.PidsLimit), int64(over.PidsLimit)))
	base.DiskMB = pick(base.DiskMB, over.DiskMB)
	if over.SnapshotTTL != 0 {
		base.SnapshotTTL = over.SnapshotTTL
	}
	return base
}

// DefaultImage is the runtime template used when no image is declared. It
// matches the image the v3 executor used, so a migration does not silently
// change the environment.
const DefaultImage = "agent-runner:1"

// Validate rejects configurations that would be unsafe in practice. It is
// exported because profiles are assembled from config files and must be
// checkable before opening a handle.
func (c *Config) Validate() error {
	if c.Egress.Mode == EgressOpen && (c.ProfileName == ProfileProduction || c.ProfileName == ProfileCI) {
		return fmt.Errorf("profile %s forbids open egress; declare an allowlist", c.ProfileName)
	}
	if c.AllowFallback && len(c.Observers) == 0 {
		if c.FallbackExplicit {
			// D-13: when a caller asked for fallback by name, refusing loudly is
			// right — a silent downgrade is the failure mode the floor prevents.
			return fmt.Errorf("fallback tiers require an observer so a downgrade is always reported; add WithObserver")
		}
		// A profile default silently loses its meaning without a reporter, so
		// the profile posture wins: no fallback (D-07 FailFast).
		c.AllowFallback = false
		c.Fallback = nil
	}
	if c.AllowFallback {
		for _, lvl := range c.Fallback {
			if lvl == IsolationUnknown {
				return fmt.Errorf("fallback tier list contains an unknown isolation level")
			}
			if c.MinIsolation != IsolationUnknown && lvl.Rank() < c.MinIsolation.Rank() {
				return fmt.Errorf("fallback tier %s is below the minimum isolation %s; raise the floor or drop the fallback", lvl, c.MinIsolation)
			}
		}
	}
	if c.Limits.InlineFileMaxBytes <= 0 || c.Limits.OutputMaxBytes <= 0 {
		return fmt.Errorf("inline file and output limits must be positive")
	}
	if c.Limits.SessionMaxConcurrentRuns < 1 {
		return fmt.Errorf("session concurrency must be at least 1")
	}
	if c.Lock == LockParallel && c.Limits.SessionMaxConcurrentRuns < 1 {
		return fmt.Errorf("parallel lock requires a concurrency allowance")
	}
	if c.AutoCheckpoint && c.Limits.SnapshotKeep < 1 {
		return fmt.Errorf("auto checkpointing requires snapshot retention of at least 1")
	}
	for i, p := range c.Policies {
		if p == nil {
			return fmt.Errorf("policy at index %d is nil", i)
		}
	}
	return nil
}
