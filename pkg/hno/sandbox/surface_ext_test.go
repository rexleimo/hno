package sandbox_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rexleimo/agno-go/pkg/hno/sandbox"
	"github.com/rexleimo/agno-go/pkg/hno/sandbox/sandboxtest"
)

// --- approval + tighten flows -------------------------------------------

func TestApprovalFlowSuspendsAndNamesTheRequester(t *testing.T) {
	ctx := context.Background()
	sb := openHandle(t, sandbox.WithPolicy(sandbox.PolicyFunc(
		func(_ context.Context, rc *sandbox.RunContext) (sandbox.Decision, error) {
			if rc.Request.Effect == sandbox.EffectExternal {
				return sandbox.ApproveBy("external write", "release@team"), nil
			}
			return sandbox.Allow(), nil
		})))

	rep, err := sb.Run(ctx, sandbox.Request{Runtime: "sh", Code: "#!echo x", Effect: sandbox.EffectExternal})
	if err == nil {
		t.Fatal("expected an approval interruption")
	}
	var pending *sandbox.ApprovalPendingError
	if !errors.As(err, &pending) {
		t.Fatalf("err = %T %v, want *ApprovalPendingError", err, err)
	}
	if pending.ApprovalID == "" {
		t.Fatal("an interruption must carry an id a human can act on")
	}
	if rep == nil || rep.Failure == nil || rep.Failure.NextAction != sandbox.ActionRequestApproval {
		t.Fatalf("report should say what to do next: %+v", rep)
	}
}

func TestTightenAppliedByPolicyReachesTheProvider(t *testing.T) {
	ctx := context.Background()
	sb := openHandle(t, sandbox.WithPolicy(sandbox.PolicyFunc(
		func(_ context.Context, _ *sandbox.RunContext) (sandbox.Decision, error) {
			return sandbox.Tighten(func(r *sandbox.Request) {
				r.Egress = sandbox.Egress{Mode: sandbox.EgressAllowlist, Allow: []string{"pypi.org"}}
			}), nil
		})))
	// The request asks for nothing; the policy narrows egress, which is allowed.
	// A refusal here would mean tighten-only is enforced against its own purpose.
	if _, err := sb.Run(ctx, sandbox.Request{Runtime: "sh", Code: "#!echo x"}); err != nil {
		t.Fatalf("a tightening policy broke the run: %v", err)
	}
}

// --- artefact collection shapes -----------------------------------------

func TestDirAndGlobArtifactCollection(t *testing.T) {
	ctx := context.Background()
	sb := openHandle(t)
	sess, err := sb.Session(ctx, "tree", sandbox.WithCreateIfMissing(true))
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	writes := []string{
		"#!write reports/a.xml one",
		"#!write reports/b.xml two",
		"#!write reports/notes.txt skip",
	}
	for _, w := range writes {
		if _, err := sess.Exec(ctx, sandbox.Request{Runtime: "sh", Code: w}); err != nil {
			t.Fatalf("write %q: %v", w, err)
		}
	}
	rep, err := sess.Exec(ctx, sandbox.Request{
		Runtime: "sh",
		Code:    "#!echo done",
		Outputs: []sandbox.ArtifactSpec{
			{Path: "reports", Kind: sandbox.KindDir},
			{Path: "reports/*.xml", Kind: sandbox.KindGlob},
		},
	})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(rep.Artifacts) == 0 {
		t.Fatal("dir and glob collection produced nothing")
	}
	// The dir spec owns all three files; the glob overlaps it and must not
	// duplicate or smuggle anything extra.
	if len(rep.Artifacts) != 3 {
		t.Fatalf("dir reports/ holds three files, got %v", rep.Artifacts.Names())
	}
	count := map[string]int{}
	for _, n := range rep.Artifacts.Names() {
		count[n]++
	}
	for name, n := range count {
		if n > 1 {
			t.Fatalf("%q collected %d times; overlapping specs must dedupe", name, n)
		}
	}
	if _, ok := rep.Artifacts.Get("reports/a.xml"); !ok {
		t.Fatalf("a declared report is missing: %v", rep.Artifacts.Names())
	}
}

func TestArtifactSetHelpers(t *testing.T) {
	set := sandbox.ArtifactSet{
		{Name: "a.json", Path: "a.json", Kind: sandbox.KindFile},
		{Name: "b.txt", Path: "b.txt", Kind: sandbox.KindFile},
	}
	if _, ok := set.Get("a.json"); !ok {
		t.Fatal("Get by name should find a declared artefact")
	}
	if got := set.Names(); len(got) != 2 {
		t.Fatalf("Names = %v", got)
	}
	if missing, ok := set.Get("nope"); ok || missing.Name != "" {
		t.Fatal("Get must report absence honestly")
	}
}

