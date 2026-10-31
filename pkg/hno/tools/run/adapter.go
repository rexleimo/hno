package run

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/rexleimo/agno-go/pkg/hno/sandbox"
)

// This file is the compatibility layer between the frozen legacy Executor contract
// (docs/design/sandbox-runtime-architecture.html D-10) and the sandbox runtime in
// pkg/hno/sandbox. It is additive by design: nothing here changes how podman, docker, or
// e2b executors behave for existing callers, and no caller has to adopt it.
//
// The legacy shape cannot express everything the new runtime can do, and it promises some
// things the new runtime no longer does the same way. Where a Spec asks for something the
// sandbox cannot honour as stated, the adapter refuses with an error that says what to do
// instead. Silently reinterpreting a request is how a compatibility layer ends up promising
// a read-write mount and delivering a snapshot, or promising 2 GiB and enforcing 512 MiB.

// ErrSandboxUnsupported marks a Spec field whose meaning cannot be preserved on the sandbox
// runtime. It is a configuration error, not a runtime failure: the same Spec works on the
// legacy executors, so the caller decides whether to migrate or keep the old path.
var ErrSandboxUnsupported = errors.New("sandbox adapter: field not representable on the sandbox runtime")

// ErrSandboxLimitMismatch marks a Spec that asks for resource limits the adapter was not
// constructed to enforce.
var ErrSandboxLimitMismatch = errors.New("sandbox adapter: resource limits differ from the handle's")

// SandboxConfig configures the adapter. It exists because the legacy Config has no fields
// for the choices the sandbox runtime needs, and inventing them inside Config would change
// a frozen struct.
type SandboxConfig struct {
	// Config selects the backend. Backends the sandbox runtime cannot provide are refused
	// rather than silently mapped to another engine.
	Config
	// Limits are the resource ceilings the handle enforces. Per-run limits cannot be
	// expressed by the sandbox API, so a Spec asking for anything other than these (or the
	// zero value, which means "use these") is rejected with ErrSandboxLimitMismatch.
	// Zero-value Limits.MemoryMB/PidsLimit fall back to the legacy defaults.
	Limits sandbox.Limits
	// Options are appended last, so a caller can override provider registration, isolation,
	// or timeouts without this file growing a knob per sandbox option.
	Options []sandbox.Option
	// ExecTimeout bounds a run whose Spec leaves Timeout unset. Zero is DefaultTimeout,
	// matching the legacy executor.
	ExecTimeout time.Duration
	// Open is the sandbox constructor. Tests substitute it; production leaves it nil and
	// gets sandbox.Open.
	Open func(ctx context.Context, opts ...sandbox.Option) (*sandbox.Sandbox, error)
}

// NewSandboxExecutor returns an Executor backed by pkg/hno/sandbox.
//
// It probes and binds a provider eagerly, so a host with no engine fails here with
// ErrNoProvider rather than on the first run: callers already treat that sentinel as an
// execution denial, and discovering it mid-workflow is worse than discovering it at startup.
func NewSandboxExecutor(cfg SandboxConfig) (Executor, error) {
	switch cfg.Backend {
	case "", "auto", "docker":
		// The sandbox registry ships one container provider, which speaks both the docker
		// and the Apple container dialect internally. "auto" and "docker" therefore mean
		// the same engine here, not a guess between two.
	case "podman", "e2b":
		return nil, fmt.Errorf("%w: backend %q has no sandbox provider in this build; keep NewExecutor for that path", ErrSandboxUnsupported, cfg.Backend)
	default:
		return nil, fmt.Errorf("%w %q: %w", ErrUnsupportedBackend, cfg.Backend, ErrSandboxUnsupported)
	}

	limits := cfg.Limits
	if limits.MemoryMB == 0 {
		limits.MemoryMB = DefaultMemoryLimit / (1 << 20)
	}
	if limits.PidsLimit == 0 {
		limits.PidsLimit = DefaultPidLimit
	}
	if limits.OutputMaxBytes == 0 {
		limits.OutputMaxBytes = DefaultOutputLimit
	}

	image := cfg.Image
	if image == "" {
		image = "agent-runner:1"
	}
	// The legacy default deadline becomes the handle's exec timeout, so a Spec with no
	// Timeout still gets 30s like it did before, and a timeout error can name it.
	execTimeout := cfg.ExecTimeout
	if execTimeout <= 0 {
		execTimeout = DefaultTimeout
	}

	opts := []sandbox.Option{
		sandbox.WithProvider("docker"),
		sandbox.WithAllowedProviders("docker"),
		sandbox.WithImage(image),
		sandbox.WithLimits(limits),
		// The legacy executor denies network unconditionally: NetworkPolicy has exactly one
		// value and validateSpec rejects anything else. Carrying that over keeps the
		// compatibility layer from opening an egress path the old code never allowed.
		sandbox.WithEgress(sandbox.Egress{Mode: sandbox.EgressNone}),
		sandbox.WithMinimumIsolation(sandbox.IsolationContainer),
	}
	opts = append(opts, sandbox.WithTimeouts(sandbox.Timeouts{Exec: execTimeout}))
	opts = append(opts, cfg.Options...)

	open := cfg.Open
	if open == nil {
		open = sandbox.Open
	}
	sb, err := open(context.Background(), opts...)
	if err != nil {
		if isNoProvider(err) {
			return nil, fmt.Errorf("%w: %v", ErrNoProvider, unwrapUnavailableReason(err))
		}
		return nil, err
	}
	// Read the ceilings back from the handle rather than keeping the ones computed above:
	// caller Options are appended after the adapter's own, so they can replace Limits, image,
	// or timeout. A compatibility layer that refuses based on what it *meant* to configure
	// while the engine runs with something else is worse than no adapter at all -- the number
	// in the error would be a fiction.
	inForce := sb.Status()
	return &sandboxExecutor{
		sb:          sb,
		limits:      inForce.Limits,
		execTimeout: execTimeout,
		image:       inForce.Image,
	}, nil
}

