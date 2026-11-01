package sandbox_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/rexleimo/agno-go/pkg/hno/sandbox"
	"github.com/rexleimo/agno-go/pkg/hno/sandbox/sandboxtest"
)

// The isolation ladder describes a boundary, and the usual way of filling it in --
// "this is the docker provider, so it is container isolation" -- is wrong on machines
// where the docker binary is a shim over a VM engine. These tests pin the replacement:
// the tier is read off measurements, a claim the readings do not support is refused,
// and an unmeasured handle is not allowed to look healthy on the strength of nobody
// having checked (same family as D-36).

// boundaryDouble registers the memory double under a fresh name and returns the handle
// plus the double, so assertions can read what the provider was actually asked.
func boundaryDouble(t *testing.T, name string, tweak func(*sandboxtest.Provider)) (*sandbox.Sandbox, *sandboxtest.Provider) {
	t.Helper()
	inner := &sandboxtest.Provider{}
	if tweak != nil {
		tweak(inner)
	}
	registerFixture(t, name, func(sandbox.ProviderConfig) (sandbox.Provider, error) { return inner, nil })
	sb := openWith(t, name, "alpine:3.20")
	return sb, inner
}

// TestBoundaryRefusesAProviderThatCannotAnswer is the reachability half of D-36: an
// absent capability must be said out loud, with the interface to implement named,
// rather than reported as a measurement that found nothing.
func TestBoundaryRefusesAProviderThatCannotAnswer(t *testing.T) {
	inner := &sandboxtest.Provider{}
	registerFixture(t, "no-boundary", func(sandbox.ProviderConfig) (sandbox.Provider, error) {
		return noQueryProvider{inner: inner}, nil
	})
	sb := openWith(t, "no-boundary", "alpine:3.20")

	_, err := sb.MeasureKernelBoundary(context.Background(), 0)
	if err == nil {
		t.Fatalf("a provider that cannot measure returned no error; the gap is invisible again")
	}
	var unsupported *sandbox.BoundaryUnsupportedError
	if !asBoundaryUnsupported(err, &unsupported) {
		t.Fatalf("error is %T (%v), want *sandbox.BoundaryUnsupportedError", err, err)
	}
	if !strings.Contains(err.Error(), "KernelBoundaryQueryable") {
		t.Errorf("error %q does not name the interface to implement", err.Error())
	}
	// Nothing was provisioned on the way to the refusal.
	if inner.BoundaryCalls != 0 {
		t.Errorf("refusal still attempted %d measurements", inner.BoundaryCalls)
	}
}

func asBoundaryUnsupported(err error, target **sandbox.BoundaryUnsupportedError) bool {
	if e, ok := err.(*sandbox.BoundaryUnsupportedError); ok {
		*target = e
		return true
	}
	return false
}

// TestBoundaryBoxBudgetIsRefusedBeforeProvisioning pins that a cost guard refuses
// before paying for boots, not after.
func TestBoundaryBoxBudgetIsRefusedBeforeProvisioning(t *testing.T) {
	sb, inner := boundaryDouble(t, "boundary-budget", nil)
	for _, boxes := range []int{1, -1, 99} {
		_, err := sb.MeasureKernelBoundary(context.Background(), boxes)
		var budget *sandbox.BoundaryBudgetError
		if err == nil || !assignBudget(err, &budget) {
			t.Fatalf("boxes=%d: err=%v, want *sandbox.BoundaryBudgetError", boxes, err)
		}
		if budget.Boxes != boxes {
			t.Errorf("boxes=%d: error carries %d", boxes, budget.Boxes)
		}
	}
	if inner.BoundaryCalls != 0 {
		t.Errorf("three refusals still reached the provider %d times", inner.BoundaryCalls)
	}
}

func assignBudget(err error, target **sandbox.BoundaryBudgetError) bool {
	if e, ok := err.(*sandbox.BoundaryBudgetError); ok {
		*target = e
		return true
	}
	return false
}

