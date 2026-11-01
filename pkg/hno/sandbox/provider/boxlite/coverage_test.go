package boxlite

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rexleimo/agno-go/pkg/hno/sandbox"
)

func TestProbeFailsWhenTheEngineErrors(t *testing.T) {
	f := &fakeRunner{replies: map[string]Outcome{"info": {Exit: 2, Stderr: "cannot read config\n"}}}
	err := newTestProvider(t, f).Probe(context.Background())
	if err == nil {
		t.Fatal("Probe passed on a nonzero `info` exit")
	}
	if !strings.Contains(err.Error(), "exit") {
		t.Fatalf("error %q does not say the engine exited nonzero", err)
	}
}

func TestImageAvailableRefusals(t *testing.T) {
	p := newTestProvider(t, &fakeRunner{})
	if _, err := p.ImageAvailable(context.Background(), "  "); err == nil {
		t.Fatal("an empty image reference was answered instead of refused")
	}
	failing := &fakeRunner{replies: map[string]Outcome{"images": {Exit: 1, Stderr: "daemon down\n"}}}
	if _, err := newTestProvider(t, failing).ImageAvailable(context.Background(), "alpine"); err == nil {
		t.Fatal("a failing `images` call became an answer about the image")
	}
	broken := &fakeRunner{errs: map[string]error{"images": errors.New("spawn failed")}}
	if _, err := newTestProvider(t, broken).ImageAvailable(context.Background(), "alpine"); err == nil {
		t.Fatal("a runner error became an answer about the image")
	}
}

func TestEgressAllowlistIsAppliedPerRequest(t *testing.T) {
	f := &fakeRunner{replies: map[string]Outcome{
		"create":  {Stdout: "b0x1d\n"},
		"start":   {},
		"list":    {Stdout: "│ b0x1d │ alpine:latest │ Running │\n"},
		"network": {},
		"exec":    {Stdout: "ok\n"},
	}}
	p := newTestProvider(t, f)
	env, err := p.NewEnvironment(context.Background(), sandbox.EnvSpec{Image: "alpine:latest"})
	if err != nil {
		t.Fatalf("NewEnvironment: %v", err)
	}
	req := sandbox.Command("curl", "https://example.com")
	req.Egress = sandbox.Egress{Mode: sandbox.EgressAllowlist, Allow: []string{"example.com", "api.example.com"}}
	if _, err := env.Exec(context.Background(), sandbox.RunSpec{RunID: "r", Request: req}, nil); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	var network []string
	for _, call := range f.calls {
		if len(call) > 0 && call[0] == "network" {
			network = call
		}
	}
	if len(network) == 0 {
		t.Fatal("an allowlist request ran without touching the egress policy")
	}
	joined := strings.Join(network, " ")
	for _, want := range []string{"outbound", "b0x1d", "enabled", "--allow-net example.com", "--allow-net api.example.com"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("network call %q does not contain %q", joined, want)
		}
	}
}

func TestSweepIntersectsTheEngineWithLocalBookkeeping(t *testing.T) {
	f := &fakeRunner{replies: map[string]Outcome{
		"create": {Stdout: "b0x1d\n"},
		"start":  {},
		"list":   {Stdout: "│ b0x1d │ alpine:latest │ Running │\n│ other │ alpine:latest │ Running │\n"},
	}}
	p := newTestProvider(t, f)
	if _, err := p.NewEnvironment(context.Background(), sandbox.EnvSpec{Image: "alpine:latest"}); err != nil {
		t.Fatalf("NewEnvironment: %v", err)
	}
	got, err := p.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(got) != 1 || got[0] != "b0x1d" {
		t.Fatalf("Sweep = %v, want only the box this process created", got)
	}
}

func TestCloseDropsProviderBookkeeping(t *testing.T) {
	f := &fakeRunner{replies: map[string]Outcome{
		"create": {Stdout: "b0x1d\n"},
		"start":  {},
		"list":   {Stdout: "│ b0x1d │ alpine:latest │ Running │\n"},
	}}
	p := newTestProvider(t, f)
	if _, err := p.NewEnvironment(context.Background(), sandbox.EnvSpec{Image: "alpine:latest"}); err != nil {
		t.Fatalf("NewEnvironment: %v", err)
	}
	if err := p.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	got, err := p.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Sweep after Close = %v, want empty", got)
	}
}

// The measurement's teardown runs in a defer, and Go drops a deferred write to an
// anonymous return. This is the case that catch exists for: a box that cannot be closed
// must be reported, or the measurement looks clean while a VM is still running.
func TestMeasureKernelBoundaryReportsABoxItCouldNotClose(t *testing.T) {
	f := &fakeRunner{replies: map[string]Outcome{
		"images": {Stdout: "│ NAME   │ TAG    │\n│ alpine │ latest │\n"},
		"create": {Stdout: "b0x1d\n"},
		"start":  {},
		"list":   {Stdout: "│ b0x1d │ alpine:latest │ Running │\n"},
		// Each box reports a distinct boot id, so the verdict is private-kernel.
		"exec": {Stdout: "aaaa-1111\n0.5\nLinux 6.12.87\n"},
		"rm":   {Exit: 1, Stderr: "cannot remove\n"},
	}}
	p := newTestProvider(t, f)
	report, err := p.MeasureKernelBoundary(context.Background(), 2)
	if err != nil {
		t.Fatalf("MeasureKernelBoundary: %v", err)
	}
	var found bool
	for _, fix := range report.Fixes {
		if strings.Contains(fix, "could not be closed") {
			found = true
		}
	}
	if !found {
		t.Fatalf("a box that could not be closed was not reported; fixes = %v", report.Fixes)
	}
}