// sandboxExecutor adapts Executor onto a sandbox handle.
type sandboxExecutor struct {
	sb *sandbox.Sandbox
	// limits and execTimeout record what the handle was actually told, so a refusal or a
	// timeout message quotes a number in force rather than one the caller hoped for.
	limits      sandbox.Limits
	execTimeout time.Duration
	image       string
}

// Run executes one Spec.
//
// The legacy contract returns a Result even when it returns an error, so callers can read
// partial output and TimedOut; the adapter keeps that shape.
func (e *sandboxExecutor) Run(ctx context.Context, spec Spec) (Result, error) {
	// Legacy validation first: its error strings are part of what existing callers see, and
	// a Spec this package would have rejected must not reach the sandbox layer at all.
	if err := validateSpec(spec); err != nil {
		return Result{}, err
	}
	if spec.Workspace != "" {
		return Result{}, fmt.Errorf("%w: Spec.Workspace %q was a read-write host bind mount, and the sandbox runtime stages a snapshot in and copies declared outputs out; use sandbox.Project with RunProject, or a session with WithWorkspace, and declare which outputs come back", ErrSandboxUnsupported, spec.Workspace)
	}
	if err := e.checkLimits(spec); err != nil {
		return Result{}, err
	}

	req := sandbox.Request{
		Runtime: spec.Runtime,
		Code:    spec.Code,
		Env:     append([]string(nil), spec.Env...),
	}
	opts := []sandbox.RunOption{
		sandbox.WithRunEgress(sandbox.Egress{Mode: sandbox.EgressNone}),
	}
	if spec.Timeout > 0 {
		opts = append(opts, sandbox.WithTimeout(spec.Timeout))
	}
	if spec.OutputLimit > 0 {
		opts = append(opts, sandbox.WithOutputLimit(spec.OutputLimit))
	}

	rep, err := e.sb.Run(ctx, req, opts...)
	return mapRunOutcome(rep, err, spec, e.execTimeout)
}

// mapRunOutcome turns one sandbox result into the legacy pair. It is separated from Run so
// the shapes that a provider can hand back -- including the report-less one -- are testable
// without an engine, which is the only way the TimedOut flag's own branch stays honest.
func mapRunOutcome(rep *sandbox.Report, err error, spec Spec, execTimeout time.Duration) (Result, error) {
	res := Result{}
	if rep != nil {
		res = Result{
			ExitCode: rep.ExitCode,
			Stdout:   rep.Stdout,
			Stderr:   rep.Stderr,
			Duration: rep.Duration,
			TimedOut: rep.TimedOut,
		}
	}
	if err == nil {
		return res, nil
	}
	if rep == nil {
		// No report means nothing was observed, and 0 is the code for success. A caller that
		// reads ExitCode without checking the error would record a killed or broken run as a
		// clean one, so the absence is reported as -1, matching what the sandbox core uses.
		res.ExitCode = -1
	}
	if res.TimedOut || errors.Is(err, context.DeadlineExceeded) {
		// The legacy shape signals a timeout with both the flag and a wrapped ErrTimeout,
		// and callers test either. A provider that returns no report at all would otherwise
		// lose the flag exactly when it matters most: the run was killed with nothing read
		// back.
		res.TimedOut = true
		return res, fmt.Errorf("%w after %s: %v", ErrTimeout, orDefault(spec.Timeout, execTimeout), err)
	}
	return res, err
}

// Close releases the handle.
func (e *sandboxExecutor) Close() error {
	if e == nil || e.sb == nil {
		return nil
	}
	return e.sb.Close()
}

// checkLimits refuses a Spec whose resource asks are not what this handle enforces.
//
// The sandbox API applies CPU, memory and process ceilings when the handle is opened, so a
// per-run number cannot be honoured. Accepting it anyway would be the worse failure: the
// caller would believe a 2 GiB job was bounded to 2 GiB when the engine was told 512 MiB, or
// that a tighter cap was applied when it was not.
func (e *sandboxExecutor) checkLimits(spec Spec) error {
	if spec.MemoryLimit != 0 {
		want := e.limits.MemoryMB * (1 << 20)
		if spec.MemoryLimit != want {
			return fmt.Errorf("%w: Spec.MemoryLimit %d bytes, this executor enforces %d bytes (%d MiB); construct it with SandboxConfig.Limits.MemoryMB to change", ErrSandboxLimitMismatch, spec.MemoryLimit, want, e.limits.MemoryMB)
		}
	}
	if spec.PidLimit != 0 && int64(spec.PidLimit) != int64(e.limits.PidsLimit) {
		return fmt.Errorf("%w: Spec.PidLimit %d, this executor enforces %d; construct it with SandboxConfig.Limits.PidsLimit to change", ErrSandboxLimitMismatch, spec.PidLimit, e.limits.PidsLimit)
	}
	return nil
}

func orDefault(d time.Duration, fallback time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return fallback
}

func isNoProvider(err error) bool {
	var un *sandbox.UnavailableError
	if errors.As(err, &un) {
		return true
	}
	return false
}

// unwrapUnavailableReason quotes the engine's own words rather than the whole error, so
// ErrNoProvider stays one line even when several providers were tried.
func unwrapUnavailableReason(err error) string {
	var un *sandbox.UnavailableError
	if errors.As(err, &un) {
		if un.Reason != "" {
			return un.Reason
		}
	}
	return err.Error()
}
