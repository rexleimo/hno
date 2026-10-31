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

// openHandle opens a memory-provider handle for a test.
func openHandle(t *testing.T, opts ...sandbox.Option) *sandbox.Sandbox {
	t.Helper()
	if err := sandboxtest.Register(); err != nil {
		t.Fatalf("register provider: %v", err)
	}
	base := []sandbox.Option{
		sandbox.WithProvider("memory"),
		sandbox.WithAllowedProviders("memory"),
		sandbox.WithImage("test:latest"),
	}
	sb, err := sandbox.Open(context.Background(), append(base, opts...)...)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = sb.Close() })
	return sb
}

// TestProbeResolveValidateRun covers the B1 core gate: a run goes through
// probe, resolve, validate and run, and returns a Report even when refused.
func TestProbeResolveValidateRun(t *testing.T) {
	ctx := context.Background()
	sb := openHandle(t)

	if err := sb.Probe(ctx); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	st := sb.Status()
	if st.Provider != "memory" {
		t.Fatalf("Status().Provider = %q, want memory", st.Provider)
	}
	if !st.Capabilities.Has(sandbox.CapSession) {
		t.Fatalf("Status().Capabilities missing CapSession: %v", st.Capabilities.Strings())
	}

	rep, err := sb.Run(ctx, sandbox.Request{Runtime: "sh", Code: "#!echo hello"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.Stdout != "hello\n" {
		t.Fatalf("Stdout = %q, want %q", rep.Stdout, "hello\n")
	}
	if rep.ExitCode != 0 || rep.RunID == "" || rep.Provider == "" {
		t.Fatalf("report incomplete: %+v", rep)
	}
}

// TestValidationFailsBeforeProvider proves validation runs before any engine is
// touched: a bad request costs nothing and returns a typed error.
func TestValidationFailsBeforeProvider(t *testing.T) {
	ctx := context.Background()
	sb := openHandle(t)

	cases := []struct {
		name string
		req  sandbox.Request
	}{
		{"empty", sandbox.Request{}},
		{"absolute cwd", sandbox.Request{Runtime: "sh", Code: "x", Cwd: "/etc", Workspace: sandbox.FromImage("i")}},
		{"parent traversal input", sandbox.Request{Runtime: "sh", Code: "x", Inputs: []sandbox.Input{{Path: "../escape", Bytes: []byte("x")}}}},
		{"inline over limit", sandbox.Request{Runtime: "sh", Code: "#!echo x", Files: map[string]sandbox.FileContent{"big": {Content: []byte(strings.Repeat("a", 8<<20))}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rep, err := sb.Run(ctx, tc.req)
			if err == nil {
				t.Fatalf("expected error, got report %+v", rep)
			}
			if rep != nil && rep.Provider == "" {
				t.Fatalf("refused runs must still identify the provider: %+v", rep)
			}
		})
	}
}

// TestPolicyDenyReturnsReportAndError is the regression the old toolkit shipped
// with: a refusal must surface as an error AND a report, never a nil error with
// an "error" key in a map (§10, §16.1).
func TestPolicyDenyReturnsReportAndError(t *testing.T) {
	ctx := context.Background()
	sb := openHandle(t, sandbox.WithPolicy(sandbox.PolicyFunc(func(_ context.Context, rc *sandbox.RunContext) (sandbox.Decision, error) {
		if rc.Request.Runtime == "sh" {
			return sandbox.Deny("sh is not permitted in this profile"), nil
		}
		return sandbox.Allow(), nil
	})))

	rep, err := sb.Run(ctx, sandbox.Request{Runtime: "sh", Code: "#!echo hi"})
	if err == nil {
		t.Fatal("expected error for denied run")
	}
	var denied *sandbox.DeniedError
	if !errors.As(err, &denied) {
		t.Fatalf("err = %T %v, want *sandbox.DeniedError", err, err)
	}
	if rep == nil {
		t.Fatal("denied run must still return a Report for audit and metrics")
	}
	if rep.Failure == nil || rep.Failure.Class != sandbox.ClassDenied {
		t.Fatalf("Failure = %+v, want class denied", rep.Failure)
	}
}

// TestPolicyMayOnlyTighten checks the D-rule that hooks never widen privileges.
func TestPolicyMayOnlyTighten(t *testing.T) {
	ctx := context.Background()
	sb := openHandle(t, sandbox.WithPolicy(sandbox.PolicyFunc(func(_ context.Context, rc *sandbox.RunContext) (sandbox.Decision, error) {
		rc.Request.Egress.Mode = sandbox.EgressOpen // widening attempt
		return sandbox.Allow(), nil
	})))

	_, err := sb.Run(ctx, sandbox.Request{Runtime: "sh", Code: "#!echo x", Egress: sandbox.Egress{Mode: sandbox.EgressAllowlist, Allow: []string{"pypi.org"}}})
	if err == nil {
		t.Fatal("expected the core to reject a policy that loosened the request")
	}
	if !strings.Contains(err.Error(), "tighten") {
		t.Fatalf("err = %v, want tighten-only explanation", err)
	}
}

// TestPolicyPanicFailsClosed proves a panicking guard stops the run instead of
// letting an unchecked request through (§7).
func TestPolicyPanicFailsClosed(t *testing.T) {
	ctx := context.Background()
	sb := openHandle(t, sandbox.WithPolicy(sandbox.PolicyFunc(func(_ context.Context, _ *sandbox.RunContext) (sandbox.Decision, error) {
		panic("guard crashed")
	})))
	_, err := sb.Run(ctx, sandbox.Request{Runtime: "sh", Code: "#!echo x"})
	var fault *sandbox.HookFaultError
	if !errors.As(err, &fault) {
		t.Fatalf("err = %T %v, want *sandbox.HookFaultError", err, err)
	}
}

// TestFailureClassificationIsStable pins the class mapping the graph retry
// predicate depends on (§16.2).
func TestFailureClassificationIsStable(t *testing.T) {
	cases := []struct {
		err  error
		want sandbox.FailureClass
	}{
		{&sandbox.DeniedError{Policy: "p"}, sandbox.ClassDenied},
		{&sandbox.UnavailableError{Tried: []sandbox.Attempt{{Provider: "x"}}}, sandbox.ClassPermanent},
		{&sandbox.ProvisionError{Provider: "memory", Stage: "boot"}, sandbox.ClassTransient},
		{&sandbox.QuotaError{Scope: "host", Limit: 4, Active: 4}, sandbox.ClassResource},
		{&sandbox.LostError{RunID: "r"}, sandbox.ClassLost},
		{context.DeadlineExceeded, sandbox.ClassTimeout},
		{&sandbox.CapabilityError{Provider: "memory"}, sandbox.ClassPermanent},
	}
	for _, tc := range cases {
		if got := sandbox.ClassOf(tc.err); got != tc.want {
			t.Errorf("ClassOf(%T) = %v, want %v", tc.err, got, tc.want)
		}
	}
	if sandbox.ClassOf(&sandbox.DeniedError{}).IsModelRecoverable() {
		t.Error("a refusal is not something the model fixes by re-running the same request")
	}
	if !sandbox.ClassOf(&sandbox.QuotaError{Scope: "host", Limit: 4, Active: 4}).RetryableByClass() {
		t.Error("waiting for capacity is not an immediate retry")
	}
}

// TestRetryBudgetPerClass pins the "retries are bounded per class" rule and the
// special case where an external effect may never auto-retry (§16.3).
func TestRetryBudgetPerClass(t *testing.T) {
	b := sandbox.RetryBudget{
		MaxAttempts: 3,
		PerAttempt:  5 * time.Second,
		WallClock:   30 * time.Second,
		ClassBudget: map[sandbox.FailureClass]int{
			sandbox.ClassTransient: 3,
			sandbox.ClassResource:  1,
		},
	}
	if b.BudgetFor(sandbox.ClassTransient) != 3 {
		t.Fatalf("transient budget = %d, want 3", b.BudgetFor(sandbox.ClassTransient))
	}
	if b.BudgetFor(sandbox.ClassResource) != 1 {
		t.Fatalf("resource budget = %d, want 1", b.BudgetFor(sandbox.ClassResource))
	}
	if b.BudgetFor(sandbox.ClassUserCode) != 0 {
		t.Fatal("user code never gets an automatic retry budget")
	}
	external := sandbox.Request{Runtime: "sh", Code: "#!echo x", Effect: sandbox.EffectExternal}
	if external.RetryAllowed(sandbox.ClassTransient) {
		t.Fatal("an external effect must not be auto-retried; use AtMostOnce plus a journal key")
	}
	idem := external
	idem.Delivery = sandbox.DeliveryAtMostOnce
	idem.IdempotencyKey = "order-42"
	if !idem.RetryAllowed(sandbox.ClassTransient) {
		t.Fatal("with an idempotency key the same effect may be re-attempted safely")
	}
}

// TestIdempotencyReplayUsesJournal proves the AtMostOnce contract: a replayed
// key returns the recorded outcome instead of running twice (§16.3).
func TestIdempotencyReplayUsesJournal(t *testing.T) {
	ctx := context.Background()
	journal := sandbox.NewMemoryJournal()
	sb := openHandle(t, sandbox.WithJournal(journal))

	req := sandbox.Request{Runtime: "sh", Code: "#!echo once", IdempotencyKey: "k1", Delivery: sandbox.DeliveryAtMostOnce}
	first, err := sb.Run(ctx, req)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	second, err := sb.Run(ctx, req)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if !second.Republished {
		t.Fatalf("second run should be a replay: %+v", second)
	}
	if second.RepublishedRunID != "" && second.RepublishedRunID != first.RunID {
		t.Fatalf("replay points at %q, want %q", second.RepublishedRunID, first.RunID)
	}

	conflict := req
	conflict.Code = "#!echo different"
	if _, err := sb.Run(ctx, conflict); err == nil {
		t.Fatal("same key with a different payload must be an idempotency conflict")
	} else {
		var ice *sandbox.IdempotencyConflictError
		if !errors.As(err, &ice) {
			t.Fatalf("err = %T, want *sandbox.IdempotencyConflictError", err)
		}
	}
}

// TestJournallessAtMostOnceFailsClosed proves that claiming exactly-once without
// a journal is rejected rather than silently downgraded (§16.3).
func TestJournallessAtMostOnceFailsClosed(t *testing.T) {
	ctx := context.Background()
	sb := openHandle(t)
	_, err := sb.Run(ctx, sandbox.Request{Runtime: "sh", Code: "#!echo x", IdempotencyKey: "k", Delivery: sandbox.DeliveryAtMostOnce})
	if err == nil {
		t.Fatal("expected failure: a delivery promise without a journal store")
	}
	if !strings.Contains(err.Error(), "journal") {
		t.Fatalf("err = %v, want it to name the missing journal", err)
	}
}

// TestSessionExecPersistence covers the stateful shape: one environment, many
// runs, declared steps recorded into a replayable spec (§6.4, §17.1).
func TestSessionExecPersistence(t *testing.T) {
	ctx := context.Background()
	sb := openHandle(t)

	sess, err := sb.Session(ctx, "conv-1", sandbox.WithCreateIfMissing(true))
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if _, err := sess.Exec(ctx, sandbox.Request{Runtime: "sh", Code: "#!echo a"}); err != nil {
		t.Fatalf("first exec: %v", err)
	}
	second, err := sess.Exec(ctx, sandbox.Request{Runtime: "sh", Code: "#!echo b"})
	if err != nil {
		t.Fatalf("second exec: %v", err)
	}
	if second.SessionID != "conv-1" {
		t.Fatalf("SessionID = %q, want conv-1", second.SessionID)
	}

	// Install goes through the sanctioned vocabulary and lands in the spec.
	if err := sess.Install(ctx, sandbox.Pip("requests==2.31.0")); err != nil {
		t.Fatalf("Install: %v", err)
	}
	spec := sess.Spec()
	if len(spec.Steps) == 0 {
		t.Fatal("Install must be recorded in the replayable spec")
	}
	found := false
	for _, s := range spec.Steps {
		if s.Kind != sandbox.StepInstall {
			continue
		}
		// The step records the exact argv, so a replay cannot drift from what
		// actually ran by re-guessing flags from a tool name (§17.1).
		joined := strings.Join(s.Argv, " ")
		if strings.Contains(joined, "pip install") && strings.Contains(joined, "requests==2.31.0") {
			found = true
		}
	}
	if !found {
		t.Fatalf("spec steps = %+v, want a recorded pip install", spec.Steps)
	}

	if err := sess.Release(ctx); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if _, err := sess.Exec(ctx, sandbox.Request{Runtime: "sh", Code: "#!echo c"}); err == nil {
		t.Fatal("exec after release must fail closed")
	}
}

// TestSessionBusyIsTypedError checks D-06's serial default: a second concurrent
// exec is refused, not silently serialized (§6.4).
func TestSessionBusyIsTypedError(t *testing.T) {
	ctx := context.Background()
	sb := openHandle(t)
	sess, err := sb.Session(ctx, "conv-busy", sandbox.WithCreateIfMissing(true))
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	// Occupy the session manually to make the test deterministic.
	if !sess.Occupy() {
		t.Fatal("expected to acquire the session")
	}
	_, err = sess.Exec(ctx, sandbox.Request{Runtime: "sh", Code: "#!echo x"})
	var busy *sandbox.StateError
	if !errors.As(err, &busy) {
		sess.Vacate()
		t.Fatalf("err = %T %v, want a busy-style state error", err, err)
	}
	sess.Vacate()
}

// TestArtifactsRequiredAndOverflow pins D-17: required artefacts fail loudly on
// overflow, optional ones are truncated and flagged.
func TestArtifactsRequiredAndOverflow(t *testing.T) {
	ctx := context.Background()
	sb := openHandle(t)
	sess, err := sb.Session(ctx, "art", sandbox.WithCreateIfMissing(true))
	if err != nil {
		t.Fatalf("Session: %v", err)
	}

	// Write a small report inside the workspace, then declare it as output.
	if _, err := sess.Exec(ctx, sandbox.Request{Runtime: "sh", Code: "#!write reports/junit.xml <testsuite tests=\"3\"/>"}); err != nil {
		t.Fatalf("write artefact: %v", err)
	}
	rep, err := sess.Exec(ctx, sandbox.Request{
		Runtime: "sh",
		Code:    "#!echo done",
		Outputs: []sandbox.ArtifactSpec{{Path: "reports/junit.xml", Kind: sandbox.KindFile, Required: true}},
	})
	if err != nil {
		t.Fatalf("collect run: %v", err)
	}
	a, ok := rep.Artifacts.Get("reports/junit.xml")
	if !ok {
		t.Fatalf("artefacts = %v, want junit.xml", rep.Artifacts.Names())
	}
	data, err := a.Bytes(ctx)
	if err != nil {
		t.Fatalf("Bytes: %v", err)
	}
	if !strings.Contains(string(data), "testsuite") {
		t.Fatalf("artefact content = %q", data)
	}

	// Overflow: required + oversized is a loud failure.
	big, err := sb.Session(ctx, "art-big", sandbox.WithCreateIfMissing(true))
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if _, err := big.Exec(ctx, sandbox.Request{Runtime: "sh", Code: "#!write big.txt " + strings.Repeat("x", 4096)}); err != nil {
		t.Fatalf("write big: %v", err)
	}
	_, err = big.Exec(ctx, sandbox.Request{
		Runtime: "sh",
		Code:    "#!echo done",
		Outputs: []sandbox.ArtifactSpec{{Path: "big.txt", Kind: sandbox.KindFile, Required: true, MaxBytes: 128}},
	})
	var artErr *sandbox.ArtifactError
	if !errors.As(err, &artErr) {
		t.Fatalf("err = %T %v, want *sandbox.ArtifactError", err, err)
	}
	if artErr.Reason != "too-large" {
		t.Fatalf("reason = %q, want too-large", artErr.Reason)
	}

	// Optional overflow is delivered truncated and flagged, not lost silently.
	_, err = big.Exec(ctx, sandbox.Request{Runtime: "sh", Code: "#!echo done", Outputs: []sandbox.ArtifactSpec{{Path: "big.txt", Kind: sandbox.KindFile, MaxBytes: 128}}})
	if err != nil {
		t.Fatalf("optional overflow should not fail the run: %v", err)
	}
}

// TestMissingRequiredArtifact covers the "build failed but the test report is
// the point" inverse: a required report that never appeared is an error (§13.3).
func TestMissingRequiredArtifact(t *testing.T) {
	ctx := context.Background()
	sb := openHandle(t)
	rep, err := sb.Run(ctx, sandbox.Request{
		Runtime: "sh",
		Code:    "#!echo done",
		Outputs: []sandbox.ArtifactSpec{{Path: "reports/nope.xml", Kind: sandbox.KindFile, Required: true}},
	})
	if err == nil {
		t.Fatalf("expected missing-artefact failure, got %+v", rep)
	}
	if rep == nil {
		t.Fatal("a failed run must still return a Report")
	}
}

// TestStreamingSinksAndOverflow proves sinks are non-blocking by contract and
// that a broken sink is a fault rather than a hung run (§7).
func TestStreamingSinksAndOverflow(t *testing.T) {
	ctx := context.Background()
	var got []string
	sb := openHandle(t, sandbox.WithSink("capture", sandbox.SinkFunc(func(_ context.Context, c sandbox.OutputChunk) error {
		got = append(got, string(c.Bytes))
		return nil
	})))
	if _, err := sb.Run(ctx, sandbox.Request{Runtime: "sh", Code: "#!echo line1"}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(got) == 0 || got[0] != "line1\n" {
		t.Fatalf("sink received %v", got)
	}

	// A blocking consumer with OverflowDrop must not stall the run.
	faults := 0
	sb2 := openHandle(t, sandbox.WithSink("bad", sandbox.SinkFunc(func(context.Context, sandbox.OutputChunk) error {
		faults++
		return errors.New("backpressure")
	}), sandbox.WithOverflow(sandbox.OverflowDrop)))
	if _, err := sb2.Run(ctx, sandbox.Request{Runtime: "sh", Code: "#!echo line1"}); err != nil {
		t.Fatalf("dropping sink must not fail the run: %v", err)
	}
	if faults == 0 {
		t.Fatal("expected the sink to be called")
	}
}

// TestObserverEventsCoverLifecycle checks the event vocabulary the DX guide
// promises: boot, phase, cleanup all observable (§6.3, §7).
func TestObserverEventsCoverLifecycle(t *testing.T) {
	ctx := context.Background()
	var kinds []sandbox.EventKind
	sb := openHandle(t, sandbox.WithObserver(sandbox.ObserverFunc(func(_ context.Context, ev sandbox.Event) error {
		kinds = append(kinds, ev.Kind)
		return nil
	})))
	if _, err := sb.Run(ctx, sandbox.Request{Runtime: "sh", Code: "#!echo x"}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if err := sb.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	want := map[sandbox.EventKind]bool{sandbox.EventReady: false, sandbox.EventPhaseStarted: false, sandbox.EventPhaseFinished: false, sandbox.EventCleaned: false}
	for _, k := range kinds {
		if _, ok := want[k]; ok {
			want[k] = true
		}
	}
	for k, seen := range want {
		if !seen {
			t.Errorf("missing lifecycle event %v (saw %v)", k, kinds)
		}
	}
}

// TestUnavailableErrorCarriesFixes is the DX promise: never a one-line failure,
// always tried + fixes (§5.3).
func TestUnavailableErrorCarriesFixes(t *testing.T) {
	if err := sandboxtest.Register(); err != nil {
		t.Fatalf("register: %v", err)
	}
	_, err := sandbox.Open(context.Background(), sandbox.WithProvider("does-not-exist"))
	if err == nil {
		t.Fatal("expected unavailable error")
	}
	var un *sandbox.UnavailableError
	if !errors.As(err, &un) {
		t.Fatalf("err = %T %v, want *sandbox.UnavailableError", err, err)
	}
	if len(un.Tried) == 0 {
		t.Fatal("UnavailableError must record what was tried")
	}
}

// TestDiagnoseReportsActionableFindings covers the "why can't this run here?"
// path with a provider that reports nothing usable.
func TestDiagnoseReportsActionableFindings(t *testing.T) {
	ctx := context.Background()
	sb := openHandle(t)
	diag, err := sb.Diagnose(ctx)
	if err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	if len(diag.Checks) == 0 {
		t.Fatal("Diagnose returned no checks")
	}
}

// TestLockdownEnforceRejectsUndeclaredInstall proves D-15/D-16: in production
// the declared spec is the only way to change the environment, and the refusal
// tells the model what to do instead (§17.3).
func TestLockdownEnforceRejectsUndeclaredInstall(t *testing.T) {
	ctx := context.Background()
	sb := openHandle(t,
		sandbox.WithProfile(sandbox.ProfileProduction),
		sandbox.WithMinimumIsolation(sandbox.IsolationContainer),
		sandbox.WithEgress(sandbox.Egress{Mode: sandbox.EgressNone}),
		sandbox.WithLockdown(sandbox.LockdownEnforce),
	)
	sess, err := sb.Session(ctx, "lock", sandbox.WithCreateIfMissing(true))
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	_, err = sess.Exec(ctx, sandbox.Request{Runtime: "sh", Code: "pip install cowsay"})
	if err == nil {
		t.Fatal("expected lockdown to refuse an undeclared install")
	}
	f := sandbox.AsFailure(err)
	if f.NextAction != sandbox.ActionUseInstallAPI {
		t.Fatalf("NextAction = %v, want use-install-api", f.NextAction)
	}
	if !strings.Contains(strings.ToLower(f.Hint), "install") {
		t.Fatalf("Hint = %q, want it to point at the Install API", f.Hint)
	}
	dr, err := sess.Drift(ctx)
	if err != nil {
		t.Fatalf("Drift: %v", err)
	}
	if dr.Empty() {
		t.Fatal("a refused undeclared mutation must still be recorded in the drift report")
	}
}

// TestLockdownDetectDoesNotBreakDevWorkflow checks the dev default: drift is
// reported, execution continues (§17.3).
func TestLockdownDetectDoesNotBreakDevWorkflow(t *testing.T) {
	ctx := context.Background()
	sb := openHandle(t,
		sandbox.WithProfile(sandbox.ProfileDev),
		sandbox.WithMinimumIsolation(sandbox.IsolationContainer),
		sandbox.WithEgress(sandbox.Egress{Mode: sandbox.EgressNone}),
		sandbox.WithLockdown(sandbox.LockdownDetect),
	)
	sess, err := sb.Session(ctx, "dev-lock", sandbox.WithCreateIfMissing(true))
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if _, err := sess.Exec(ctx, sandbox.Request{Runtime: "sh", Code: "pip install cowsay"}); err != nil {
		t.Fatalf("detect mode must not block a dev run: %v", err)
	}
	dr, err := sess.Drift(ctx)
	if err != nil {
		t.Fatalf("Drift: %v", err)
	}
	if dr.Empty() {
		t.Fatal("detect mode must still report the drift")
	}
}

// TestProjectPhasesAndResume exercises the project shape: workspace once,
// phases in order, required-phase failure stops the run (§13.2).
func TestProjectPhasesAndResume(t *testing.T) {
	ctx := context.Background()
	sb := openHandle(t)
	sess, err := sb.Session(ctx, "proj", sandbox.WithCreateIfMissing(true))
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	proj := sandbox.Project{
		From: sandbox.FromArtifacts(sandbox.ArtifactSet{{Name: "src/main.go", Path: "src/main.go"}}, "src"),
		Mode: sandbox.CopySnapshot,
		Phases: []sandbox.Phase{
			{Name: "build", Run: sandbox.Request{Runtime: "sh", Code: "#!echo built"}},
			{Name: "test", Run: sandbox.Request{Runtime: "sh", Code: "#!exit 1"}, Required: true},
			{Name: "package", Run: sandbox.Request{Runtime: "sh", Code: "#!echo packaged"}},
		},
		Artifacts: []sandbox.ArtifactSpec{{Path: "src/main.go", Kind: sandbox.KindFile}},
	}
	res, err := sess.RunProject(ctx, proj)
	if err != nil {
		t.Fatalf("RunProject returned error: %v", err)
	}
	if res.Status != sandbox.ProjectFailed {
		t.Fatalf("Status = %v, want failed", res.Status)
	}
	if res.FailedPhase != "test" {
		t.Fatalf("FailedPhase = %q, want test", res.FailedPhase)
	}
	if len(res.Phases) != 2 {
		t.Fatalf("phases = %d, want build+test (package skipped after required failure)", len(res.Phases))
	}
	if res.NextAction != sandbox.ActionModelFix {
		t.Fatalf("NextAction = %v, want model-fix", res.NextAction)
	}

	// Resume from the failing phase: earlier phases are not repeated.
	resume := proj
	resume.ResumeFrom = "test"
	resume.Phases[1].Run.Code = "#!echo tests-pass"
	again, err := sess.RunProject(ctx, resume)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if again.Status != sandbox.ProjectSucceeded && again.Status != sandbox.ProjectPartial {
		t.Fatalf("resume status = %v", again.Status)
	}
}

// TestRebuildReplaysSpec proves the default retry mode: after the environment
// is lost, the session comes back by re-rendering the declared spec (§17.2).
func TestRebuildReplaysSpec(t *testing.T) {
	ctx := context.Background()
	sb := openHandle(t)
	sess, err := sb.Session(ctx, "rebuild", sandbox.WithCreateIfMissing(true))
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if err := sess.Install(ctx, sandbox.Pip("requests==2.31.0")); err != nil {
		t.Fatalf("Install: %v", err)
	}
	// Simulate engine loss.
	sess.MarkLost(errors.New("simulated engine loss"))
	if sess.Status().State != sandbox.StateLost {
		t.Fatalf("state = %v, want lost", sess.Status().State)
	}
	rep, err := sess.Rebuild(ctx)
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if len(rep.Replayed) == 0 && rep.Cached == 0 {
		t.Fatalf("rebuild replayed nothing: %+v", rep)
	}
	// The session is usable again and keeps the recorded environment.
	if _, err := sess.Exec(ctx, sandbox.Request{Runtime: "sh", Code: "#!echo post-rebuild"}); err != nil {
		t.Fatalf("exec after rebuild: %v", err)
	}
}

// TestAuditRecordWrittenOnEveryPath checks the missing piece of today's
// implementation: execution audit, including refusals (§5.3).
func TestAuditRecordWrittenOnEveryPath(t *testing.T) {
	ctx := context.Background()
	var recs []sandbox.AuditRecord
	sb := openHandle(t,
		sandbox.WithAudit(sandbox.AuditSinkFunc(func(_ context.Context, rec sandbox.AuditRecord) error {
			recs = append(recs, rec)
			return nil
		}), sandbox.WithAuditOptions{IncludeArgv: true}),
		sandbox.WithPolicy(sandbox.PolicyFunc(func(_ context.Context, rc *sandbox.RunContext) (sandbox.Decision, error) {
			if strings.Contains(rc.Request.Code, "forbidden") {
				return sandbox.Deny("not allowed"), nil
			}
			return sandbox.Allow(), nil
		})),
	)
	if _, err := sb.Run(ctx, sandbox.Request{Runtime: "sh", Code: "#!echo ok"}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if _, err := sb.Run(ctx, sandbox.Request{Runtime: "sh", Code: "#!echo forbidden"}); err == nil {
		t.Fatal("expected denial")
	}
	if len(recs) != 2 {
		t.Fatalf("audit records = %d, want 2 (one success, one denial)", len(recs))
	}
	var denial *sandbox.AuditRecord
	for i := range recs {
		if recs[i].Failure == sandbox.ClassDenied.String() {
			denial = &recs[i]
		}
	}
	if denial == nil {
		t.Fatalf("denial was not audited: %+v", recs)
	}
	if denial.Provider == "" || denial.RunID == "" {
		t.Fatalf("audit record missing identity: %+v", denial)
	}
}

// TestEgressAllowlistProducesBlockedList checks that a refused host reads as a
// refusal with the list attached, not as a network error (§16.6).
func TestEgressAllowlistProducesBlockedList(t *testing.T) {
	ctx := context.Background()
	sb := openHandle(t)
	rep, err := sb.Run(ctx, sandbox.Request{
		Runtime: "sh",
		Code:    "#!echo fetch",
		Egress:  sandbox.Egress{Mode: sandbox.EgressAllowlist, Allow: []string{"pypi.org"}, Requested: []string{"evil.example"}},
	})
	if err == nil {
		t.Fatal("expected egress denial")
	}
	if len(rep.BlockedHosts) == 0 || rep.BlockedHosts[0] != "evil.example" {
		t.Fatalf("BlockedHosts = %v, want evil.example", rep.BlockedHosts)
	}
}

// TestCloseIsIdempotentAndCleansUp covers the cleanup contract (§5.3, §15).
func TestCloseIsIdempotentAndCleansUp(t *testing.T) {
	ctx := context.Background()
	sb := openHandle(t)
	if _, err := sb.Session(ctx, "cleanup", sandbox.WithCreateIfMissing(true)); err != nil {
		t.Fatalf("Session: %v", err)
	}
	if err := sb.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}
	if err := sb.Close(); err != nil {
		t.Fatalf("second close must be a no-op: %v", err)
	}
	if _, err := sb.Run(ctx, sandbox.Request{Runtime: "sh", Code: "#!echo x"}); err == nil {
		t.Fatal("run after close must fail")
	}
}

// TestProfileDefaults checks the D-15 posture: dev detects, production enforces.
func TestProfileDefaults(t *testing.T) {
	cases := []struct {
		profile sandbox.Profile
		want    sandbox.Lockdown
	}{
		{sandbox.ProfileDev, sandbox.LockdownDetect},
		{sandbox.ProfileCI, sandbox.LockdownEnforce},
		{sandbox.ProfileProduction, sandbox.LockdownEnforce},
	}
	for _, tc := range cases {
		// The floor is lowered to what the reference provider can honestly
		// provide; lockdown posture is orthogonal to isolation level.
		sb := openHandle(t, sandbox.WithProfile(tc.profile), sandbox.WithMinimumIsolation(sandbox.IsolationContainer), sandbox.WithEgress(sandbox.Egress{Mode: sandbox.EgressNone}))
		if got := sb.LockdownMode(); got != tc.want {
			t.Errorf("profile %s lockdown = %v, want %v", tc.profile, got, tc.want)
		}
	}
}

// TestMinIsolationRefusesWeakerProvider is the D-02 rule: the handle never
// silently lands on a weaker engine than it asked for.
func TestMinIsolationRefusesWeakerProvider(t *testing.T) {
	if err := sandboxtest.Register(); err != nil {
		t.Fatalf("register: %v", err)
	}
	_, err := sandbox.Open(context.Background(),
		sandbox.WithProvider("memory"),
		sandbox.WithAllowedProviders("memory"),
		sandbox.WithMinimumIsolation(sandbox.IsolationMicroVM),
	)
	if err == nil {
		t.Fatal("expected refusal: memory provider cannot reach micro-VM isolation")
	}
	var un *sandbox.UnavailableError
	if !errors.As(err, &un) {
		t.Fatalf("err = %T %v, want *sandbox.UnavailableError", err, err)
	}
	if len(un.Tried) == 0 {
		t.Fatal("UnavailableError must record what was tried")
	}
	if un.Want != sandbox.IsolationMicroVM {
		t.Fatalf("Want = %v, want micro-VM", un.Want)
	}
}