// --- effect gate ---------------------------------------------------------

func TestEffectGateBlocksExternalRetry(t *testing.T) {
	external := sandbox.Request{Effect: sandbox.EffectExternal}
	if external.RetryAllowed(sandbox.ClassTransient) {
		t.Fatal("an external effect must never auto-retry without a delivery promise")
	}
	promised := sandbox.Request{Effect: sandbox.EffectExternal, Delivery: sandbox.DeliveryAtMostOnce, IdempotencyKey: "k"}
	if !promised.RetryAllowed(sandbox.ClassTransient) {
		t.Fatal("with at-most-once plus a key, a retry is safe")
	}
	workspace := sandbox.Request{Effect: sandbox.EffectWorkspace}
	if !workspace.RetryAllowed(sandbox.ClassLost) {
		t.Fatal("a lost workspace is exactly what rebuild answers")
	}
	if workspace.RetryAllowed(sandbox.ClassPermanent) {
		t.Fatal("a permanent error is not retryable on a workspace effect")
	}
	pure := sandbox.Request{Effect: sandbox.EffectPure}
	if !pure.RetryAllowed(sandbox.ClassTransient) || pure.RetryAllowed(sandbox.ClassUserCode) {
		t.Fatal("pure effects follow the class alone")
	}
}

// --- unavailable diagnostics --------------------------------------------

func TestUnavailablePathCollectsAttemptsAndFixes(t *testing.T) {
	ctx := context.Background()
	_, err := sandbox.Open(ctx,
		sandbox.WithProvider("memory"),
		sandbox.WithAllowedProviders("memory"),
		sandbox.WithMinimumIsolation(sandbox.IsolationMicroVMHosted),
		sandbox.WithFallbackTier(),
		// Fallback is only legal with an observer attached (D-13): a downgrade
		// nobody can see is a downgrade that will be blamed on the sandbox later.
		sandbox.WithObserver(sandbox.ObserverFunc(func(_ context.Context, _ sandbox.Event) error { return nil })))
	var un *sandbox.UnavailableError
	if !errors.As(err, &un) {
		t.Fatalf("err = %T %v, want *UnavailableError", err, err)
	}
	if len(un.Tried) == 0 {
		t.Fatal("no attempts recorded")
	}
	if len(un.Remediation()) == 0 {
		t.Fatalf("an UnavailableError without a fix is the failure mode this type exists to remove: %+v", un)
	}
	if !strings.Contains(un.Error(), "memory") {
		t.Fatalf("the message should name what was tried: %q", un.Error())
	}
}

// --- sandbox accessors ---------------------------------------------------

func TestSandboxAccessorSurface(t *testing.T) {
	ctx := context.Background()
	sb := openHandle(t)
	if sb.Limits().OutputMaxBytes <= 0 {
		t.Fatal("Limits should report resolved ceilings")
	}
	switch sb.LockdownMode() {
	case sandbox.LockdownDetect, sandbox.LockdownEnforce:
	default:
		t.Fatalf("unexpected LockdownMode %v", sb.LockdownMode())
	}
	if err := sb.Probe(ctx); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	st := sb.Status()
	if st.Provider == "" || st.Isolation == sandbox.IsolationUnknown {
		t.Fatalf("Status incomplete: %+v", st)
	}
	if err := st.Meets(sandbox.CapSession, sandbox.IsolationContainer); err != nil {
		t.Fatalf("Meets should pass for what the provider declares: %v", err)
	}
	if err := st.Meets(sandbox.CapPortForward, sandbox.IsolationMicroVM); err == nil {
		t.Fatal("Meets must fail for what it does not have")
	}
}

// --- caller attribution --------------------------------------------------

func TestCallerPlumbingReachesPolicy(t *testing.T) {
	ctx := context.Background()
	var seen sandbox.CallerInfo
	sb := openHandle(t, sandbox.WithPolicy(sandbox.PolicyFunc(
		func(_ context.Context, rc *sandbox.RunContext) (sandbox.Decision, error) {
			seen = rc.Caller
			return sandbox.Allow(), nil
		})))
	if _, err := sb.Run(ctx, sandbox.Request{Runtime: "sh", Code: "#!echo x"},
		sandbox.WithCaller(sandbox.CallerInfo{Agent: "coder", User: "rex", TraceID: "t-1"})); err != nil {
		t.Fatalf("run: %v", err)
	}
	if seen.Agent != "coder" || seen.TraceID != "t-1" {
		t.Fatalf("caller info lost: %+v", seen)
	}
}