// TestBoundaryDefaultBoxCount: the default has to be what the docs say, and a caller
// who omits it must not get a single-box "measurement" that cannot compare anything.
func TestBoundaryDefaultBoxCount(t *testing.T) {
	sb, inner := boundaryDouble(t, "boundary-default", nil)
	report, err := sb.MeasureKernelBoundary(context.Background(), 0)
	if err != nil {
		t.Fatalf("measure: %v", err)
	}
	if inner.LastBoundaryBoxes != sandbox.DefaultBoundaryBoxes {
		t.Errorf("provider was asked for %d boxes, the documented default is %d", inner.LastBoundaryBoxes, sandbox.DefaultBoundaryBoxes)
	}
	if report.Boxes != sandbox.DefaultBoundaryBoxes {
		t.Errorf("report says %d boxes", report.Boxes)
	}
	if report.Verdict != sandbox.BoundarySharedKernel {
		t.Errorf("the memory double shares one kernel by construction; verdict %q lies in the other direction", report.Verdict)
	}
}

// TestBoundaryPrivateClaimWithoutDistinctReadingsIsRefused is the anti-lying test.
// A provider that says "one kernel per box" while handing back identical readings is
// over-claiming the strongest tier in the ladder, and the core has to read the
// readings rather than the assertion.
func TestBoundaryPrivateClaimWithoutDistinctReadingsIsRefused(t *testing.T) {
	sb, _ := boundaryDouble(t, "boundary-overclaim", func(p *sandboxtest.Provider) {
		p.BoundaryReport = &sandbox.KernelBoundaryReport{
			Boxes:   2,
			Verdict: sandbox.BoundaryPrivateKernel,
			BootIDs: []string{"same", "same"},
		}
	})
	report, err := sb.MeasureKernelBoundary(context.Background(), 2)
	if err != nil {
		t.Fatalf("measure: %v", err)
	}
	if report.Verdict != sandbox.BoundarySharedKernel {
		t.Fatalf("verdict %q survived two identical readings; the claim was trusted over the evidence", report.Verdict)
	}
	joined := strings.Join(report.Fixes, " | ")
	if !strings.Contains(joined, "share a kernel") {
		t.Errorf("Fixes %q do not say the boxes share a kernel", joined)
	}
}

// TestBoundaryClaimWithNoReadingsIsUnknown closes the other direction: an empty
// measurement must not default to either conclusion.
func TestBoundaryClaimWithNoReadingsIsUnknown(t *testing.T) {
	sb, _ := boundaryDouble(t, "boundary-no-readings", func(p *sandboxtest.Provider) {
		p.BoundaryReport = &sandbox.KernelBoundaryReport{
			Boxes:   2,
			Verdict: sandbox.BoundaryPrivateKernel,
		}
	})
	report, err := sb.MeasureKernelBoundary(context.Background(), 2)
	if err != nil {
		t.Fatalf("measure: %v", err)
	}
	if report.Verdict != sandbox.BoundaryUnknown {
		t.Fatalf("verdict %q came out of no readings at all", report.Verdict)
	}
	if len(report.Fixes) == 0 {
		t.Errorf("an unknown verdict arrived with no remediation")
	}
}

