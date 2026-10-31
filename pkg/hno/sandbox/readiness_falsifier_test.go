package sandbox_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rexleimo/agno-go/pkg/hno/sandbox"
	"github.com/rexleimo/agno-go/pkg/hno/sandbox/sandboxtest"
)

// TestReadyProbeIsNotSilentlyIgnored is the falsifier for the D-05 class at the API
// surface. Request.ReadyProbe is deep-copied by clone() and checked by Validate(), and
// before this file no code path read its URL, Port, Pattern, or Deadline. The
// documented contract was "start a service and wait until it is ready"; the implemented
// contract was "return immediately, whether or not anything came up".
func TestReadyProbeIsNotSilentlyIgnored(t *testing.T) {
	sb := openHandle(t)

	rep, err := sb.Run(context.Background(), sandbox.Request{
		Runtime:    "sh",
		Code:       "echo starting; echo server up",
		Background: true,
		ReadyProbe: &sandbox.ReadyProbe{Pattern: "will never appear", Deadline: 300 * time.Millisecond},
	})
	if err == nil {
		t.Fatalf("a background run whose probe never matched returned success (report=%p)\n"+
			"the probe is decorative: nothing waits on it", rep)
	}
	var re *sandbox.ReadinessError
	if !errors.As(err, &re) {
		t.Fatalf("want *sandbox.ReadinessError, got %T: %v", err, err)
	}
	if re.RunID == "" {
		t.Fatalf("the readiness failure must name the run: %+v", re)
	}
	if !strings.Contains(re.Probe.Pattern, "will never appear") {
		t.Fatalf("the error must echo the probe that failed so a caller can fix it: %+v", re.Probe)
	}
	if re.Remediation() == "" {
		t.Fatal("the readiness failure must carry a remedy")
	}
	// The class says the payload, not the platform, is at fault, so the retry layer
	// must not loop on it.
	if sandbox.ClassOf(err) != sandbox.ClassUserCode {
		t.Fatalf("readiness failure class = %v, want ClassUserCode", sandbox.ClassOf(err))
	}
}

