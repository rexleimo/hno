package run

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rexleimo/agno-go/pkg/hno/sandbox"
	"github.com/rexleimo/agno-go/pkg/hno/sandbox/sandboxtest"
)

// memoryBacked opens the adapter against the in-memory double, which is how the mapping is
// tested without an engine. It works because cfg.Options is appended after the adapter's own
// defaults: the same override a caller uses to name a different registered provider.
func memoryBacked(t *testing.T, extra ...sandbox.Option) (Executor, *int) {
	t.Helper()
	if err := sandboxtest.Register(); err != nil {
		t.Fatalf("register: %v", err)
	}
	runs := new(int)
	var mu sync.Mutex
	// The observer counts engine-side runs so a refusal can be proven to have stopped short
	// of execution rather than running anyway and reporting an error afterwards.
	count := sandbox.WithObserver(sandbox.ObserverFunc(func(_ context.Context, ev sandbox.Event) error {
		if ev.Kind == sandbox.EventPhaseStarted {
			mu.Lock()
			*runs++
			mu.Unlock()
		}
		return nil
	}))
	opts := append([]sandbox.Option{
		sandbox.WithProvider("memory"),
		sandbox.WithAllowedProviders("memory"),
		count,
	}, extra...)
	ex, err := NewSandboxExecutor(SandboxConfig{
		Config:  Config{Image: "test:latest"},
		Options: opts,
	})
	if err != nil {
		t.Fatalf("NewSandboxExecutor: %v", err)
	}
	t.Cleanup(func() { _ = ex.Close() })
	return ex, runs
}

// TestAdapterRunsAPluginSpec proves the adapter is a real Executor: the legacy shape goes in,
// the legacy shape comes out, and the values are the sandbox's.
func TestAdapterRunsALegacySpec(t *testing.T) {
	ctx := context.Background()
	ex, runs := memoryBacked(t)

	res, err := ex.Run(ctx, Spec{Runtime: "shell", Code: "export ADAPTER=1"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("want a successful run, got %+v", res)
	}
	if res.Duration <= 0 {
		t.Fatalf("Duration must be reported, got %s", res.Duration)
	}
	if *runs != 1 {
		t.Fatalf("exactly one engine run expected, got %d", *runs)
	}
}

// TestAdapterFailingCommandIsAResult keeps the legacy shape where a non-zero exit is returned
// as data with a nil error, not folded into an error.
func TestAdapterFailingCommandIsAResult(t *testing.T) {
	ctx := context.Background()
	ex, _ := memoryBacked(t)

	res, err := ex.Run(ctx, Spec{Runtime: "shell", Code: "#!fail boom"})
	if err != nil {
		t.Fatalf("a failing command must not become an error: %v", err)
	}
	if res.ExitCode == 0 {
		t.Fatal("the non-zero exit code must reach the caller")
	}
}

// TestAdapterRejectsWorkspaceMount is the semantics-preservation gate: the legacy field is a
// live read-write bind mount, and quietly staging a snapshot instead would leave a caller's
// host directory un-updated while reporting success.
func TestAdapterRejectsWorkspaceMount(t *testing.T) {
	ctx := context.Background()
	ex, runs := memoryBacked(t)

	res, err := ex.Run(ctx, Spec{
		Runtime:   "shell",
		Code:      "export ANY=1",
		Workspace: "/tmp/does-not-matter",
	})
	if !errors.Is(err, ErrSandboxUnsupported) {
		t.Fatalf("want ErrSandboxUnsupported, got %v", err)
	}
	if *runs != 0 {
		t.Fatalf("a refusal must happen before the engine, saw %d runs", *runs)
	}
	if res != (Result{}) {
		t.Fatalf("a refused spec must not report a run, got %+v", res)
	}
	// The message has to say what to do instead, or the caller only learns the field broke.
	for _, want := range []string{"bind mount", "RunProject"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the error should name the change and the replacement, missing %q in %q", want, err)
		}
	}
}

