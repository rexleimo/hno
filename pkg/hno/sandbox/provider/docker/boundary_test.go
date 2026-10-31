package docker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/rexleimo/agno-go/pkg/hno/sandbox"
)

// The provider's boundary measurement is the piece that turns "docker means container
// isolation" from a naming convention into a question the engine answers. These tests
// drive it through the same fake command seam the rest of the package uses, so the
// verdict logic is pinned without an engine, and the real-engine test then checks the
// same logic against actual kernels.

// boundaryScript is a fake engine that hands out distinct ids, keeps each container
// running, and answers the kernel probe from a per-container script.
type boundaryScript struct {
	// readings maps container id to the three-line probe output.
	readings map[string]string
	// created and removed record the lifecycle calls, so leak assertions have
	// something real to count.
	created  []string
	removed  []string
	failExec bool
	// imagePresent answers the D-36 image question.
	imagePresent bool
	execExit     int
	execStderr   string
	startFails   bool
	closeFails   bool
	n            int
}

func (b *boundaryScript) respond(cmd Command) (Outcome, bool) {
	joined := strings.Join(cmd.Args, " ")
	switch {
	case strings.HasPrefix(joined, "image inspect"):
		if b.imagePresent {
			return Outcome{Stdout: "sha256:abc\n"}, true
		}
		return Outcome{Exit: 1, Stderr: "Error: No such image"}, true
	case strings.HasPrefix(joined, "create"):
		b.n++
		id := fmt.Sprintf("box-%d", b.n)
		b.created = append(b.created, id)
		return Outcome{Stdout: id + "\n"}, true
	case strings.HasPrefix(joined, "start "):
		if b.startFails {
			return Outcome{Exit: 1, Stderr: "start failed"}, true
		}
		return Outcome{Stdout: "ok\n"}, true
	case strings.HasPrefix(joined, "inspect --format"):
		if b.closeFails && len(b.removed) == len(b.created) {
			return Outcome{Stdout: "false\n"}, true
		}
		return Outcome{Stdout: "true\n"}, true
	case strings.HasPrefix(joined, "rm -f"):
		id := cmd.Args[len(cmd.Args)-1]
		b.removed = append(b.removed, id)
		if b.closeFails {
			return Outcome{Exit: 1, Stderr: "in use"}, true
		}
		return Outcome{}, true
	case strings.HasPrefix(joined, "exec"):
		if b.failExec {
			return Outcome{Exit: 1, Stderr: "no shell"}, true
		}
		id := cmd.Args[2]
		if out, ok := b.readings[id]; ok {
			return Outcome{Stdout: out, Exit: b.execExit, Stderr: b.execStderr}, true
		}
		return Outcome{Stdout: ""}, true
	case strings.HasPrefix(joined, "version"):
		return Outcome{Stdout: "27.0.0"}, true
	}
	return Outcome{}, false
}

// reading builds the three-line probe output the guest would print.
func reading(boot, up, kernel string) string {
	return boot + "\n" + up + "\n" + kernel + "\n"
}

func TestProviderImplementsKernelBoundaryQueryable(t *testing.T) {
	var _ sandbox.KernelBoundaryQueryable = (*Provider)(nil)
}

// TestBoundaryDistinctReadingsMeanPrivateKernel: the verdict must be read off the
// readings.
func TestBoundaryDistinctReadingsMeanPrivateKernel(t *testing.T) {
	b := &boundaryScript{readings: map[string]string{
		"box-1": reading("aaaa-1111", "6.51", "Linux 6.18.35"),
		"box-2": reading("bbbb-2222", "4.54", "Linux 6.18.35"),
	}}
	b.imagePresent = true
	fr := &fakeRunner{respond: b.respond}
	p, err := New(Config{Image: "golang:1.26", Runner: fr, StartupTimeout: 5e9})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	report, err := p.MeasureKernelBoundary(context.Background(), 2)
	if err != nil {
		t.Fatalf("measure: %v", err)
	}
	if report.Verdict != sandbox.BoundaryPrivateKernel {
		t.Fatalf("two distinct boot ids produced %q: the readings are what decide", report.Verdict)
	}
	if len(b.created) != 2 {
		t.Errorf("measured %d boxes, report says %d", len(b.created), report.Boxes)
	}
	if len(b.removed) != 2 {
		t.Errorf("the measurement left %d of %d boxes running: %v", 2-len(b.removed), 2, b.removed)
	}
	if !strings.Contains(strings.Join(report.Evidence, " | "), "one kernel per box") {
		t.Errorf("evidence %q does not state the conclusion it drew", strings.Join(report.Evidence, " | "))
	}
}

