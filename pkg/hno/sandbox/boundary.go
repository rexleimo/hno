package sandbox

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"time"
)

// KernelBoundary says what a measurement found about how much of the kernel is
// shared between two boxes.
//
// It exists because the isolation ladder is a claim about a boundary, and the
// obvious way to fill that claim in — "this is the docker provider, so it is
// container isolation" — is provably wrong on real machines. On the host this
// project was developed on, the `docker` binary is a compatibility shim over the
// Apple containerisation engine, and every box it starts gets its own kernel:
// four boxes produced four different /proc/sys/kernel/random/boot_id values and
// four different /proc/uptime readings. Name-based classification would have
// labelled that host container isolation while the engine was handing out VMs,
// and the same binary on a Linux host with a shared-kernel daemon would be
// telling the truth. So the tier is measured, never named.
//
// The four states are deliberately not collapsed. "Unmeasured" is the absence of
// a claim; "unknown" is a claim the engine refused to support; the last two are
// answers. Reporting unknown as container isolation, or unmeasured as safe, is
// the false green light D-36 was written about.
type KernelBoundary int

const (
	// BoundaryUnmeasured means nobody asked yet.
	BoundaryUnmeasured KernelBoundary = iota
	// BoundarySharedKernel means two boxes reached the same kernel instance:
	// namespace and seccomp isolation, not a VM boundary.
	BoundarySharedKernel
	// BoundaryPrivateKernel means every box observed its own kernel instance:
	// the property IsolationMicroVM is supposed to describe.
	BoundaryPrivateKernel
	// BoundaryUnknown means the measurement ran and the engine did not give an
	// answer worth believing.
	BoundaryUnknown
)

// String renders the verdict in the vocabulary the guide and the CLI use.
func (b KernelBoundary) String() string {
	switch b {
	case BoundarySharedKernel:
		return "shared-kernel"
	case BoundaryPrivateKernel:
		return "private-kernel"
	case BoundaryUnknown:
		return "unknown"
	default:
		return "unmeasured"
	}
}

// MarshalText keeps `--json` output readable instead of a bare integer.
func (b KernelBoundary) MarshalText() ([]byte, error) { return []byte(b.String()), nil }

// ParseKernelBoundary accepts exactly the names String renders, plus "private"
// and "shared" as the CLI's --expect shorthands. An unrecognised value is an
// error: a typo in a gate is worse than no gate.
func ParseKernelBoundary(s string) (KernelBoundary, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "unmeasured":
		return BoundaryUnmeasured, nil
	case "shared", "shared-kernel":
		return BoundarySharedKernel, nil
	case "private", "private-kernel":
		return BoundaryPrivateKernel, nil
	case "unknown":
		return BoundaryUnknown, nil
	default:
		return BoundaryUnmeasured, fmt.Errorf("sandbox: %q is not a kernel boundary verdict; use %s", s, strings.Join(KernelBoundaryNames(), ", "))
	}
}

// KernelBoundaryNames lists the accepted spellings, for help text and errors.
func KernelBoundaryNames() []string {
	return []string{"shared-kernel", "private-kernel", "unknown", "unmeasured"}
}

// DefaultBoundaryBoxes is how many boxes a measurement uses when the caller does
// not say. Two is the minimum that can distinguish sharing from separation, and
// each additional box costs a real boot.
const DefaultBoundaryBoxes = 2

// MaxBoundaryBoxes caps the cost of one measurement. Booting boxes is the
// expensive thing this API does, so the cap is a refusal rather than a warning.
const MaxBoundaryBoxes = 4

