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

// TestNestedUnavailableDoesNotRepeatItself guards the diagnosis quality found
// while running against a real engine: a provider-level UnavailableError that was
// re-serialised into the core's attempt string made the same daemon sentence
// appear three times and the same fix twice in one message. A reader has to parse
// noise before reaching the one fact that matters, so the splicing is a testable
// property, not a style preference.
func TestNestedUnavailableDoesNotRepeatItself(t *testing.T) {
	inner := &UnavailableError{
		Reason: "provider answers version but refuses container create: not supported here",
		Tried: []Attempt{{
			Provider:  "docker",
			Isolation: IsolationContainer,
			Reason:    "provider answers version but refuses container create: not supported here",
			Fixes:     []string{"register the provider against an engine that creates containers"},
		}},
		Want: IsolationContainer,
	}
	tried := appendAttempt(nil, "docker", inner)
	if len(tried) != 1 {
		t.Fatalf("expected the inner attempt spliced through, got %d", len(tried))
	}
	if tried[0].Provider != "docker" {
		t.Fatalf("attempt should keep the provider identity, got %q", tried[0].Provider)
	}
	if tried[0].Fix == "" {
		t.Fatal("the fix must survive the splice; dropping it restores the old DX failure")
	}

	outer := &UnavailableError{Tried: tried, Want: IsolationContainer}
	msg := outer.Error()
	if n := strings.Count(msg, "refuses container create"); n > 1 {
		t.Fatalf("finding repeated %d times in one message:\n%s", n, msg)
	}
	if n := strings.Count(msg, "register the provider against"); n > 1 {
		t.Fatalf("fix repeated %d times:\n%s", n, msg)
	}
}

func TestAppendAttemptFallsBackToPlainError(t *testing.T) {
	tried := appendAttempt(nil, "p", errors.New("boom"))
	if len(tried) != 1 || tried[0].Reason != "boom" {
		t.Fatalf("a plain error should still be recorded verbatim: %+v", tried)
	}
	if tried[0].Provider != "p" {
		t.Fatalf("provider name missing: %+v", tried[0])
	}
}

// TestMergeLimitsCoversEveryField guards the class of bug where a newly added
// limit is declared on Limits but silently dropped on the per-run merge path, so
// a caller tightens CPU and the engine never hears about it. Any field added to
// Limits must participate in mergeLimits, and only ever in the tightening
// direction.
func TestMergeLimitsCoversEveryField(t *testing.T) {
	base := Limits{
		InlineFileMaxBytes: 100, OutputMaxBytes: 200, WorkspaceMaxBytes: 300,
		ArtifactMaxBytes: 400, ArtifactTotalMaxBytes: 500, EnvMaxEntries: 60,
		SessionMaxConcurrentRuns: 70, SnapshotKeep: 80,
		CPUCount: 4, MemoryMB: 2048, PidsLimit: 512, DiskMB: 1024,
	}
	over := Limits{
		InlineFileMaxBytes: 50, OutputMaxBytes: 0, WorkspaceMaxBytes: 0,
		ArtifactMaxBytes: 0, ArtifactTotalMaxBytes: 0, EnvMaxEntries: 0,
		SessionMaxConcurrentRuns: 0, SnapshotKeep: 0,
		CPUCount: 2, MemoryMB: 512, PidsLimit: 100, DiskMB: 256,
	}
	got := mergeLimits(base, over)
	if got.InlineFileMaxBytes != 50 {
		t.Fatalf("byte tightening lost: %d", got.InlineFileMaxBytes)
	}
	if got.CPUCount != 2 || got.MemoryMB != 512 || got.PidsLimit != 100 || got.DiskMB != 256 {
		t.Fatalf("compute limits dropped: %+v", got)
	}
	// Unstated on the override side must keep the profile value, never zero it.
	if g := mergeLimits(base, Limits{}); g.CPUCount != 4 || g.MemoryMB != 2048 || g.PidsLimit != 512 {
		t.Fatalf("an unstated per-run limit wiped the profile: %+v", g)
	}
	// A per-run widening must be refused, the same rule that protects the byte
	// ceilings: one caller must not undo the profile's decision.
	if g := mergeLimits(base, Limits{CPUCount: 64, MemoryMB: 1 << 30}); g.CPUCount != 4 || g.MemoryMB != 2048 {
		t.Fatalf("per-run options widened a production ceiling: %+v", g)
	}
}

func TestTailExcerptKeepsTheReason(t *testing.T) {
	long := strings.Repeat("noise line\n", 500) + "the real error: cannot find module"
	got := tailExcerpt(long, 200)
	if !strings.Contains(got, "cannot find module") {
		t.Fatalf("tail excerpt lost the last line: %q", got)
	}
	if !strings.HasPrefix(got, "[…") {
		t.Fatalf("omission not marked, so a reader cannot tell it is a fragment: %q", got)
	}
	if len(got) > 400 {
		t.Fatalf("excerpt far exceeded the cap: %d bytes", len(got))
	}
	if tailExcerpt("short", 100) != "short" {
		t.Fatal("a short string must pass through unchanged")
	}
	if tailExcerpt("", 100) != "" {
		t.Fatal("empty in, empty out")
	}
}

// --- capability semantics registry ---------------------------------------

// TestEveryCapabilityHasDeclaredSemantics is the anti-drift guard. A capability bit
// is a promise in the public vocabulary; the enum growing past the table is how
// CapAudit and CapIdleSuspend came to be declared, advertised, and consulted by
// nothing. Adding a bit without recording what the core does with it fails here.
func TestEveryCapabilityHasDeclaredSemantics(t *testing.T) {
	seen := map[Capability]bool{}
	for _, c := range allCapabilities {
		if seen[c] {
			t.Fatalf("capability %v listed twice in allCapabilities", c)
		}
		seen[c] = true
		sem, ok := SemanticsOf(c)
		if !ok {
			t.Fatalf("capability %v has no semantics entry; either wire it or record that it is unwired", c)
		}
		if sem.Owner == "" && sem.Note == "" {
			t.Fatalf("capability %v is unimplemented but carries no explanation", c)
		}
	}
}

// TestUnconsumedCapabilitiesAreReported keeps the honest record visible: the bits
// with no core behaviour must be listable, so Preflight and a provider author's own
// test can see that "declared" is not "delivered".
func TestUnconsumedCapabilitiesAreReported(t *testing.T) {
	cs := Capabilities(
		CapSession, CapAudit, CapIdleSuspend,
		CapQueryStatus, CapPTY, CapHostedTrust,
	)
	got := cs.Unconsumed()
	names := make(map[string]bool, len(got))
	for _, c := range got {
		names[c.String()] = true
	}
	for _, want := range []string{"audit", "idle-suspend", "query-status", "pty", "hosted-trust"} {
		if !names[want] {
			t.Fatalf("%s should be reported as having no core behaviour, got %v", want, got)
		}
	}
	// And a bit that IS wired must not be swept into the same bucket.
	for _, c := range got {
		if c == CapSession {
			t.Fatal("CapSession is consulted by the core and must not be flagged")
		}
	}
	if honest := Capabilities(CapSession, CapFileBridge).Unconsumed(); len(honest) != 0 {
		t.Fatalf("a fully-wired set was flagged: %v", honest)
	}
}

// sandboxCapabilitiesHas reports whether Capabilities() can carry the bit, which is
// the same construction providers use.
func sandboxCapabilitiesHas(c Capability) bool {
	return Capabilities(c).Has(c)
}