// TestDiagnoseReportsBoundaryOnlyWhenMeasured is the reachability assertion for this
// feature: the cached verdict must actually arrive in the health report, and the
// absence of a measurement must not be rendered as a passing check.
func TestDiagnoseReportsBoundaryOnlyWhenMeasured(t *testing.T) {
	ctx := context.Background()

	t.Run("unmeasured produces no check", func(t *testing.T) {
		sb, _ := boundaryDouble(t, "boundary-unmeasured", nil)
		diag, err := sb.Diagnose(ctx)
		if err != nil {
			t.Fatalf("diagnose: %v", err)
		}
		if _, ok := findCheck(diag, "kernel-boundary"); ok {
			t.Errorf("Diagnose invented a kernel-boundary check nobody measured")
		}
	})

	t.Run("shared kernel at container floor is consistent", func(t *testing.T) {
		sb, _ := boundaryDouble(t, "boundary-floor-none", nil)
		if _, err := sb.MeasureKernelBoundary(ctx, 2); err != nil {
			t.Fatalf("measure: %v", err)
		}
		diag, err := sb.Diagnose(ctx)
		if err != nil {
			t.Fatalf("diagnose: %v", err)
		}
		c, ok := findCheck(diag, "kernel-boundary")
		if !ok {
			t.Fatalf("the measurement is cached but Diagnose does not report it: implemented, unreachable")
		}
		if !c.OK {
			t.Errorf("container floor with a shared kernel is not a contradiction, yet the check failed: %s", c.Fix)
		}
	})

	t.Run("shared kernel contradicts a micro-VM floor", func(t *testing.T) {
		// over claiming engine.
		//
		// This fixture is the only way to reach the failing branch at all: a provider
		// that honestly says IsolationContainer is refused by WithMinimumIsolation at
		// Open, so a check nested behind that floor would never be seen by anyone whose
		// provider told the truth about its tier. What has to be caught is the opposite
		// case -- an engine that claims the VM tier and shares one kernel underneath it,
		// which admission cannot see because it trusts the claim.
		name := "boundary-claims-vm"
		inner := &sandboxtest.Provider{}
		registerFixture(t, name, func(sandbox.ProviderConfig) (sandbox.Provider, error) {
			return overclaimProvider{Provider: inner}, nil
		})
		sb, err := sandbox.Open(ctx,
			sandbox.WithProvider(name),
			sandbox.WithAllowedProviders(name),
			sandbox.WithImage("alpine:3.20"),
			sandbox.WithMinimumIsolation(sandbox.IsolationMicroVM),
		)
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		t.Cleanup(func() { _ = sb.Close() })
		if _, err := sb.MeasureKernelBoundary(ctx, 2); err != nil {
			t.Fatalf("measure: %v", err)
		}
		diag, err := sb.Diagnose(ctx)
		if err != nil {
			t.Fatalf("diagnose: %v", err)
		}
		c, ok := findCheck(diag, "kernel-boundary")
		if !ok {
			t.Fatalf("no kernel-boundary check after measuring")
		}
		if c.OK {
			t.Errorf("the handle demands micro-VM and the engine measured one shared kernel, yet the check passed: %s", c.Got)
		}
		if !strings.Contains(c.Fix, "hno sandbox boundary") {
			t.Errorf("Fix %q does not tell the operator how to re-measure", c.Fix)
		}
		joined := strings.Join(diag.Fixes, " | ")
		if !strings.Contains(joined, "shared-kernel") {
			t.Errorf("Diagnose.Fixes %q does not name the measured verdict", joined)
		}
	})
}

// overclaimProvider is an engine that reports IsolationMicroVM while delegating
// everything else to the memory double, which shares one kernel by construction. The
// embedding promotes MeasureKernelBoundary, so the claim and the measurement disagree
// in exactly the way a name-based tier assignment would let through.
type overclaimProvider struct {
	*sandboxtest.Provider
}

func (overclaimProvider) Isolation() sandbox.Isolation { return sandbox.IsolationMicroVM }

