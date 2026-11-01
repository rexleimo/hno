package boxlite

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/rexleimo/agno-go/pkg/hno/sandbox"
)

// realOptions builds the option set the real gate runs under.
//
// Every Timeouts field is named rather than defaulted because WithTimeouts replaces the
// whole struct: a gate that leaves BoundaryFresh out measures with a zero freshness and
// then wonders why admission refuses its own measurement.
func realConfig(t *testing.T) Config {
	t.Helper()
	if os.Getenv("HNO_SANDBOX_BOXLITE") == "" {
		t.Skip("set HNO_SANDBOX_BOXLITE=1 to run the real boxlite gate")
	}
	cfg := Config{
		Bin:            envOr("HNO_SANDBOX_BOXLITE_BIN", "boxlite"),
		Home:           os.Getenv("HNO_SANDBOX_BOXLITE_HOME"),
		Image:          envOr("HNO_SANDBOX_BOXLITE_IMAGE", "alpine:latest"),
		StartupTimeout: 3 * time.Minute,
	}
	if regs := os.Getenv("HNO_SANDBOX_BOXLITE_REGISTRY"); regs != "" {
		cfg.Registry = strings.Split(regs, ",")
	}
	// The gate is opted into, so a missing binary is a failure of the gate rather than a
	// reason to pass: a skip here would mean deleting the install turns the gate green.
	if _, err := os.Stat(cfg.Bin); err != nil {
		if _, lookErr := lookPath(cfg.Bin); lookErr != nil {
			t.Fatalf("HNO_SANDBOX_BOXLITE is set but %q is not executable (%v); install it or point HNO_SANDBOX_BOXLITE_BIN at it", cfg.Bin, err)
		}
	}
	return cfg
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// TestRealBoxRoundTrip is the measured gate: a real engine, a real box, a real exit code.
func TestRealBoxRoundTrip(t *testing.T) {
	cfg := realConfig(t)
	p, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	if err := p.Probe(ctx); err != nil {
		t.Fatalf("Probe against the real engine: %v", err)
	}

	env, err := p.NewEnvironment(ctx, sandbox.EnvSpec{SessionID: "hno-real-gate", Image: cfg.Image})
	if err != nil {
		t.Fatalf("NewEnvironment: %v", err)
	}
	t.Logf("box %s", env.ID())
	defer func() {
		if err := env.Close(context.WithoutCancel(ctx)); err != nil {
			t.Fatalf("Close left the box behind: %v", err)
		}
	}()

	rep, err := env.Exec(ctx, sandbox.RunSpec{
		RunID:   "real-gate-echo",
		Request: sandbox.Shell(`uname -sr; echo payload-ok`),
	}, nil)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if rep.ExitCode != 0 {
		t.Fatalf("exec exited %d: %s", rep.ExitCode, rep.Stderr)
	}
	if !strings.Contains(rep.Stdout, "payload-ok") {
		t.Fatalf("stdout = %q, want the payload's output", rep.Stdout)
	}
	// A Linux guest on this host is the VM evidence; the boundary gate then asks whether
	// it is one per box.
	if !strings.Contains(rep.Stdout, "Linux") {
		t.Fatalf("stdout = %q, want a Linux guest kernel", rep.Stdout)
	}

	// Exit-code propagation is measured, not assumed: a toolkit that cannot see exit 7
	// cannot tell a failing test from a passing one.
	fail, err := env.Exec(ctx, sandbox.RunSpec{RunID: "real-gate-exit", Request: sandbox.Shell("exit 7")}, nil)
	if err != nil {
		t.Fatalf("Exec with a nonzero payload returned an error: %v", err)
	}
	if fail.ExitCode != 7 {
		t.Fatalf("payload `exit 7` reported exit %d", fail.ExitCode)
	}
	if fail.Failure == nil || fail.Failure.Class != sandbox.ClassUserCode {
		t.Fatalf("failure = %+v, want ClassUserCode", fail.Failure)
	}
}

// TestRealKernelBoundary measures the tier this provider claims, using the same probe the
// core uses for admission (D-38/D-39). A provider claiming IsolationMicroVM that shares a
// kernel is exactly the over-claim those decisions exist to catch.
func TestRealKernelBoundary(t *testing.T) {
	cfg := realConfig(t)
	p, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	// The image question first: a measurement that provisions boxes to discover the image
	// is missing has wasted the boxes.
	if present, qerr := p.ImageAvailable(ctx, cfg.Image); qerr != nil || !present {
		t.Fatalf("ImageAvailable(%q) = %v, %v; the boundary gate needs a present image", cfg.Image, present, qerr)
	}

	report, err := p.MeasureKernelBoundary(ctx, 2)
	if err != nil {
		t.Fatalf("MeasureKernelBoundary: %v", err)
	}
	if report.Provider != ProviderName {
		t.Fatalf("report.Provider = %q; provenance is what makes this evidence about this engine", report.Provider)
	}
	if len(report.BootIDs) != 2 {
		t.Fatalf("boot ids = %v, want one per box", report.BootIDs)
	}
	if report.BootIDs[0] == report.BootIDs[1] {
		t.Fatalf("both boxes report boot_id %s: this engine shares a kernel, so IsolationMicroVM is an over-claim", report.BootIDs[0])
	}
	if report.Verdict != sandbox.BoundaryPrivateKernel {
		t.Fatalf("verdict = %s, want %s; evidence: %v", report.Verdict, sandbox.BoundaryPrivateKernel, report.Evidence)
	}
	if !strings.HasPrefix(strings.ToLower(report.GuestKernel), "linux") {
		t.Fatalf("guest kernel = %q, want a Linux guest", report.GuestKernel)
	}
	for _, fix := range report.Fixes {
		t.Logf("fix: %s", fix)
	}
	for _, ev := range report.Evidence {
		t.Logf("evidence: %s", ev)
	}
}

// TestRealBoundaryLeavesNothingBehind checks the failure mode a measurement must not have:
// leaking the boxes it just provisioned.
func TestRealBoundaryLeavesNothingBehind(t *testing.T) {
	cfg := realConfig(t)
	p, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	before, err := p.boxState(ctx)
	if err != nil {
		t.Fatalf("boxState: %v", err)
	}
	if _, err := p.MeasureKernelBoundary(ctx, 2); err != nil {
		t.Fatalf("MeasureKernelBoundary: %v", err)
	}
	after, err := p.boxState(ctx)
	if err != nil {
		t.Fatalf("boxState: %v", err)
	}
	for id := range after {
		if _, existed := before[id]; !existed {
			t.Fatalf("the measurement left box %s behind", id)
		}
	}
}

func lookPath(bin string) (string, error) {
	for _, dir := range strings.Split(os.Getenv("PATH"), string(os.PathListSeparator)) {
		candidate := dir + string(os.PathSeparator) + bin
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	return "", errors.New("not found in PATH")
}