// TestReadyProbePatternMatchedLetsTheRunProceed is the positive half: when the service
// really does come up, the probe must observe it and return promptly.
func TestReadyProbePatternMatchedLetsTheRunProceed(t *testing.T) {
	sb := openHandle(t)
	start := time.Now()
	if _, err := sb.Run(context.Background(), sandbox.Request{
		Runtime:    "sh",
		Code:       "echo starting",
		Background: true,
		ReadyProbe: &sandbox.ReadyProbe{Pattern: "bg 0", Deadline: 2 * time.Second},
	}); err != nil {
		t.Fatalf("a probe that matches must succeed: %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("the probe blocked %v after matching", d)
	}
}

// TestReadyProbeWithoutBackgroundIsRefused. A probe on a foreground run can never be
// evaluated, because the command itself blocks until exit: accepting the field was
// accepting a promise nobody could keep.
func TestReadyProbeWithoutBackgroundIsRefused(t *testing.T) {
	sb := openHandle(t)
	_, err := sb.Run(context.Background(), sandbox.Request{
		Runtime:    "sh",
		Code:       "echo hi",
		ReadyProbe: &sandbox.ReadyProbe{Pattern: "hi"},
	})
	if err == nil {
		t.Fatal("a ready probe on a foreground run was accepted; it can never be evaluated")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "background") {
		t.Fatalf("the refusal should say why the probe cannot work, got: %v", err)
	}
}

// TestPortProbeOnProberlessProviderIsRefused covers the other half of the lie. The
// memory provider supports background, so the run really is detached, but implements no
// ReadinessProber: it can start a service and cannot look inside it. Passing the probe
// there would be indistinguishable from ignoring it.
func TestPortProbeOnProberlessProviderIsRefused(t *testing.T) {
	sb := openHandle(t)
	_, err := sb.Run(context.Background(), sandbox.Request{
		Runtime:    "sh",
		Code:       "sleep 1",
		Background: true,
		ReadyProbe: &sandbox.ReadyProbe{Port: 8080, Deadline: 300 * time.Millisecond},
	})
	if err == nil {
		t.Fatal("a port probe on a provider that cannot inspect the guest returned success")
	}
	var up *sandbox.UnsupportedProbeError
	if !errors.As(err, &up) {
		t.Fatalf("want *sandbox.UnsupportedProbeError, got %T: %v", err, err)
	}
	if up.Remediation() == "" {
		t.Fatal("the refusal must say what to do instead")
	}
	if !strings.Contains(up.Remediation(), "Pattern") {
		t.Fatalf("the refusal should offer the condition the core can evaluate alone, got %q", up.Remediation())
	}
	if sandbox.ClassOf(err) != sandbox.ClassPermanent {
		t.Fatalf("a missing provider capability is not retryable, class=%v", sandbox.ClassOf(err))
	}
}

// TestInvalidProbePatternIsRefusedAtValidate. An uncompileable pattern used to be
// accepted and matched as a literal, which is a behaviour nobody asked for; the caller
// now hears about it where it is still cheap to fix.
func TestInvalidProbePatternIsRefusedAtValidate(t *testing.T) {
	err := sandbox.Request{
		Runtime:    "sh",
		Code:       "echo x",
		Background: true,
		ReadyProbe: &sandbox.ReadyProbe{Pattern: "([unclosed"},
	}.Validate()
	if err == nil {
		t.Fatal("an invalid probe regexp was accepted")
	}
	if !strings.Contains(err.Error(), "regexp") {
		t.Fatalf("want a regexp-specific complaint, got %v", err)
	}
}

// TestProbeDeadlineNegativeIsRefused: an inverted window would read as expired
// immediately or never, depending on the loop, so it is a bug shape worth refusing.
func TestProbeDeadlineNegativeIsRefused(t *testing.T) {
	err := sandbox.Request{
		Runtime:    "sh",
		Code:       "echo x",
		Background: true,
		ReadyProbe: &sandbox.ReadyProbe{Pattern: "x", Deadline: -time.Second},
	}.Validate()
	if err == nil {
		t.Fatal("a negative probe deadline was accepted")
	}
}

// TestProbeWithoutSinkStillMatchesPattern pins the lesson from the streaming audit: an
// extension point that only works when the caller configured extra machinery is itself a
// silent no-op. Readiness must be observable with the default run.
func TestProbeWithoutSinkStillMatchesPattern(t *testing.T) {
	sb := openHandle(t)
	if _, err := sb.Run(context.Background(), sandbox.Request{
		Runtime:    "sh",
		Code:       "echo x",
		Background: true,
		ReadyProbe: &sandbox.ReadyProbe{Pattern: "bg 1", Deadline: time.Second},
		// No WithRunSink, no Observer: the probe must still work.
	}); err != nil {
		t.Fatalf("probe without sinks: %v", err)
	}
}

// TestPortProbeWaitsUntilTheServiceIsReady exercises the provider-backed half of the
// contract: the core must ask, and must keep asking until the answer changes.
func TestPortProbeWaitsUntilTheServiceIsReady(t *testing.T) {
	var asks int64
	readyAfter := int64(3)
	base, err := sandboxtest.NewProvider(sandbox.ProviderConfig{Image: "test:latest"})
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	prov := &sandboxtest.ProbedProvider{Provider: base.(*sandboxtest.Provider)}
	prov.Ready = func(context.Context, sandbox.Environment, sandbox.ReadyProbe) (bool, string, error) {
		asks++
		return asks >= readyAfter, "connection refused", nil
	}
	if err := sandbox.RegisterProvider("probed", func(sandbox.ProviderConfig) (sandbox.Provider, error) {
		return prov, nil
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	t.Cleanup(func() { sandbox.UnregisterProvider("probed") })

	sb, err2 := sandbox.Open(context.Background(),
		sandbox.WithProvider("probed"),
		sandbox.WithAllowedProviders("probed"),
		sandbox.WithMinimumIsolation(sandbox.IsolationNone),
	)
	if err2 != nil {
		t.Fatalf("Open: %v", err2)
	}
	defer sb.Close()

	start := time.Now()
	if _, err := sb.Run(context.Background(), sandbox.Request{
		Runtime:    "sh",
		Code:       "echo starting",
		Background: true,
		ReadyProbe: &sandbox.ReadyProbe{Port: 8080, Deadline: 5 * time.Second},
	}); err != nil {
		t.Fatalf("the service became ready on the third ask, so the run should pass: %v", err)
	}
	if asks < readyAfter {
		t.Fatalf("the core asked %d times and gave up before the service came up", asks)
	}
	if d := time.Since(start); d < 2*readinessPollObserved() {
		t.Fatalf("the probe returned in %v without ever waiting for a poll interval", d)
	}
}

// readinessPollObserved keeps this readable: the interval lives in the package under
// test, and the assertion only needs "more than one round trip".
func readinessPollObserved() time.Duration { return 100 * time.Millisecond }

// TestPortProbeFailsWhenTheServiceNeverListens is the negative half of the same path.
func TestPortProbeFailsWhenTheServiceNeverListens(t *testing.T) {
	base, err := sandboxtest.NewProvider(sandbox.ProviderConfig{Image: "test:latest"})
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	prov := &sandboxtest.ProbedProvider{Provider: base.(*sandboxtest.Provider)}
	prov.Ready = func(context.Context, sandbox.Environment, sandbox.ReadyProbe) (bool, string, error) {
		return false, "connect: connection refused", nil
	}
	if err := sandbox.RegisterProvider("never-ready", func(sandbox.ProviderConfig) (sandbox.Provider, error) {
		return prov, nil
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	t.Cleanup(func() { sandbox.UnregisterProvider("never-ready") })

	sb, err2 := sandbox.Open(context.Background(),
		sandbox.WithProvider("never-ready"),
		sandbox.WithAllowedProviders("never-ready"),
		sandbox.WithMinimumIsolation(sandbox.IsolationNone),
	)
	if err2 != nil {
		t.Fatalf("Open: %v", err2)
	}
	defer sb.Close()

	_, err = sb.Run(context.Background(), sandbox.Request{
		Runtime:    "sh",
		Code:       "echo starting",
		Background: true,
		ReadyProbe: &sandbox.ReadyProbe{Port: 9090, Deadline: 300 * time.Millisecond},
	})
	var re *sandbox.ReadinessError
	if !errors.As(err, &re) {
		t.Fatalf("want ReadinessError, got %T: %v", err, err)
	}
	if re.LastSeen != "connect: connection refused" {
		t.Fatalf("what the provider saw must reach the caller, got %q", re.LastSeen)
	}
	if re.Probe.Port != 9090 {
		t.Fatalf("the failed condition must be echoed, got %+v", re.Probe)
	}
}

// TestExposeOnExistingSessionIsRefused. The mapping is a create-time property of the
// substrate, so a run that asks for one later cannot get it. Before EnvSpec carried
// Expose at all, no provider could honour the field even at creation: it was dropped on
// the floor between the request and the engine.
func TestExposeOnExistingSessionIsRefused(t *testing.T) {
	// The memory provider does not declare CapPortForward, so the earliest honest
	// refusal is the capability gate; either way the caller is told rather than misled.
	sb := openHandle(t)
	ctx := context.Background()
	sess, err := sb.Session(ctx, "expose-none")
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	defer sess.Release(ctx)

	_, err = sess.Exec(ctx, sandbox.Request{
		Runtime: "sh",
		Code:    "echo serving",
		Expose:  []sandbox.PortMap{{HostPort: 8080, GuestPort: 8080}},
	})
	if err == nil {
		t.Fatal("a run asked to publish a port on a session created without one, and nothing said so")
	}
	var ce *sandbox.ExposeConflictError
	var capErr *sandbox.CapabilityError
	if !errors.As(err, &ce) && !errors.As(err, &capErr) {
		t.Fatalf("want ExposeConflictError or CapabilityError, got %T: %v", err, err)
	}
}

// TestEnvSpecCarriesExpose proves the structural half of the fix: a provider receives
// the mapping at create, which is the only moment it can act on it.
func TestEnvSpecCarriesExpose(t *testing.T) {
	var seen []sandbox.PortMap
	if err := sandbox.RegisterProvider("sees-expose", func(sandbox.ProviderConfig) (sandbox.Provider, error) {
		return &exposeRecordingProvider{onEnv: func(spec sandbox.EnvSpec) { seen = spec.Expose }}, nil
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	t.Cleanup(func() { sandbox.UnregisterProvider("sees-expose") })

	sb, err := sandbox.Open(context.Background(),
		sandbox.WithProvider("sees-expose"),
		sandbox.WithAllowedProviders("sees-expose"),
		sandbox.WithMinimumIsolation(sandbox.IsolationNone),
	)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer sb.Close()

	if _, err := sb.Session(context.Background(), "with-expose",
		sandbox.WithSessionExpose(sandbox.PortMap{HostPort: 9099, GuestPort: 80}),
	); err != nil {
		t.Fatalf("Session: %v", err)
	}
	if len(seen) != 1 || seen[0].HostPort != 9099 || seen[0].GuestPort != 80 {
		t.Fatalf("EnvSpec.Expose did not reach the provider at create: %+v", seen)
	}
}

// exposeRecordingProvider is a minimal provider that records what the core handed it.
type exposeRecordingProvider struct {
	onEnv func(sandbox.EnvSpec)
}

func (p *exposeRecordingProvider) Name() string { return "sees-expose" }
func (p *exposeRecordingProvider) Isolation() sandbox.Isolation {
	return sandbox.IsolationContainer
}
func (p *exposeRecordingProvider) Capabilities() sandbox.CapabilitySet {
	return sandbox.Capabilities(sandbox.CapSession, sandbox.CapPortForward)
}
func (p *exposeRecordingProvider) Probe(context.Context) error { return nil }
func (p *exposeRecordingProvider) Close(context.Context) error { return nil }
func (p *exposeRecordingProvider) NewEnvironment(_ context.Context, spec sandbox.EnvSpec) (sandbox.Environment, error) {
	if p.onEnv != nil {
		p.onEnv(spec)
	}
	return &lyingEnv{}, nil
}

// forwardProvider declares port forwarding so a run-level Expose reaches the
// session-time consistency check instead of the capability gate.
type forwardProvider struct {
	exposeRecordingProvider
}

func (p *forwardProvider) Name() string { return "forwarder" }
func (p *forwardProvider) Capabilities() sandbox.CapabilitySet {
	// CapFileBridge is deliberately absent: Open now refuses a declared bit the
	// provider does not back with an interface, which is the bind-time claim check.
	return sandbox.Capabilities(sandbox.CapSession, sandbox.CapPortForward)
}

// TestExposeOnForwardingSessionIsRefusedWhenNotPublished is the precise shape: the
// provider can forward, the session published a different mapping, and the run must not
// be told its service is reachable from the host.
func TestExposeOnForwardingSessionIsRefusedWhenNotPublished(t *testing.T) {
	if err := sandbox.RegisterProvider("forwarder", func(sandbox.ProviderConfig) (sandbox.Provider, error) {
		return &forwardProvider{}, nil
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	t.Cleanup(func() { sandbox.UnregisterProvider("forwarder") })
	sb, err := sandbox.Open(context.Background(),
		sandbox.WithProvider("forwarder"),
		sandbox.WithAllowedProviders("forwarder"),
		sandbox.WithMinimumIsolation(sandbox.IsolationNone),
	)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer sb.Close()

	sess, err := sb.Session(context.Background(), "fwd-1",
		sandbox.WithSessionExpose(sandbox.PortMap{HostPort: 1234, GuestPort: 80}),
	)
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	defer sess.Release(context.Background())

	_, err = sess.Exec(context.Background(), sandbox.Request{
		Runtime: "sh",
		Code:    "echo serving",
		Expose:  []sandbox.PortMap{{HostPort: 8080, GuestPort: 8080}},
	})
	var ce *sandbox.ExposeConflictError
	if !errors.As(err, &ce) {
		t.Fatalf("want ExposeConflictError, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "1234->80") {
		t.Fatalf("the error must show what the session really publishes, got %v", err)
	}
	if sandbox.ClassOf(err) != sandbox.ClassPermanent {
		t.Fatalf("a create-time mismatch is not retryable, class=%v", sandbox.ClassOf(err))
	}
}

// TestExposeMatchingWhatWasPublishedIsAccepted closes the loop: the check has to allow
// the case it exists to validate.
func TestExposeMatchingWhatWasPublishedIsAccepted(t *testing.T) {
	if err := sandbox.RegisterProvider("forwarder-ok", func(sandbox.ProviderConfig) (sandbox.Provider, error) {
		return &forwardProvider{}, nil
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	t.Cleanup(func() { sandbox.UnregisterProvider("forwarder-ok") })
	sb, err := sandbox.Open(context.Background(),
		sandbox.WithProvider("forwarder-ok"),
		sandbox.WithAllowedProviders("forwarder-ok"),
		sandbox.WithMinimumIsolation(sandbox.IsolationNone),
	)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer sb.Close()

	sess, err := sb.Session(context.Background(), "fwd-2",
		sandbox.WithSessionExpose(sandbox.PortMap{HostPort: 8080, GuestPort: 8080}),
	)
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	defer sess.Release(context.Background())
	if _, err := sess.Exec(context.Background(), sandbox.Request{
		Runtime: "sh",
		Code:    "echo serving",
		Expose:  []sandbox.PortMap{{HostPort: 8080, GuestPort: 8080}},
	}); err != nil {
		t.Fatalf("a run asking for a port the session already publishes should proceed: %v", err)
	}
}
