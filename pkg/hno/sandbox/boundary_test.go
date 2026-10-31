package sandbox_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

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
