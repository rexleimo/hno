package sandbox

import (
	"time"
)

// RunOption tunes one execution. Like every other lever in this package it can
// only tighten the resolved profile: an option that would widen egress, raise a
// limit, or lower isolation is rejected at resolve time (§5.3).
type RunOption func(*runOptions)

type runOptions struct {
	timeout         time.Duration
	setupTimeout    time.Duration
	env             []string
	workspace       *WorkspaceSource
	mountMode       MountMode
	egress          *Egress
	sinks           []SinkBinding
	runID           string
	caller          CallerInfo
	queueIfBusy     bool
	detachOnTimeout bool
	outputLimit     int64
	attrs           map[string]any
	lock            LockMode
	strict          bool
}

// WithTimeout bounds this run only.
func WithTimeout(d time.Duration) RunOption { return func(o *runOptions) { o.timeout = d } }

// WithSetupTimeout separates boot cost from execution cost (§16.2).
func WithSetupTimeout(d time.Duration) RunOption { return func(o *runOptions) { o.setupTimeout = d } }

// WithEnv adds KEY=VALUE entries for this run.
func WithEnv(kv ...string) RunOption { return func(o *runOptions) { o.env = append(o.env, kv...) } }

// WithWorkspace attaches a host directory to a one-shot run, using the
// profile's mount mode.
func WithWorkspace(source *WorkspaceSource) RunOption {
	return func(o *runOptions) { o.workspace = source }
}

// WithMountMode overrides the mount mode for this run, never below the profile
// default in restrictiveness.
func WithMountMode(m MountMode) RunOption { return func(o *runOptions) { o.mountMode = m } }

// WithRunEgress tightens the network posture for this run.
func WithRunEgress(e Egress) RunOption { return func(o *runOptions) { o.egress = &e } }

// WithRunSink adds a per-run output consumer. Handle-level WithSink stays the
// common case; per-run sinks exist for one caller that wants to watch (§5.3).
func WithRunSink(name string, s Sink, opts ...SinkOption) RunOption {
	return func(o *runOptions) {
		b := SinkBinding{Name: name, Sink: s, Buffered: 256, OnOverflow: OverflowDrop}
		for _, opt := range opts {
			opt(&b)
		}
		o.sinks = append(o.sinks, b)
	}
}

// WithRunID pins the run id so a detached run can be reattached by a known key.
func WithRunID(id string) RunOption { return func(o *runOptions) { o.runID = id } }

// WithCaller attributes the run for policy and audit.
func WithCaller(info CallerInfo) RunOption { return func(o *runOptions) { o.caller = info } }

// WithQueueIfBusy waits for the session lock instead of failing busy (§15).
func WithQueueIfBusy(wait bool) RunOption { return func(o *runOptions) { o.queueIfBusy = wait } }

// WithDetachOnTimeout keeps a timed-out run alive as a background handle
// (§15). Without it, timeout means kill.
func WithDetachOnTimeout(on bool) RunOption { return func(o *runOptions) { o.detachOnTimeout = on } }

// WithOutputLimit tightens the captured output bound for this run.
func WithOutputLimit(n int64) RunOption { return func(o *runOptions) { o.outputLimit = n } }

// WithRunAttrs attaches per-run metadata for policies and observers.
func WithRunAttrs(kv map[string]any) RunOption {
	return func(o *runOptions) {
		if o.attrs == nil {
			o.attrs = map[string]any{}
		}
		for k, v := range kv {
			o.attrs[k] = v
		}
	}
}

// WithLock overrides the session lock mode for this run, tightening only.
func WithLock(m LockMode) RunOption { return func(o *runOptions) { o.lock = m } }

// WithStrictPolicy marks the run as requiring policy approval even when the
// profile would allow it.
func WithStrictPolicy(on bool) RunOption { return func(o *runOptions) { o.strict = on } }

// resolvedRunOptions is the validated, profile-clamped outcome of applying
// RunOptions. Only this type is allowed to reach the engine.
type resolvedRunOptions struct {
	Budget          Budget
	Env             []string
	Workspace       *WorkspaceSource
	MountMode       MountMode
	Egress          Egress
	Sinks           []SinkBinding
	RunID           string
	Caller          CallerInfo
	QueueIfBusy     bool
	DetachOnTimeout bool
	Limits          Limits
	Attrs           map[string]any
	Lock            LockMode
	Strict          bool
}