// KernelBoundaryReport is a measurement plus the evidence it was derived from.
//
// The evidence travels with the verdict on purpose. A reader who disagrees with
// the conclusion can check the raw readings instead of taking the boolean on
// faith, and a provider that returns a verdict unsupported by its own evidence
// gets downgraded by the core rather than believed.
type KernelBoundaryReport struct {
	Verdict KernelBoundary
	Boxes   int
	// BootIDs holds one guest boot identifier per box, in box order. Sharing is
	// read off this list, so an empty entry means the box did not answer and the
	// verdict cannot be better than unknown.
	BootIDs []string
	// Uptimes holds each box's seconds-since-kernel-boot, which corroborates the
	// boot ids: two boxes on one kernel cannot have independently aged kernels.
	Uptimes []string
	// GuestKernel is the guest's "uname -sr" equivalent, HostKernel the same for
	// this process. A Linux guest on a non-Linux host already proves a VM exists;
	// the boot-id comparison then decides whether it is per box.
	GuestKernel string
	HostKernel  string
	// Evidence is the prose a human reads; Fixes is actionable remediation, in the
	// same shape as every other actionable string in this package (D-34: these are
	// checked against the exported surface by a gate).
	Evidence []string
	Fixes    []string
	// MeasuredAt is when the measurement finished.
	MeasuredAt time.Time
}

// Distinct returns how many different non-empty readings the measurement got.
func (r KernelBoundaryReport) Distinct() int {
	seen := map[string]bool{}
	for _, id := range r.BootIDs {
		if strings.TrimSpace(id) == "" {
			continue
		}
		seen[id] = true
	}
	return len(seen)
}

// KernelBoundaryQueryable is implemented by providers that can put several boxes
// side by side and report what their kernels say.
//
// It is a separate optional interface rather than a field on ProviderConfig
// because the answer costs work: it provisions real boxes, so it must never run
// as a side effect of opening a handle or of a routine Diagnose. Callers ask for
// it deliberately — `hno sandbox boundary` from a shell, MeasureKernelBoundary
// from code.
type KernelBoundaryQueryable interface {
	MeasureKernelBoundary(ctx context.Context, boxes int) (KernelBoundaryReport, error)
}

// BoundaryUnsupportedError is returned when the bound provider cannot measure the
// boundary at all. It names the interface to implement, which is how a missing
// capability becomes reachable instead of silently absent.
type BoundaryUnsupportedError struct {
	Provider string
}

func (e *BoundaryUnsupportedError) Error() string {
	return fmt.Sprintf("sandbox: provider %q cannot measure the kernel boundary: it does not implement sandbox.KernelBoundaryQueryable", e.Provider)
}

// Remediation is what an operator does about it.
func (e *BoundaryUnsupportedError) Remediation() string {
	return "ask the provider to implement sandbox.KernelBoundaryQueryable, or run a measurement against an engine that does; until then the isolation tier is a claim, not a fact"
}

// BoundaryBudgetError is returned for a box count outside what one measurement
// may cost.
type BoundaryBudgetError struct {
	Boxes int
}

func (e *BoundaryBudgetError) Error() string {
	return fmt.Sprintf("sandbox: %d boxes is not a valid boundary measurement: use %d to %d (two is the minimum that can tell sharing from separation)", e.Boxes, 2, MaxBoundaryBoxes)
}

// Remediation is what an operator does about it.
func (e *BoundaryBudgetError) Remediation() string {
	return fmt.Sprintf("re-run with a box count between 2 and %d, or omit it to use the default %d", MaxBoundaryBoxes, DefaultBoundaryBoxes)
}

// MeasureKernelBoundary asks the bound provider what its boxes' kernels say, and
// returns the reconciled answer.
//
// Two things are guaranteed. Nothing is provisioned when the provider cannot
// answer, so a capability gap costs the caller one call rather than N boots. And
// a verdict the provider's own evidence does not support is downgraded to unknown
// with the reason in the report, which is the "test doubles must not lie in the
// same direction as the provider" rule pointed at the provider instead.
func (s *Sandbox) MeasureKernelBoundary(ctx context.Context, boxes int) (*KernelBoundaryReport, error) {
	if boxes == 0 {
		boxes = DefaultBoundaryBoxes
	}
	if boxes < 2 || boxes > MaxBoundaryBoxes {
		return nil, &BoundaryBudgetError{Boxes: boxes}
	}
	prov := s.provider
	if prov == nil {
		return nil, fmt.Errorf("sandbox: no provider is bound to this handle, so the kernel boundary cannot be measured")
	}
	q, ok := prov.(KernelBoundaryQueryable)
	if !ok {
		return nil, &BoundaryUnsupportedError{Provider: prov.Name()}
	}
	report, err := q.MeasureKernelBoundary(ctx, boxes)
	if err != nil {
		return nil, err
	}
	reconcileBoundaryReport(&report)
	report.MeasuredAt = time.Now().UTC()

	s.noteBoundary(&report)
	out := report
	return &out, nil
}