// TestKernelBoundaryVocabularyIsRoundTripped guards the CLI's --expect and the guide's
// names the same way the isolation vocabulary is guarded: a typo must not silently
// become a different verdict.
// TestBoundaryErrorStringsAreTheirOwnContract covers the two typed errors' text inside this
// package. They were exercised only from cmd/hno, where the calls do not count toward this
// package's coverage and, more importantly, nothing here pinned the wording. Actionable
// strings are an API surface without a compiler (D-34): an empty or drifted Remediation()
// would still have left every test green.
func TestBoundaryErrorStringsAreTheirOwnContract(t *testing.T) {
	unsupported := &sandbox.BoundaryUnsupportedError{Provider: "quiet"}
	if !strings.Contains(unsupported.Error(), "quiet") || !strings.Contains(unsupported.Error(), "KernelBoundaryQueryable") {
		t.Errorf("Error() %q neither names the provider nor the interface to implement", unsupported.Error())
	}
	if !strings.Contains(unsupported.Remediation(), "KernelBoundaryQueryable") {
		t.Errorf("Remediation() %q does not repeat what must be implemented", unsupported.Remediation())
	}

	budget := &sandbox.BoundaryBudgetError{Boxes: 9}
	if !strings.Contains(budget.Error(), "9") {
		t.Errorf("Error() %q does not show the rejected box count", budget.Error())
	}
	rem := budget.Remediation()
	if !strings.Contains(rem, "2") || !strings.Contains(rem, fmt.Sprint(sandbox.MaxBoundaryBoxes)) {
		t.Errorf("Remediation() %q does not state the accepted range", rem)
	}
	if !strings.Contains(rem, fmt.Sprint(sandbox.DefaultBoundaryBoxes)) {
		t.Errorf("Remediation() %q does not offer the default the caller can omit", rem)
	}
}

func TestKernelBoundaryVocabularyIsRoundTripped(t *testing.T) {
	for _, want := range []sandbox.KernelBoundary{
		sandbox.BoundaryUnmeasured,
		sandbox.BoundarySharedKernel,
		sandbox.BoundaryPrivateKernel,
		sandbox.BoundaryUnknown,
	} {
		got, err := sandbox.ParseKernelBoundary(want.String())
		if err != nil {
			t.Errorf("ParseKernelBoundary(%q): %v", want.String(), err)
			continue
		}
		if got != want {
			t.Errorf("%q parsed to %q", want.String(), got)
		}
	}
	// The --expect shorthands resolve to the same verdicts, and each shorthand must
	// differ from the other under a wrong mapping.
	if s, err := sandbox.ParseKernelBoundary("shared"); err != nil || s != sandbox.BoundarySharedKernel {
		t.Errorf("shared -> %q (%v)", s, err)
	}
	if p, err := sandbox.ParseKernelBoundary("private"); err != nil || p != sandbox.BoundaryPrivateKernel {
		t.Errorf("private -> %q (%v)", p, err)
	}
	if _, err := sandbox.ParseKernelBoundary("privae"); err == nil {
		t.Errorf("a typo parsed successfully; --expect would silently mean something else")
	}
	// Distinctness: the two shorthands must not collapse onto one verdict.
	if s, _ := sandbox.ParseKernelBoundary("shared"); s == sandbox.BoundaryPrivateKernel {
		t.Errorf("shared and private are the same verdict")
	}
}

