package sandbox

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestDecisionHelpersCarryTheirPayload(t *testing.T) {
	if Allow().Action != ActAllow {
		t.Fatal("Allow")
	}
	d := Deny("no")
	if d.Action != ActDeny || d.Reason != "no" {
		t.Fatalf("Deny = %+v", d)
	}
	dr := DenyRule("blocked host", "egress.evil")
	if dr.Rule != "egress.evil" {
		t.Fatalf("DenyRule = %+v", dr)
	}
	ap := Approve("needs a human")
	if ap.Action != ActRequireApproval || ap.Reason != "needs a human" {
		t.Fatalf("Approve = %+v", ap)
	}
	aby := ApproveBy("prod write", "ops@team")
	if aby.Approval == nil || aby.Approval.Approver != "ops@team" {
		t.Fatalf("ApproveBy = %+v", aby)
	}
	tg := Tighten(func(r *Request) { r.Egress.Mode = EgressNone })
	if tg.Tighten == nil {
		t.Fatal("Tighten must carry its mutation")
	}
	rc := &RunContext{Request: &Request{Egress: Egress{Mode: EgressOpen}}}
	tg.Tighten(rc.Request)
	if rc.Request.Egress.Mode != EgressNone {
		t.Fatal("tighten closure did not apply")
	}
}
func TestInstallHelpersAndDisplay(t *testing.T) {
	p := Pip("x").WithFlags("--quiet")
	if !strings.Contains(strings.Join(p.installCommand(), " "), "--quiet") {
		t.Fatalf("WithFlags did not reach argv: %v", p.installCommand())
	}
	withExtra := Pip("y").With("index-url", "https://pypi.example")
	if !strings.Contains(strings.Join(withExtra.installCommand(), " "), "--index-url=https://pypi.example") {
		t.Fatalf("extras missing: %v", withExtra.installCommand())
	}
	if Pip("z").String() == "" || !strings.Contains(strings.ToLower(Pip("z").String()), "pip") {
		t.Fatalf("PackageSpec.String = %q", Pip("z").String())
	}
	if len(shellQuoteAll(nil)) != 0 {
		t.Fatal("shellQuoteAll(nil) should be empty")
	}
	if !strings.Contains(shellQuoteWord("a b"), "'") {
		t.Fatal("words with spaces must be quoted")
	}
	if hashString("") == "" || hashString("a") == hashString("b") {
		t.Fatal("hashString must be stable and discriminating")
	}
	if inputDigest(Input{Path: "a", Bytes: []byte("x")}) == "" {
		t.Fatal("inputDigest")
	}
	if Apt("curl").Fingerprint() == "" {
		t.Fatal("a package spec needs a fingerprint for step caching")
	}
	if len(Npm("left-pad").requirementArgs()) != 1 {
		t.Fatal("requirementArgs")
	}
}
func TestErrorTypesRenderAndUnwrap(t *testing.T) {
	inner := errors.New("inner")
	cases := []error{
		&ProvisionError{Provider: "p", Stage: "boot", Cause: inner},
		&DeniedError{Policy: "p", Reason: "r"},
		&PathError{Path: "p", Reason: "r"},
		&StaleSessionError{SessionID: "s", Cause: inner},
		&LostError{RunID: "r", SessionID: "s", Provider: "p"},
		&UncertainOutcomeError{RunID: "r", Effect: EffectExternal},
		&IdempotencyConflictError{Key: "k", OriginalRun: "r"},
		&ArtifactError{Spec: ArtifactSpec{Path: "a"}, Reason: "missing"},
		&CleanupError{Provider: "p", Residuals: []string{"x"}, SweeperID: "s"},
		&StateError{SessionID: "s", State: StateReleased, Action: "exec", Allowed: []SessionState{StateReady}},
		&DriftError{SessionID: "s", Items: []string{"i"}, Strict: true},
		&StepReplayError{Step: Step{Kind: StepInstall, Tool: "pip"}, Cause: inner},
		&CapabilityError{Provider: "p", Missing: CapClone, Wanted: Capabilities(CapSession)},
		&IsolationError{Provider: "p", Got: IsolationContainer, Want: IsolationMicroVM},
		&QuotaError{Scope: "host", Limit: 1, Active: 1},
		&HookFaultError{Kind: "policy", Name: "guard", Detail: "panic"},
		&ApprovalPendingError{ApprovalID: "a", Requested: "run"},
		&HostWatermarkError{Resource: "disk", Free: 0, Want: 1},
		&ReadinessError{RunID: "r"},
		&VersionSkewError{Provider: "p"},
		&RetryBudgetError{Scope: "run", Attempts: 2, Window: time.Second},
	}
	for _, err := range cases {
		if err.Error() == "" {
			t.Errorf("%T renders an empty message", err)
		}
		f := AsFailure(err)
		if f == nil || f.Class == ClassUnknown {
			t.Errorf("%T should classify, got %+v", err, f)
		}
		if errors.Is(err, inner) && !strings.Contains(err.Error(), "inner") {
			t.Errorf("%T wraps but hides the cause", err)
		}
	}
	// StateError must enumerate what is legal, which is the difference between
	// a dead end and a next step (§6.2).
	se := &StateError{SessionID: "s", State: StateReleased, Action: "exec", Allowed: []SessionState{StateReady, StateBusy}}
	if !strings.Contains(se.Error(), "ready") || !strings.Contains(se.Error(), "busy") {
		t.Fatalf("StateError should list allowed states: %q", se.Error())
	}
	if joinStates(nil) == "" {
		t.Fatal("joinStates should say none rather than nothing")
	}
}
func TestClassifyReportCoversSilentFailure(t *testing.T) {
	sb := &Sandbox{cfg: Config{}}

	lost := &Report{RunID: "r1", ExitCode: -1, Lost: true}
	f := classifyReport(sb, lost, nil)
	if f == nil || f.Class != ClassLost {
		t.Fatalf("lost report classified as %+v", f)
	}
	timed := &Report{RunID: "r1", TimedOut: true, ExitCode: -1}
	if f := classifyReport(sb, timed, nil); f.Class != ClassTimeout {
		t.Fatalf("timeout report classified as %+v", f)
	}
	code := &Report{RunID: "r1", ExitCode: 2, Stderr: "Traceback"}
	f = classifyReport(sb, code, nil)
	if f.Class != ClassUserCode || f.NextAction != ActionModelFix {
		t.Fatalf("user code report classified as %+v", f)
	}
	if !strings.Contains(f.Cause.Error(), "2") {
		t.Fatalf("cause should explain the exit status, got %q", f.Cause)
	}
}
func TestPolicyLabelPrefersExplicitName(t *testing.T) {
	if got := policyLabel("generated", "explicit"); got != "explicit" {
		t.Fatalf("policyLabel = %q", got)
	}
	if got := policyLabel("generated", ""); got != "generated" {
		t.Fatalf("policyLabel fallback = %q", got)
	}
}
func TestGuardedPolicyRecoversPanicAndRejectsNil(t *testing.T) {
	_, err := guardedPolicy(context.Background(), PolicyFunc(func(context.Context, *RunContext) (Decision, error) {
		panic("boom")
	}), &RunContext{})
	if err == nil {
		t.Fatal("a panicking policy must surface as a fault")
	}
	if _, err := guardedPolicy(context.Background(), nil, &RunContext{}); err == nil {
		t.Fatal("a nil policy is a configuration error")
	}
}
func TestEgressTighteningIntersectsAllowlists(t *testing.T) {
	profile := Egress{Mode: EgressAllowlist, Allow: []string{"pypi.org", "files.pythonhosted.org"}}
	run := Egress{Mode: EgressAllowlist, Allow: []string{"pypi.org", "evil.example"}}
	got := tightenEgress(profile, run)
	if got.Mode != EgressAllowlist {
		t.Fatalf("mode = %v", got.Mode)
	}
	for _, h := range got.Allow {
		if h == "evil.example" {
			t.Fatal("per-run egress widened past the profile allowlist")
		}
	}
	// A run asking for open egress under an allowlisted profile stays locked.
	open := tightenEgress(profile, Egress{Mode: EgressOpen})
	if open.Mode == EgressOpen {
		t.Fatal("a per-run option must never open egress the profile closed")
	}
	// Profile-level open with a narrower request takes the narrower one.
	wide := tightenEgress(Egress{Mode: EgressOpen}, Egress{Mode: EgressNone})
	if wide.Mode != EgressNone {
		t.Fatalf("expected the stricter posture, got %v", wide.Mode)
	}
}
func TestRunOptionsOnlyTightenProfile(t *testing.T) {
	cfg := &Config{}
	cfg.ProfileName = ProfileCI
	if err := cfg.resolve(); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	ro := resolveRunOptions([]RunOption{WithTimeout(time.Hour), WithOutputLimit(1 << 30)}, cfg)
	if ro.Budget.ExecTimeout > cfg.Timeouts.Exec && cfg.Timeouts.Exec > 0 {
		t.Fatalf("run option widened the profile exec budget: %v > %v", ro.Budget.ExecTimeout, cfg.Timeouts.Exec)
	}
	if ro.Limits.OutputMaxBytes > cfg.Limits.OutputMaxBytes {
		t.Fatalf("run option widened the output ceiling: %d > %d", ro.Limits.OutputMaxBytes, cfg.Limits.OutputMaxBytes)
	}
}