// TestBoundaryIdenticalReadingsMeanSharedKernel is the falsifier for the whole
// feature: an implementation that returned private-kernel unconditionally -- or that
// trusted what an engine "is" rather than what it said -- passes every other test and
// fails this one.
func TestBoundaryIdenticalReadingsMeanSharedKernel(t *testing.T) {
	b := &boundaryScript{readings: map[string]string{
		"box-1": reading("same-boot-id", "900.10", "Linux 6.6.0"),
		"box-2": reading("same-boot-id", "900.08", "Linux 6.6.0"),
	}}
	b.imagePresent = true
	fr := &fakeRunner{respond: b.respond}
	p, err := New(Config{Image: "golang:1.26", Runner: fr, StartupTimeout: 5e9})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	report, err := p.MeasureKernelBoundary(context.Background(), 2)
	if err != nil {
		t.Fatalf("measure: %v", err)
	}
	if report.Verdict != sandbox.BoundarySharedKernel {
		t.Fatalf("identical boot ids on two boxes returned %q; that is one kernel shared, which is container isolation and not a VM per box", report.Verdict)
	}
	if strings.Contains(strings.Join(report.Evidence, " | "), "one kernel per box") {
		t.Errorf("evidence claims a kernel per box for a shared kernel")
	}
}

// TestBoundaryPartialAnswerIsUnknown: a measurement that lost a box cannot support
// either conclusion.
func TestBoundaryPartialAnswerIsUnknown(t *testing.T) {
	b := &boundaryScript{readings: map[string]string{
		"box-1": reading("aaaa-1111", "6.51", "Linux 6.18.35"),
		"box-2": "",
	}}
	b.imagePresent = true
	fr := &fakeRunner{respond: b.respond}
	p, err := New(Config{Image: "golang:1.26", Runner: fr, StartupTimeout: 5e9})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	report, err := p.MeasureKernelBoundary(context.Background(), 2)
	if err != nil {
		t.Fatalf("a missing answer is a finding, not an engine fault: %v", err)
	}
	if report.Verdict != sandbox.BoundaryUnknown {
		t.Fatalf("verdict %q came out of one reading and one blank", report.Verdict)
	}
	if len(report.Fixes) == 0 {
		t.Errorf("unknown verdict with no remediation")
	}
}

// TestBoundaryRefusesBeforeBootingWhenTheImageIsAbsent ties the two measured claims
// together: the image question is cheap and the boundary question is not, so the cheap
// answer must gate the expensive one (D-36).
func TestBoundaryRefusesBeforeBootingWhenTheImageIsAbsent(t *testing.T) {
	b := &boundaryScript{readings: map[string]string{}, imagePresent: false}
	fr := &fakeRunner{respond: b.respond}
	p, err := New(Config{Image: "golang:1.26", Runner: fr, StartupTimeout: 5e9})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = p.MeasureKernelBoundary(context.Background(), 3)
	if err == nil {
		t.Fatalf("the measurement proceeded with an image the engine says is absent")
	}
	if len(b.created) != 0 {
		t.Errorf("refusal still booted %d boxes: %v", len(b.created), b.created)
	}
	// Error() renders the headline and the per-provider line; the actionable part
	// lives in Remediation(), so that is where the command must appear.
	var unavail *sandbox.UnavailableError
	if !errors.As(err, &unavail) {
		t.Fatalf("error is %T (%v), want *sandbox.UnavailableError", err, err)
	}
	joined := strings.Join(unavail.Remediation(), " | ")
	if !strings.Contains(joined, "hno sandbox boundary") {
		t.Errorf("Remediation() %q does not tell the operator what to re-run", joined)
	}
}