// resolveRunOptions applies options and enforces the tighten-only rule (§6.3).
func resolveRunOptions(opts []RunOption, cfg *Config) resolvedRunOptions {
	o := runOptions{mountMode: cfg.Limits.DefaultMountMode, lock: cfg.Lock}
	for _, f := range opts {
		if f != nil {
			f(&o)
		}
	}
	limits := cfg.Limits
	if o.outputLimit > 0 && o.outputLimit < limits.OutputMaxBytes {
		limits.OutputMaxBytes = o.outputLimit
	}
	budget := Budget{ExecTimeout: cfg.Timeouts.Exec, SetupTimeout: cfg.Timeouts.Setup}
	// A per-run timeout may only shorten the profile's budget. Allowing a run to
	// extend it would let one caller hold a lease past the profile's cost ceiling
	// (§6.3 tightening rules apply to time as much as to bytes).
	if o.timeout > 0 && (budget.ExecTimeout == 0 || o.timeout < budget.ExecTimeout) {
		budget.ExecTimeout = o.timeout
	}
	if o.setupTimeout > 0 && (budget.SetupTimeout == 0 || o.setupTimeout < budget.SetupTimeout) {
		budget.SetupTimeout = o.setupTimeout
	}
	mount := o.mountMode
	if mount == 0 {
		mount = cfg.Limits.DefaultMountMode
	}
	// Tighten-only: MountShare is never granted by a per-run option unless the
	// profile already allowed it (D-12).
	if mount == MountShare && cfg.Limits.DefaultMountMode != MountShare {
		mount = cfg.Limits.DefaultMountMode
	}
	egress := cfg.Egress
	if o.egress != nil {
		egress = tightenEgress(cfg.Egress, *o.egress)
	}
	lock := o.lock
	if lock == LockUnset {
		lock = cfg.Lock
	}
	if cfg.Lock == LockSerial && lock == LockParallel {
		lock = LockSerial // a profile that forbids parallel work is not loosened per run
	}
	sinks := append(append([]SinkBinding(nil), cfg.Sinks...), o.sinks...)
	return resolvedRunOptions{
		Budget:          budget,
		Env:             append([]string(nil), o.env...),
		Workspace:       o.workspace,
		MountMode:       mount,
		Egress:          egress,
		Sinks:           sinks,
		RunID:           o.runID,
		Caller:          o.caller,
		QueueIfBusy:     o.queueIfBusy,
		DetachOnTimeout: o.detachOnTimeout,
		Limits:          limits,
		Attrs:           o.attrs,
		Lock:            lock,
		Strict:          o.strict || cfg.StrictLockdown,
	}
}

// tightenEgress returns the stricter of the profile posture and the requested
// one. Ordering: None < Allowlist < Open, so "open" can never replace a
// profile-level "none" (§6.3).
func tightenEgress(profile, requested Egress) Egress {
	rank := func(m EgressMode) int {
		switch m {
		case EgressNone:
			return 0
		case EgressAllowlist:
			return 1
		case EgressOpen:
			return 2
		default:
			return 1
		}
	}
	if rank(requested.Mode) >= rank(profile.Mode) {
		return profile
	}
	if len(requested.Allow) > 0 && requested.Mode == EgressAllowlist {
		// Intersect so a caller cannot widen the allowlist.
		allowed := intersect(profile.Allow, requested.Allow)
		if len(allowed) == 0 {
			return Egress{Mode: EgressNone}
		}
		return Egress{Mode: EgressAllowlist, Allow: allowed, SecretRefs: profile.SecretRefs}
	}
	return requested
}

// intersect keeps only entries present in both lists.
func intersect(a, b []string) []string {
	if len(a) == 0 {
		return append([]string(nil), b...)
	}
	set := make(map[string]bool, len(a))
	for _, v := range a {
		set[v] = true
	}
	var out []string
	for _, v := range b {
		if set[v] {
			out = append(out, v)
		}
	}
	return out
}