// TestAdapterRejectsLimitMismatch pins the second落差: per-run memory ceilings are not
// expressible, so a Spec asking for a different one must not be accepted and ignored.
func TestAdapterRejectsLimitMismatch(t *testing.T) {
	ctx := context.Background()
	ex, runs := memoryBacked(t)

	// 2 GiB asked, 512 MiB enforced: accepting it would promise a bound the engine never got.
	_, err := ex.Run(ctx, Spec{Runtime: "shell", Code: "export A=1", MemoryLimit: 2 << 30})
	if !errors.Is(err, ErrSandboxLimitMismatch) {
		t.Fatalf("want ErrSandboxLimitMismatch, got %v", err)
	}
	if *runs != 0 {
		t.Fatalf("a limit refusal must happen before the engine, saw %d runs", *runs)
	}
	if !strings.Contains(err.Error(), "Limits.MemoryMB") {
		t.Fatalf("the error must name where to configure it, got %q", err)
	}

	// A matching number is accepted, and so is the zero value meaning "use the default".
	if _, err := ex.Run(ctx, Spec{Runtime: "shell", Code: "export B=1", MemoryLimit: DefaultMemoryLimit}); err != nil {
		t.Fatalf("the enforced limit should be accepted as-is: %v", err)
	}
	if _, err := ex.Run(ctx, Spec{Runtime: "shell", Code: "export C=1", PidLimit: 9999}); !errors.Is(err, ErrSandboxLimitMismatch) {
		t.Fatalf("pid limits are the same problem, got %v", err)
	}
}

// TestAdapterHonoursConfiguredLimits shows the refusal is about the mismatch, not about the
// number: a handle constructed with 1 GiB accepts a 1 GiB Spec.
func TestAdapterHonoursConfiguredLimits(t *testing.T) {
	ctx := context.Background()
	ex, _ := memoryBacked(t, sandbox.WithLimits(sandbox.Limits{MemoryMB: 1024, PidsLimit: 64, OutputMaxBytes: DefaultOutputLimit}))
	if _, err := ex.Run(ctx, Spec{Runtime: "shell", Code: "export A=1", MemoryLimit: 1024 << 20, PidLimit: 64}); err != nil {
		t.Fatalf("configured limits should line up: %v", err)
	}
}

// TestAdapterPreservesLegacyValidation keeps the error text callers already match on:
// unknown runtime and empty code are rejected by this package, before any sandbox call.
func TestAdapterPreservesLegacyValidation(t *testing.T) {
	ctx := context.Background()
	ex, runs := memoryBacked(t)

	_, err := ex.Run(ctx, Spec{Runtime: "ruby", Code: "puts 1"})
	if err == nil || !strings.Contains(err.Error(), "unknown sandbox runtime") {
		t.Fatalf("legacy validation text must survive, got %v", err)
	}
	if _, err := ex.Run(ctx, Spec{Runtime: "shell", Code: "   "}); err == nil || !strings.Contains(err.Error(), "non-empty code") {
		t.Fatalf("empty payload must still be rejected here, got %v", err)
	}
	if _, err := ex.Run(ctx, Spec{Runtime: "shell", Code: "export A=1", Env: []string{"=nope"}}); err == nil {
		t.Fatal("malformed env entries must still be rejected here")
	}
	if *runs != 0 {
		t.Fatalf("invalid specs must never reach the engine, saw %d runs", *runs)
	}
}

// TestAdapterBackendRefusal checks no provider substitution: a caller asking for podman or
// e2b is told the sandbox runtime has no such provider, rather than silently getting docker.
func TestAdapterBackendRefusal(t *testing.T) {
	for _, backend := range []string{"podman", "e2b"} {
		_, err := NewSandboxExecutor(SandboxConfig{Config: Config{Backend: backend}})
		if !errors.Is(err, ErrSandboxUnsupported) {
			t.Fatalf("backend %q: want ErrSandboxUnsupported, got %v", backend, err)
		}
		if strings.Contains(err.Error(), "docker") && backend == "podman" {
			t.Fatalf("the refusal must not read like a docker error: %q", err)
		}
	}
	_, err := NewSandboxExecutor(SandboxConfig{Config: Config{Backend: "nomad"}})
	if !errors.Is(err, ErrUnsupportedBackend) {
		t.Fatalf("an unknown backend name should still be the legacy sentinel, got %v", err)
	}
}