// TestBoundaryBoxBudgetIsRefusedWithoutTouchingTheEngine mirrors the core guard at the
// provider boundary, because MeasureKernelBoundary is exported and callable directly.
func TestBoundaryBoxBudgetIsRefusedWithoutTouchingTheEngine(t *testing.T) {
	b := &boundaryScript{readings: map[string]string{}, imagePresent: true}
	fr := &fakeRunner{respond: b.respond}
	p, err := New(Config{Image: "golang:1.26", Runner: fr, StartupTimeout: 5e9})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, boxes := range []int{1, 7} {
		_, err := p.MeasureKernelBoundary(context.Background(), boxes)
		if _, ok := err.(*sandbox.BoundaryBudgetError); !ok {
			t.Errorf("boxes=%d: %v, want *sandbox.BoundaryBudgetError", boxes, err)
		}
	}
	if len(b.created) != 0 {
		t.Errorf("an invalid box count booted %d boxes", len(b.created))
	}
}

// TestBoundaryTeardownFailureIsReportedNotSwallowed: a measurement that cannot close
// its boxes has leaked VMs, and the cheapest way to hide that is a nil error.
func TestBoundaryTeardownFailureIsReportedNotSwallowed(t *testing.T) {
	b := &boundaryScript{
		readings: map[string]string{
			"box-1": reading("aaaa-1111", "6.51", "Linux 6.18.35"),
			"box-2": reading("bbbb-2222", "4.54", "Linux 6.18.35"),
		},
		imagePresent: true,
		closeFails:   true,
	}
	fr := &fakeRunner{respond: b.respond}
	p, err := New(Config{Image: "golang:1.26", Runner: fr, StartupTimeout: 5e9})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	report, err := p.MeasureKernelBoundary(context.Background(), 2)
	if err != nil {
		t.Fatalf("a teardown problem is reported in the report, not turned into an engine failure: %v", err)
	}
	joined := strings.Join(report.Fixes, " | ")
	if !strings.Contains(joined, "hno sandbox prune") {
		t.Errorf("Fixes %q do not tell the operator how to reclaim what this measurement could not close", joined)
	}
}

// TestBoundaryGuestKernelContrastIsRecorded keeps the independent signal in the
// evidence: a Linux guest on a non-Linux host already says a VM exists, so the reader
// can tell "we are inside a VM" from "we are inside a namespace".
func TestBoundaryGuestKernelContrastIsRecorded(t *testing.T) {
	b := &boundaryScript{readings: map[string]string{
		"box-1": reading("aaaa-1111", "6.51", "Linux 6.18.35"),
		"box-2": reading("bbbb-2222", "4.54", "Linux 6.18.35"),
	}, imagePresent: true}
	fr := &fakeRunner{respond: b.respond}
	p, err := New(Config{Image: "golang:1.26", Runner: fr, StartupTimeout: 5e9})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	report, err := p.MeasureKernelBoundary(context.Background(), 2)
	if err != nil {
		t.Fatalf("measure: %v", err)
	}
	if report.GuestKernel == "" || report.HostKernel == "" {
		t.Errorf("guest=%q host=%q: the contrast is the independent signal", report.GuestKernel, report.HostKernel)
	}
	joined := strings.Join(report.Evidence, " | ")
	if !strings.Contains(joined, "guest") && !strings.Contains(joined, "Guest") && !strings.Contains(joined, "kernel") {
		t.Errorf("evidence %q never mentions the kernel contrast", joined)
	}
}