// TestKernelBoundaryMarshalsAsItsName keeps `--json` a readable contract.
func TestKernelBoundaryMarshalsAsItsName(t *testing.T) {
	b, err := json.Marshal(struct {
		V sandbox.KernelBoundary `json:"verdict"`
	}{V: sandbox.BoundaryPrivateKernel})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"private-kernel"`) {
		t.Errorf("json is %s; the verdict arrives as a bare integer", string(b))
	}
}

// TestBoundaryCacheIsPerHandle stops one handle's measurement from talking about
// another's engine.
func TestBoundaryCacheIsPerHandle(t *testing.T) {
	a, _ := boundaryDouble(t, "boundary-cache-a", nil)
	b, _ := boundaryDouble(t, "boundary-cache-b", nil)
	if _, ok := a.KernelBoundary(); ok {
		t.Fatalf("a fresh handle already has a cached measurement")
	}
	if _, err := a.MeasureKernelBoundary(context.Background(), 2); err != nil {
		t.Fatalf("measure: %v", err)
	}
	if _, ok := a.KernelBoundary(); !ok {
		t.Fatalf("the measurement did not reach the cache Diagnose reads")
	}
	if _, ok := b.KernelBoundary(); ok {
		t.Fatalf("a second handle inherited the first one's measurement")
	}
}

// A2 (D-39): a measured private kernel admits a provider whose *claim* is below the floor,
// and a measured shared kernel refuses a provider whose claim is above what it delivers.
// Both halves are exercised here through Open, because admission is the thing being changed
// and a helper-level test would prove only that a boolean can be computed.

// openWithBoundary opens a handle against a named fixture with a floor and an optional
// measurement.
func openWithBoundary(t *testing.T, name string, floor sandbox.Isolation, measurement *sandbox.KernelBoundaryReport) (*sandbox.Sandbox, error) {
	t.Helper()
	opts := []sandbox.Option{
		sandbox.WithProvider(name),
		sandbox.WithAllowedProviders(name),
		sandbox.WithImage("alpine:3.20"),
		sandbox.WithMinimumIsolation(floor),
	}
	if measurement != nil {
		opts = append(opts, sandbox.WithBoundaryMeasurement(*measurement))
	}
	return sandbox.Open(context.Background(), opts...)
}

// measuredReport is a report that would be usable as evidence here and now: the right
// provider, this host, just taken.
func measuredReport(provider string, verdict sandbox.KernelBoundary) *sandbox.KernelBoundaryReport {
	ids := []string{"aaaa-1111", "bbbb-2222"}
	if verdict != sandbox.BoundaryPrivateKernel {
		ids = []string{"same-id", "same-id"}
	}
	host := struct {
		goos, goarch string
	}{runtime.GOOS, runtime.GOARCH}
	return &sandbox.KernelBoundaryReport{
		Provider:    provider,
		Boxes:       2,
		Verdict:     verdict,
		BootIDs:     ids,
		GuestKernel: "Linux 6.18.35",
		HostKernel:  host.goos + "/" + host.goarch,
		MeasuredAt:  time.Now().UTC(),
	}
}

// TestAdmissionLiftsTheFloorOnAMeasuredPrivateKernel is the decision this batch exists
// for: the handle used to fail with "no provider satisfies isolation microvm" because the
// provider's constant said container, while its boxes each had their own kernel.
func TestAdmissionLiftsTheFloorOnAMeasuredPrivateKernel(t *testing.T) {
	inner := &sandboxtest.Provider{}
	registerFixture(t, "lift-private", func(sandbox.ProviderConfig) (sandbox.Provider, error) { return inner, nil })

	// Baseline: without evidence the floor is refused. If this ever passes, the rest of
	// the test proves nothing.
	if _, err := openWithBoundary(t, "lift-private", sandbox.IsolationMicroVM, nil); err == nil {
		t.Fatalf("a container-isolation provider satisfied a micro-VM floor with no evidence: admission widened on its own")
	}

	sb, err := openWithBoundary(t, "lift-private", sandbox.IsolationMicroVM, measuredReport("lift-private", sandbox.BoundaryPrivateKernel))
	if err != nil {
		t.Fatalf("a measured private kernel did not satisfy the floor: %v", err)
	}
	t.Cleanup(func() { _ = sb.Close() })

	st := sb.Status()
	if st.Boundary == nil || st.Boundary.Verdict != sandbox.BoundaryPrivateKernel {
		t.Fatalf("Status does not carry the measurement admission relied on: %+v", st.Boundary)
	}
	// The claim is not overwritten: the handle still says the provider claims container
	// isolation, and reports the evidence next to it. Rewriting the claim to micro-VM
	// would make every downstream reader believe an engine changed its nature.
	if st.Isolation != sandbox.IsolationContainer {
		t.Errorf("Status.Isolation = %q; the provider's own claim must stay visible", st.Isolation)
	}

	diag, err := sb.Diagnose(context.Background())
	if err != nil {
		t.Fatalf("diagnose: %v", err)
	}
	c, ok := findCheck(diag, "isolation")
	if !ok {
		t.Fatalf("no isolation check")
	}
	if !c.OK {
		t.Errorf("the health report contradicts admission: got %q, fix %q", c.Got, c.Fix)
	}
	if !strings.Contains(c.Got, "measured") {
		t.Errorf("the isolation line %q does not say the floor was met on measurement", c.Got)
	}
}

// TestAdmissionRefusesAVMClaimAgainstASharedKernel is the tightening half: the claim would
// have passed, and the measurement is the only thing that can stop it.
func TestAdmissionRefusesAVMClaimAgainstASharedKernel(t *testing.T) {
	inner := &sandboxtest.Provider{}
	registerFixture(t, "claim-vm-shared-kernel", func(sandbox.ProviderConfig) (sandbox.Provider, error) {
		return overclaimProvider{Provider: inner}, nil
	})

	// The claim alone satisfies the floor -- that is the hole.
	sb, err := openWithBoundary(t, "claim-vm-shared-kernel", sandbox.IsolationMicroVM, nil)
	if err != nil {
		t.Fatalf("baseline: a provider claiming the VM tier was refused without evidence: %v", err)
	}
	_ = sb.Close()

	_, err = openWithBoundary(t, "claim-vm-shared-kernel", sandbox.IsolationMicroVM, measuredReport("claim-vm-shared-kernel", sandbox.BoundarySharedKernel))
	if err == nil {
		t.Fatalf("an engine claiming micro-VM while sharing one kernel was admitted: the measurement bought nothing")
	}
	if !strings.Contains(err.Error(), "shared kernel") {
		t.Errorf("refusal %q does not say what was measured", err.Error())
	}
	var unavail *sandbox.UnavailableError
	if !errors.As(err, &unavail) {
		t.Fatalf("refusal is %T, want *sandbox.UnavailableError", err)
	}
	joined := strings.Join(unavail.Fixes, " | ")
	if !strings.Contains(joined, "do not raise the floor to match the claim") {
		t.Errorf("fixes %q do not warn against papering over it", joined)
	}
}

// TestAdmissionIgnoresEvidenceAboutAnotherEngine: a report is evidence about one engine.
func TestAdmissionIgnoresEvidenceAboutAnotherEngine(t *testing.T) {
	inner := &sandboxtest.Provider{}
	registerFixture(t, "lift-wrong-provider", func(sandbox.ProviderConfig) (sandbox.Provider, error) { return inner, nil })
	report := measuredReport("some-other-engine", sandbox.BoundaryPrivateKernel)
	_, err := openWithBoundary(t, "lift-wrong-provider", sandbox.IsolationMicroVM, report)
	if err == nil {
		t.Fatalf("a measurement taken on another provider satisfied this provider's floor")
	}
	if !strings.Contains(err.Error(), "some-other-engine") {
		t.Errorf("refusal %q does not name where the evidence came from", err.Error())
	}
}

// TestAdmissionIgnoresEvidenceFromAnotherHost: the same engine on another machine is not
// this machine.
func TestAdmissionIgnoresEvidenceFromAnotherHost(t *testing.T) {
	inner := &sandboxtest.Provider{}
	registerFixture(t, "lift-wrong-host", func(sandbox.ProviderConfig) (sandbox.Provider, error) { return inner, nil })
	report := measuredReport("lift-wrong-host", sandbox.BoundaryPrivateKernel)
	report.HostKernel = "plan9/mips"
	if _, err := openWithBoundary(t, "lift-wrong-host", sandbox.IsolationMicroVM, report); err == nil {
		t.Fatalf("a measurement taken on plan9/mips satisfied a floor on this host")
	} else if !strings.Contains(err.Error(), "different host") {
		t.Errorf("refusal %q does not say the host differs", err.Error())
	}
}

// TestAdmissionIgnoresAStaleMeasurement: an engine can be upgraded under a report, which
// is exactly the risk of letting a measurement outlive the moment it described.
func TestAdmissionIgnoresAStaleMeasurement(t *testing.T) {
	inner := &sandboxtest.Provider{}
	registerFixture(t, "lift-stale", func(sandbox.ProviderConfig) (sandbox.Provider, error) { return inner, nil })
	report := measuredReport("lift-stale", sandbox.BoundaryPrivateKernel)
	report.MeasuredAt = time.Now().Add(-2 * sandbox.DefaultTimeouts().BoundaryFresh)
	_, err := openWithBoundary(t, "lift-stale", sandbox.IsolationMicroVM, report)
	if err == nil {
		t.Fatalf("a stale measurement still lifted the floor")
	}
	if !strings.Contains(err.Error(), "stale") {
		t.Errorf("refusal %q does not say the measurement expired", err.Error())
	}

	// A negative BoundaryFresh means "measure every time", which includes never
	// accepting a report that carries a timestamp.
	sb, oerr := sandbox.Open(context.Background(),
		sandbox.WithProvider("lift-stale"), sandbox.WithAllowedProviders("lift-stale"),
		sandbox.WithImage("alpine:3.20"),
		sandbox.WithMinimumIsolation(sandbox.IsolationMicroVM),
		sandbox.WithTimeouts(sandbox.Timeouts{BoundaryFresh: -1}),
		sandbox.WithBoundaryMeasurement(*measuredReport("lift-stale", sandbox.BoundaryPrivateKernel)),
	)
	if oerr == nil {
		_ = sb.Close()
		t.Errorf("BoundaryFresh=-1 accepted a measurement with a timestamp; it must mean measure-per-Open")
	}
}

// TestAdmissionTreatsUnknownAsNoEvidence: an engine that could not answer must not move
// the floor in either direction.
func TestAdmissionTreatsUnknownAsNoEvidence(t *testing.T) {
	inner := &sandboxtest.Provider{}
	registerFixture(t, "lift-unknown", func(sandbox.ProviderConfig) (sandbox.Provider, error) { return inner, nil })
	report := measuredReport("lift-unknown", sandbox.BoundaryUnknown)
	report.BootIDs = nil
	if _, err := openWithBoundary(t, "lift-unknown", sandbox.IsolationMicroVM, report); err == nil {
		t.Fatalf("an unknown measurement satisfied a micro-VM floor")
	}
	// And it does not refuse a floor the claim already met.
	if _, err := openWithBoundary(t, "lift-unknown", sandbox.IsolationContainer, report); err != nil {
		t.Fatalf("an unknown measurement refused a handle the claim allowed: %v", err)
	}
}

// TestZeroBoundaryFreshMeansTheDefault protects against a trap this field would otherwise
// create: WithTimeouts replaces the whole struct, so a caller who set only Setup and Exec
// arrives with BoundaryFresh zeroed. If zero meant "stale", a measurement taken one
// millisecond earlier would be rejected as expired -- and the message would say so, which
// looks like the runtime is lying about a measurement the caller just took.
func TestZeroBoundaryFreshMeansTheDefault(t *testing.T) {
	inner := &sandboxtest.Provider{}
	registerFixture(t, "lift-zero-fresh", func(sandbox.ProviderConfig) (sandbox.Provider, error) { return inner, nil })
	sb, err := sandbox.Open(context.Background(),
		sandbox.WithProvider("lift-zero-fresh"), sandbox.WithAllowedProviders("lift-zero-fresh"),
		sandbox.WithImage("alpine:3.20"),
		sandbox.WithMinimumIsolation(sandbox.IsolationMicroVM),
		// A partial literal, exactly as the real-engine helpers write it.
		sandbox.WithTimeouts(sandbox.Timeouts{Exec: time.Minute}),
		sandbox.WithBoundaryMeasurement(*measuredReport("lift-zero-fresh", sandbox.BoundaryPrivateKernel)),
	)
	if err != nil {
		t.Fatalf("a partial Timeouts literal turned a fresh measurement into an expired one: %v", err)
	}
	_ = sb.Close()
}