func TestMeasureKernelBoundaryRefusesABadBoxCount(t *testing.T) {
	f := &fakeRunner{}
	p := newTestProvider(t, f)
	for _, boxes := range []int{0, 1, sandbox.MaxBoundaryBoxes + 1} {
		if _, err := p.MeasureKernelBoundary(context.Background(), boxes); err == nil {
			t.Fatalf("boxes=%d was accepted", boxes)
		}
	}
	if len(f.calls) != 0 {
		t.Fatalf("a refused measurement still booted boxes: %v", f.calls)
	}
}

func TestMeasureKernelBoundaryNamesAnAbsentImage(t *testing.T) {
	f := &fakeRunner{replies: map[string]Outcome{"images": {Stdout: "│ NAME │ TAG │\n"}}}
	p := newTestProvider(t, f)
	report, err := p.MeasureKernelBoundary(context.Background(), 2)
	if err == nil {
		t.Fatalf("a measurement proceeded without its image: %+v", report)
	}
	var ue *sandbox.UnavailableError
	if !errors.As(err, &ue) {
		t.Fatalf("error is %T, want *sandbox.UnavailableError", err)
	}
	if rem := strings.Join(ue.Remediation(), " "); !strings.Contains(rem, "alpine:latest") {
		t.Fatalf("remediation %q does not name the missing image", rem)
	}
	for _, call := range f.calls {
		if len(call) > 0 && call[0] == "create" {
			t.Fatalf("a box was booted to discover the image was missing: %v", call)
		}
	}
}

func TestCommandForRefusesAnUnknownRuntime(t *testing.T) {
	if _, err := commandFor(sandbox.Request{Runtime: "ruby", Code: "puts 1"}); err == nil {
		t.Fatal("an interpreter template was invented for a runtime with no mapping")
	}
	if _, err := commandFor(sandbox.Request{}); err == nil {
		t.Fatal("a request with neither argv nor runtime was accepted")
	}
	if _, err := commandFor(sandbox.Request{Runtime: "shell"}); err == nil {
		t.Fatal("a shell runtime with no code was accepted; it would run an empty script")
	}
	argv, err := commandFor(sandbox.Request{Runtime: "python", Code: "print(1)"})
	if err != nil || len(argv) == 0 || argv[0] != "python3" {
		t.Fatalf("python template = %v, %v", argv, err)
	}
}

func TestPayloadForPrefersCodeOverInputs(t *testing.T) {
	req := sandbox.Request{Runtime: "shell", Code: "echo code"}
	if got := payloadFor(req); got != "echo code" {
		t.Fatalf("payload = %q, want the code", got)
	}
	if got := payloadFor(sandbox.Request{Runtime: "shell"}); got != "" {
		t.Fatalf("payload = %q for a request with neither code nor inputs", got)
	}
}

func TestParseListStatesToleratesEmptyAndPlainTables(t *testing.T) {
	if got := parseListStates("│ ID │ IMAGE │ STATUS │\n"); len(got) != 0 {
		t.Fatalf("a header-only table produced %v", got)
	}
	got := parseListStates("| b0x1d | alpine:latest | Stopped |\n")
	if got["b0x1d"] != "Stopped" {
		t.Fatalf("plain table parsed as %v", got)
	}
	if got := parseListStates("no table here\n"); len(got) != 0 {
		t.Fatalf("a banner line produced %v", got)
	}
}

// The production runner is exercised against /bin/sh: an unreadable dependency of the
// provider itself would otherwise be the one path no test covers.
func TestExecRunnerReportsExitCodesAndStreams(t *testing.T) {
	r := &execRunner{bin: "/bin/sh"}
	res, err := r.Run(context.Background(), Command{Args: []string{"-c", "echo out; echo err >&2; exit 5"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Exit != 5 {
		t.Fatalf("exit = %d, want 5", res.Exit)
	}
	if !strings.Contains(res.Stdout, "out") || !strings.Contains(res.Stderr, "err") {
		t.Fatalf("stdout %q / stderr %q did not capture both streams", res.Stdout, res.Stderr)
	}

	var chunks int
	res, err = r.Run(context.Background(), Command{
		Args:    []string{"-c", "printf 'a\\nb\\n'"},
		OnChunk: func(st sandbox.Stream, b []byte) { chunks++ },
	})
	if err != nil {
		t.Fatalf("Run with a chunk hook: %v", err)
	}
	if chunks == 0 || res.Streamed == 0 {
		t.Fatalf("chunks=%d streamed=%d, want live delivery counted", chunks, res.Streamed)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err = r.Run(ctx, Command{Args: []string{"-c", "sleep 5"}})
	if err == nil || !res.Canceled {
		t.Fatalf("a cancelled run returned err=%v canceled=%v; a cancel is not a failure", err, res.Canceled)
	}
}

func TestNewRefusesAnEmptyRegistryEntry(t *testing.T) {
	if _, err := New(Config{Runner: &fakeRunner{}, Registry: []string{""}}); err == nil {
		t.Fatal("an empty registry entry was accepted; it would produce a doubled slash in the image reference")
	}
}