// TestAdapterMapsMissingEngine preserves ErrNoProvider, the sentinel callers already treat as
// an execution denial.
func TestAdapterMapsMissingEngine(t *testing.T) {
	_, err := NewSandboxExecutor(SandboxConfig{
		Open: func(_ context.Context, _ ...sandbox.Option) (*sandbox.Sandbox, error) {
			return nil, &sandbox.UnavailableError{Reason: "no container runtime on PATH"}
		},
		Options: []sandbox.Option{sandbox.WithAllowedProviders("memory")},
	})
	if !errors.Is(err, ErrNoProvider) {
		t.Fatalf("want ErrNoProvider, got %v", err)
	}
	if !strings.Contains(err.Error(), "no container runtime") {
		t.Fatalf("the engine's own reason should be quoted, got %q", err)
	}
}

// TestAdapterKeepsBothTimeoutSignals: legacy callers test err and the TimedOut flag, so
// delivering only one breaks half of them.
func TestAdapterKeepsBothTimeoutSignals(t *testing.T) {
	ctx := context.Background()
	ex, _ := memoryBacked(t)

	cctx, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
	defer cancel()
	res, err := ex.Run(cctx, Spec{Runtime: "shell", Code: "#!timeout", Timeout: 10 * time.Millisecond})
	if err == nil {
		t.Skip("the in-memory double completes instantly; timeout mapping is covered by the deadline case below")
	}
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("want ErrTimeout, got %v", err)
	}
	if !res.TimedOut {
		t.Fatalf("the flag must be set alongside the error, got %+v", res)
	}
}

// TestAdapterConcurrentRuns pins the Executor contract that implementations are safe to call
// from goroutines.
func TestAdapterConcurrentRuns(t *testing.T) {
	ctx := context.Background()
	ex, runs := memoryBacked(t)

	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := ex.Run(ctx, Spec{Runtime: "shell", Code: "export A=1"})
			errs[i] = err
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent run %d: %v", i, err)
		}
	}
	if *runs != 8 {
		t.Fatalf("all eight runs should have reached the engine, got %d", *runs)
	}
}

// TestAdapterCloseIsIdempotent keeps Close() safe on a nil handle, which a caller that failed
// construction may still reach.
func TestAdapterCloseIsIdempotent(t *testing.T) {
	ex, _ := memoryBacked(t)
	if err := ex.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	var nilExec *sandboxExecutor
	if err := nilExec.Close(); err != nil {
		t.Fatalf("nil Close should be a no-op, got %v", err)
	}
}

// TestMapRunOutcomeWithoutAReport covers the branch the engine path cannot reach: a provider
// that is killed by its deadline and returns no report at all. Dropping the flag there is how
// a caller that tests res.TimedOut instead of the error misses that its job was killed.
func TestMapRunOutcomeWithoutAReport(t *testing.T) {
	res, err := mapRunOutcome(nil, context.DeadlineExceeded, Spec{Runtime: "shell", Code: "x"}, 30*time.Second)
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("want ErrTimeout, got %v", err)
	}
	if !res.TimedOut {
		t.Fatalf("the flag must be set even with no report, got %+v", res)
	}
	if res.ExitCode == 0 {
		t.Fatalf("an unreported exit code must not read as success, got %d", res.ExitCode)
	}
	// A report that already says TimedOut keeps its own numbers.
	withReport := &sandbox.Report{ExitCode: 124, Stderr: "killed", TimedOut: true, Duration: time.Second}
	res2, err2 := mapRunOutcome(withReport, context.DeadlineExceeded, Spec{Timeout: 5 * time.Second}, 30*time.Second)
	if !res2.TimedOut || res2.ExitCode != 124 || res2.Stderr != "killed" {
		t.Fatalf("the report's own fields must survive, got %+v", res2)
	}
	if !strings.Contains(err2.Error(), "5s") {
		t.Fatalf("the Spec's own deadline should be quoted, got %q", err2)
	}
	// And a non-timeout error stays itself, unwrapped by us.
	res3, err3 := mapRunOutcome(nil, errors.New("engine exploded"), Spec{}, time.Second)
	if errors.Is(err3, ErrTimeout) {
		t.Fatal("a generic engine failure must not be relabelled a timeout")
	}
	if res3.ExitCode != -1 {
		t.Fatalf("no report means no observed exit code, got %d", res3.ExitCode)
	}
}