// reconcileBoundaryReport is the core's check on the provider's self-report.
// The verdict must follow from the readings, or the verdict is not the core's
// problem to trust.
func reconcileBoundaryReport(r *KernelBoundaryReport) {
	if r == nil {
		return
	}
	r.Verdict = reconcileBoundaryVerdict(r.Verdict, r.Boxes, r.BootIDs, &r.Fixes)
	if r.Evidence == nil {
		r.Evidence = []string{}
	}
	if r.HostKernel == "" {
		r.HostKernel = hostKernelString()
	}
	if r.Boxes > 0 && len(r.BootIDs) == 0 {
		r.Evidence = append(r.Evidence, "the provider returned no readings at all")
	}
}

// reconcileBoundaryVerdict is split out so a provider-without-evidence shape can
// be tested directly. A claimed verdict survives only if the readings support it:
// private requires one distinct non-empty reading per box, shared requires a
// collision. Anything else becomes unknown, because a verdict nobody measured is
// exactly the green light that means "nobody checked".
func reconcileBoundaryVerdict(claimed KernelBoundary, boxes int, ids []string, fixes *[]string) KernelBoundary {
	readings := make([]string, 0, len(ids))
	for _, id := range ids {
		if strings.TrimSpace(id) == "" {
			continue
		}
		readings = append(readings, id)
	}
	switch {
	case len(readings) == 0:
		if fixes != nil {
			*fixes = append(*fixes, "the measurement produced no kernel readings: check that the engine can exec inside a box, then re-run hno sandbox boundary")
		}
		return BoundaryUnknown
	case len(readings) < boxes:
		if fixes != nil {
			*fixes = append(*fixes, fmt.Sprintf("only %d of %d boxes answered: the verdict cannot cover a measurement that is missing readings; re-run hno sandbox boundary once the engine is healthy", len(readings), boxes))
		}
		return BoundaryUnknown
	}
	seen := map[string]bool{}
	for _, id := range readings {
		seen[id] = true
	}
	if len(seen) == boxes {
		// One distinct reading per box: the kernels are separate. The provider's claim
		// is not needed to reach this, which is the point -- the readings decide.
		return BoundaryPrivateKernel
	}
	if claimed == BoundaryPrivateKernel && fixes != nil {
		*fixes = append(*fixes, fmt.Sprintf("the provider claimed a private kernel per box but %d readings collapse into %d distinct values: the boxes share a kernel, so the claim is refused", len(readings), len(seen)))
	}
	return BoundarySharedKernel
}

// hostKernelString describes this process's kernel, for the guest/host contrast.
func hostKernelString() string {
	return runtime.GOOS + "/" + runtime.GOARCH
}

// noteBoundary caches the latest measurement so Diagnose can talk about the
// boundary without booting anything on the caller's behalf.
func (s *Sandbox) noteBoundary(r *KernelBoundaryReport) {
	s.mu.Lock()
	if r != nil {
		cp := *r
		cp.BootIDs = append([]string(nil), r.BootIDs...)
		cp.Uptimes = append([]string(nil), r.Uptimes...)
		cp.Fixes = append([]string(nil), r.Fixes...)
		cp.Evidence = append([]string(nil), r.Evidence...)
		s.lastBoundary = &cp
	}
	s.mu.Unlock()
}

// KernelBoundary returns the cached measurement, if any. It never boots a box:
// reading the recorded answer is free, producing one is not.
func (s *Sandbox) KernelBoundary() (KernelBoundaryReport, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastBoundary == nil {
		return KernelBoundaryReport{}, false
	}
	out := *s.lastBoundary
	out.BootIDs = append([]string(nil), s.lastBoundary.BootIDs...)
	out.Uptimes = append([]string(nil), s.lastBoundary.Uptimes...)
	return out, true
}