// --- budget names --------------------------------------------------------

func TestBudgetAndDeliveryStrings(t *testing.T) {
	if sandbox.DeliveryUnset.String() != "" || sandbox.DeliveryAtMostOnce.String() != "at-most-once" {
		t.Fatalf("delivery strings: %q %q", sandbox.DeliveryUnset.String(), sandbox.DeliveryAtMostOnce.String())
	}
	for _, e := range []sandbox.EffectClass{sandbox.EffectPure, sandbox.EffectWorkspace, sandbox.EffectExternal} {
		if e.String() == "" {
			t.Fatalf("effect %d has no name", e)
		}
	}
	for _, m := range []sandbox.MountMode{sandbox.CopySnapshot, sandbox.MountShare, sandbox.MountHybrid} {
		if m.String() == "" {
			t.Fatalf("mount mode %d has no name", m)
		}
	}
	for _, r := range []sandbox.RetryMode{sandbox.RetryBlind, sandbox.RebuildFromSpec, sandbox.RollbackToPhase, sandbox.ForwardsOnly} {
		if r.String() == "" {
			t.Fatalf("retry mode %d has no name", r)
		}
	}
	for _, e := range []sandbox.EgressMode{sandbox.EgressUnset, sandbox.EgressNone, sandbox.EgressAllowlist, sandbox.EgressOpen} {
		if e.String() == "" {
			t.Fatalf("egress mode %d has no name", e)
		}
	}
}

// --- session state predicates (public surface) ---------------------------

func TestSessionStatePredicates(t *testing.T) {
	for _, s := range []sandbox.SessionState{sandbox.StateReady, sandbox.StateBusy} {
		if !s.AcceptsExec() {
			t.Errorf("%v should accept exec", s)
		}
		if s.Terminal() {
			t.Errorf("%v must not be terminal", s)
		}
	}
	for _, s := range []sandbox.SessionState{sandbox.StateSuspended, sandbox.StateLost} {
		if !s.Resumable() {
			t.Errorf("%v should be resumable", s)
		}
	}
	for _, s := range []sandbox.SessionState{sandbox.StateReleased, sandbox.StateCorrupted} {
		if !s.Terminal() {
			t.Errorf("%v should be terminal", s)
		}
	}
	if sandbox.StateNew.String() == "" {
		t.Fatal("states need names for StateError messages")
	}
}

// --- timeout is bounded per attempt -------------------------------------

// --- pre-flight diagnosis ------------------------------------------------

func TestPreflightReportsCandidatesAsData(t *testing.T) {
	ctx := context.Background()
	if err := sandboxtest.Register(); err != nil {
		t.Fatalf("register: %v", err)
	}
	rep, err := sandbox.Preflight(ctx,
		sandbox.WithProvider("memory"),
		sandbox.WithAllowedProviders("memory"))
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if len(rep.Checks) == 0 {
		t.Fatal("a pre-flight with no checks is not a diagnosis")
	}
	var sawProvider, sawKVM bool
	for _, c := range rep.Checks {
		if strings.HasPrefix(c.Env, "provider:") {
			sawProvider = true
		}
		if c.Env == "kvm" {
			sawKVM = true
		}
	}
	if !sawProvider {
		t.Fatalf("no provider verdict in %+v", rep.Checks)
	}
	if !sawKVM {
		t.Fatal("host virtualisation must be part of the answer")
	}
	if rep.Summary == "" {
		t.Fatal("a report nobody can read in one line is not a diagnosis")
	}
}

func TestPreflightWithoutProvidersIsActionable(t *testing.T) {
	ctx := context.Background()
	// An allow-list that matches nothing must say so, rather than reporting an
	// empty candidate set as success.
	rep, err := sandbox.Preflight(ctx, sandbox.WithAllowedProviders("does-not-exist"))
	if err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	for _, c := range rep.Checks {
		if c.Env == "allowed-providers" && c.OK {
			t.Fatal("an allow-list excluding everything must not pass")
		}
	}
	if len(rep.Fixes) == 0 {
		t.Fatalf("expected a fix, got %+v", rep)
	}
}
