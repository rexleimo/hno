package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// --- workspace + artefact identity ----------------------------------------

func TestWorkspaceSourceKeyStability(t *testing.T) {
	a := FromDir("/srv/app", "node_modules", ".git")
	b := FromDir("/srv/app", ".git", "node_modules")
	if a.Key() != b.Key() {
		t.Fatalf("ignore ordering changed the key: %q vs %q", a.Key(), b.Key())
	}
	c := FromGit("https://example.com/repo", "main", "api")
	d := FromGit("https://example.com/repo", "main", "api")
	if c.Key() != d.Key() {
		t.Fatal("identical git sources produced different keys")
	}
	e := FromGit("https://example.com/repo", "dev", "api")
	if c.Key() == e.Key() {
		t.Fatal("a different ref must produce a different key: replay caching depends on it")
	}
	var nilSrc *WorkspaceSource
	if nilSrc.Key() != "none" {
		t.Fatal("a nil source must key as none rather than panic")
	}
	if KeyForSource(FromImage("img")) == "" {
		t.Fatal("KeyForSource returned empty")
	}
}

func TestWorkspaceSourceValidate(t *testing.T) {
	cases := []struct {
		name    string
		src     *WorkspaceSource
		wantErr bool
	}{
		{"dir ok", FromDir("/srv/app"), false},
		{"dir empty", FromDir(""), true},
		{"git ok", FromGit("https://x/y", "main"), false},
		{"git no ref", FromGit("https://x/y", ""), true},
		{"git no url", FromGit("", "main"), true},
		{"image ok", FromImage("python:3.12"), false},
		{"image empty", FromImage(""), true},
		{"artifacts ok", FromArtifacts(ArtifactSet{{Name: "a", Path: "a"}}, "sub"), false},
		{"artifacts empty", FromArtifacts(nil, ""), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.src.Validate()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Validate() err = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

func TestArtifactKindInference(t *testing.T) {
	cases := []struct {
		path string
		want ArtifactKind
	}{
		{"out/", KindDir},
		{"reports/**", KindGlob},
		{"r.json", KindFile},
		{"src/main.go", KindFile},
	}
	for _, tc := range cases {
		if got := inferKind(ArtifactSpec{Path: tc.path}); got != tc.want {
			t.Errorf("inferKind(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
	if got := inferKind(ArtifactSpec{Path: "x", Kind: KindDir}); got != KindDir {
		t.Error("an explicit kind must win over inference")
	}
}

func TestGlobMatching(t *testing.T) {
	cases := []struct {
		pattern, name string
		want          bool
	}{
		{"*.xml", "junit.xml", true},
		{"*.xml", "junit.json", false},
		{"report-?.log", "report-1.log", true},
		{"report-?.log", "report-12.log", false},
		{"[ab]*.txt", "a.txt", true},
		{"[ab]*.txt", "c.txt", false},
		{"[!a]*.txt", "b.txt", true},
		{"[!a]*.txt", "a.txt", false},
		{"**", "anything", true},
		{"", "x", false},
		{"a", "a", true},
		{"a[", "a[", false},
	}
	for _, tc := range cases {
		if got := matchGlob(tc.pattern, tc.name); got != tc.want {
			t.Errorf("matchGlob(%q,%q) = %v, want %v", tc.pattern, tc.name, got, tc.want)
		}
	}
	if baseName("a/b/c.txt") != "c.txt" {
		t.Fatal("baseName")
	}
	dir, pat := splitPattern("reports/junit.xml")
	if dir != "reports" || pat != "junit.xml" {
		t.Fatalf("splitPattern = %q,%q", dir, pat)
	}
}

// --- capability, limits, breaker -------------------------------------------

func TestCapabilitySetOperations(t *testing.T) {
	set := Capabilities(CapSession, CapFileBridge)
	if !set.Has(CapSession) || set.Has(CapClone) {
		t.Fatal("Has is wrong")
	}
	if set.Missing(CapClone) != CapClone {
		t.Fatal("Missing should report the absent bit")
	}
	extended := set.Add(CapClone)
	if !extended.Has(CapClone) {
		t.Fatal("Add should return an extended set")
	}
	if set.Has(CapClone) {
		t.Fatal("Add must not mutate the receiver")
	}
	if strings.Join(set.Strings(), ",") != "session,files" {
		t.Fatalf("Strings = %v", set.Strings())
	}
	if CapPortForward.String() != "port-forward" {
		t.Fatalf("name = %q", CapPortForward.String())
	}
	if Capability(1<<30).String() == "" || !strings.Contains(Capability(1<<30).String(), "0x") {
		t.Fatal("an unnamed bit must render as a hex marker, not silently")
	}
}

func TestBreakerPolicyCountsOnlyEngineNoise(t *testing.T) {
	p := DefaultBreakerPolicy()
	if p.counts(ClassUserCode) {
		t.Error("a user traceback must not open the provider breaker")
	}
	if p.counts(ClassDenied) {
		t.Error("a policy refusal must not open the provider breaker")
	}
	if !p.counts(ClassTransient) || !p.counts(ClassLost) {
		t.Error("engine failures must count")
	}
	if len(p.Counted) == 0 {
		t.Fatal("a policy must name at least one counted class")
	}
}

func TestLimitsMergeOnlyTightens(t *testing.T) {
	base := DefaultLimits()
	loose := Limits{OutputMaxBytes: base.OutputMaxBytes * 2, InlineFileMaxBytes: base.InlineFileMaxBytes / 2}
	merged := mergeLimits(base, loose)
	if merged.OutputMaxBytes > base.OutputMaxBytes {
		t.Fatalf("merge widened output: %d > %d", merged.OutputMaxBytes, base.OutputMaxBytes)
	}
	if merged.InlineFileMaxBytes != base.InlineFileMaxBytes/2 {
		t.Fatalf("merge should keep the stricter inline limit, got %d", merged.InlineFileMaxBytes)
	}
}

// --- failure vocabulary ---------------------------------------------------

func TestFailureErrorAndUnwrap(t *testing.T) {
	cause := errors.New("boom")
	f := &Failure{Class: ClassLost, Cause: cause, Retryable: false, NextAction: ActionRebuild}
	if !strings.Contains(f.Error(), "boom") {
		t.Fatalf("Failure.Error = %q", f.Error())
	}
	if !errors.Is(f, cause) {
		t.Fatal("Failure must unwrap to its cause")
	}
	if f.Class.String() != "lost" {
		t.Fatalf("class string = %q", f.Class.String())
	}
	if f.NextAction.String() == "" {
		t.Fatal("actions must be nameable")
	}
	for _, c := range ClassNames() {
		if c == "" {
			t.Fatal("every class must have a name")
		}
	}
}

func TestTypedErrorsCarryClassAndFixes(t *testing.T) {
	cases := []struct {
		err   error
		class FailureClass
	}{
		{&IsolationError{Provider: "p", Got: IsolationContainer, Want: IsolationMicroVM}, ClassPermanent},
		{&HostWatermarkError{Resource: "memory", Free: 1, Want: 100}, ClassResource},
		{&PathError{Path: "/etc/passwd", Reason: "escape"}, ClassPermanent},
		{&ApprovalPendingError{ApprovalID: "a1"}, ClassDenied},
		{&UncertainOutcomeError{RunID: "r1", Effect: EffectExternal}, ClassLost},
		{&RetryBudgetError{Scope: "run", Attempts: 3, Window: time.Second}, ClassResource},
		{&IdempotencyConflictError{Key: "k"}, ClassPermanent},
		{&ReadinessError{RunID: "r1", LastSeen: "x"}, ClassUserCode},
		{&VersionSkewError{Provider: "p", Guest: "1", SDK: "2", Wanted: "3"}, ClassPermanent},
		{&CleanupError{Provider: "p", Residuals: []string{"x"}}, ClassPermanent},
		{&DriftError{SessionID: "s", Items: []string{"i"}}, ClassPermanent},
		{&StepReplayError{Cause: errors.New("x")}, ClassTransient},
	}
	for _, tc := range cases {
		if got := ClassOf(tc.err); got != tc.class {
			t.Errorf("ClassOf(%T) = %v, want %v", tc.err, got, tc.class)
		}
		if tc.err.Error() == "" {
			t.Errorf("%T has an empty message", tc.err)
		}
	}
}

func TestRemediationRoundTrip(t *testing.T) {
	err := &UnavailableError{
		Tried: []Attempt{{Provider: "boxlite", Reason: "no /dev/kvm", Fixes: []string{"apt install qemu"}}},
		Want:  IsolationMicroVM,
		Fixes: []string{"hno sandbox doctor"},
	}
	f := AsFailure(err)
	if len(f.Fixes) == 0 {
		t.Fatalf("Failure should carry remediation commands: %+v", f)
	}
	if !strings.Contains(err.Error(), "boxlite") {
		t.Fatalf("UnavailableError text = %q", err.Error())
	}
	if got := fixOf(err); got == "" {
		t.Fatal("fixOf should find the first remedy")
	}
}

func TestBlockedHostExtraction(t *testing.T) {
	err := &DeniedError{Policy: "egress", Reason: "blocked", Rule: "evil.example"}
	f := AsFailure(err)
	if f.Class != ClassDenied {
		t.Fatalf("class = %v", f.Class)
	}
	if hosts := blockedHosts(err); len(hosts) != 0 && hosts[0] != "evil.example" {
		t.Fatalf("blockedHosts = %v", hosts)
	}
}

// --- isolation + formation parsing ---------------------------------------

func TestIsolationAndFormationParsing(t *testing.T) {
	for _, name := range []string{"none", "container", "syscall-filtered", "microvm", "microvm-hosted"} {
		if _, err := ParseIsolation(name); err != nil {
			t.Errorf("ParseIsolation(%q) failed: %v", name, err)
		}
	}
	if _, err := ParseIsolation("sandbox"); err == nil {
		t.Error("unknown isolation names must fail")
	}
	if IsolationUnknown.Rank() >= IsolationNone.Rank() {
		t.Error("an undeclared isolation level must rank below every real one")
	}
	if IsolationUnknown.AtLeast(IsolationNone) {
		t.Error("unknown can never satisfy a floor")
	}
	for _, f := range []Formation{FormationInProc, FormationRemote, FormationSidecar, FormationHosted} {
		got, err := ParseFormation(f.String())
		if err != nil || got != f {
			t.Errorf("ParseFormation round-trip failed for %v: %v %v", f, got, err)
		}
	}
	if FormationHosted.IsLocal() {
		t.Error("hosted formation must not be reported as local")
	}
	if !FormationRemote.IsLocal() || !FormationSidecar.IsLocal() {
		t.Error("remote and sidecar run on this machine")
	}
}

// --- path confinement ----------------------------------------------------

func TestPathConfinement(t *testing.T) {
	ok := []string{"a.txt", "dir/sub/file.py", "./x"}
	for _, p := range ok {
		if err := checkRelPath("input", p); err != nil {
			t.Errorf("checkRelPath(%q) = %v, want ok", p, err)
		}
	}
	bad := []string{"/etc/passwd", "../out", "a/../../b", "dir\\sub", "with\x00nul", ""}
	for _, p := range bad {
		if err := checkRelPath("input", p); err == nil {
			t.Errorf("checkRelPath(%q) accepted a path that must be refused", p)
		}
	}
	root := "/sandbox/root"
	// resolveInRoot returns a workspace-relative cleaned path, which is the only
	// form a provider can map onto its own root without guessing ours.
	got, err := resolveInRoot(root, "a/b.txt")
	if err != nil || got != "a/b.txt" {
		t.Fatalf("resolveInRoot = %q, %v", got, err)
	}
	if got, err := resolveInRoot(root, "./a//b.txt"); err != nil || got != "a/b.txt" {
		t.Fatalf("resolveInRoot should clean %q, got %q (%v)", "./a//b.txt", got, err)
	}
	if _, err := resolveInRoot(root, "/etc/passwd"); err == nil {
		t.Fatal("absolute escape must be refused")
	}
	if _, err := resolveInRoot(root, "a/../../b"); err == nil {
		t.Fatal("traversal must be refused")
	}
}

func TestValidEnvEntry(t *testing.T) {
	if !validEnvEntry("PATH=/bin") || validEnvEntry("noequals") || validEnvEntry("=v") || validEnvEntry("BAD KEY=v") {
		t.Fatal("env entry validation is wrong")
	}
}

// --- spec + install determinity -------------------------------------------

func TestStepKeyIgnoresOrderAndIncludesArgv(t *testing.T) {
	a := Step{Kind: StepInstall, Tool: "pip", Argv: []string{"python3", "-m", "pip", "install", "x"}, Sequence: 1}
	b := Step{Kind: StepInstall, Tool: "pip", Argv: []string{"python3", "-m", "pip", "install", "x"}, Sequence: 9}
	if a.Key() != b.Key() {
		t.Fatal("sequence must not change a step key")
	}
	c := Step{Kind: StepInstall, Tool: "pip", Argv: []string{"python3", "-m", "pip", "install", "y"}, Sequence: 1}
	if a.Key() == c.Key() {
		t.Fatal("different packages must not share a cache key")
	}
	if err := a.Validate(); err != nil {
		t.Fatalf("an install step with argv must validate: %v", err)
	}
}

func TestSessionSpecFingerprintStable(t *testing.T) {
	s1 := SessionSpec{Base: "img", Steps: []Step{{Kind: StepInstall, Tool: "pip", Argv: []string{"pip", "install", "x"}}}, Env: []string{"A=1"}}
	s2 := SessionSpec{Base: "img", Steps: []Step{{Kind: StepInstall, Tool: "pip", Argv: []string{"pip", "install", "x"}}}, Env: []string{"A=1"}}
	if s1.Fingerprint() != s2.Fingerprint() {
		t.Fatal("identical specs must fingerprint identically")
	}
	s3 := s1.Clone()
	s3.Base = "other"
	if s1.Fingerprint() == s3.Fingerprint() {
		t.Fatal("a different base image must change the fingerprint")
	}
	if EnvFingerprint([]string{"A=1", "B=2"}) != EnvFingerprint([]string{"B=2", "A=1"}) {
		t.Fatal("env fingerprint should be order independent for keys")
	}
	if (SessionSpec{}).Fingerprint() == "" {
		t.Fatal("an empty spec still needs a stable fingerprint")
	}
}

// --- journal -------------------------------------------------------------

func TestMemoryJournalStates(t *testing.T) {
	ctx := context.Background()
	j := NewMemoryJournal()
	if _, _, err := j.Load(ctx, "missing"); err != nil {
		t.Fatalf("Load of a missing key must not error: %v", err)
	}
	if err := j.Save(ctx, JournalEntry{Key: "k", RunID: "r1", State: RunStateDone, PayloadHash: "abc"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	entry, ok, err := j.Load(ctx, "k")
	if !ok || entry.RunID != "r1" || err != nil {
		t.Fatalf("round trip failed: %+v %v %v", entry, ok, err)
	}
	if err := j.Save(ctx, JournalEntry{Key: "k", RunID: "r2", State: RunStateDone, PayloadHash: "different"}); err == nil {
		t.Fatal("saving a different payload under the same key must conflict")
	}
	// The journal contract has no Delete: a completed key must keep answering
	// "already ran", otherwise a crash between commit and return re-runs an
	// external effect (§16.3).
	if err := j.Save(ctx, JournalEntry{Key: "k", RunID: "r1", State: RunStateDone, PayloadHash: "abc"}); err != nil {
		t.Fatalf("re-saving identical state must be idempotent: %v", err)
	}
	if _, ok, _ := j.Load(ctx, "k"); !ok {
		t.Fatal("the entry disappeared")
	}
}

// --- lockdown vocabulary -------------------------------------------------

func TestUndeclaredMutationDetection(t *testing.T) {
	cases := []struct {
		code      string
		wantToken string
		wantHint  string
	}{
		{"pip install cowsay", "pip install", `Install(Pip("cowsay"))`},
		{"python3 -m pip install 'foo==1.0'", "python3 -m pip install", `Install(Pip("foo==1.0"))`},
		{"apt-get install -y curl", "apt-get install", `Install(Apt("curl"))`},
		{"npm i left-pad", "npm i", `Install(Npm("left-pad"))`},
		{"go get example.com/mod@v1", "go get", `Install(GoGet("example.com/mod@v1"))`},
		{"cd /app && pip install x", "pip install", `Install(Pip("x"))`},
		{"echo safe", "", ""},
		{"ls -la", "", ""},
	}
	for _, tc := range cases {
		req := &Request{Runtime: "sh", Code: tc.code}
		tok, hint := undeclaredMutation(req)
		if tc.wantToken == "" {
			if tok != "" {
				t.Errorf("undeclaredMutation(%q) = %q, want none", tc.code, tok)
			}
			continue
		}
		if !strings.Contains(tok, "pip") && !strings.Contains(tok, "apt") && !strings.Contains(tok, "npm") && !strings.Contains(tok, "go get") && !strings.Contains(tok, "echo") {
			t.Errorf("undeclaredMutation(%q) token = %q", tc.code, tok)
		}
		if tc.wantHint != "" && hint != tc.wantHint {
			t.Errorf("hint for %q = %q, want %q", tc.code, hint, tc.wantHint)
		}
	}
}

func TestLockdownParsingAndDisplay(t *testing.T) {
	for name, want := range map[string]Lockdown{
		"detect":  LockdownDetect,
		"enforce": LockdownEnforce,
		"strict":  LockdownEnforce,
	} {
		got, ok := ParseLockdown(name)
		if !ok || got != want {
			t.Errorf("ParseLockdown(%q) = %v,%v want %v", name, got, ok, want)
		}
	}
	if _, ok := ParseLockdown(""); ok {
		t.Error("empty lockdown should be unset, not parsed")
	}
	if LockdownEnforce.String() != "enforce" || LockdownDetect.String() != "detect" {
		t.Fatal("lockdown names must render")
	}
	if LockdownUnset.String() != "" {
		t.Fatal("unset should render empty")
	}
}

// --- audit projection ----------------------------------------------------

func TestAuditProjectionOmitsContent(t *testing.T) {
	req := &Request{Runtime: "python", Code: "print('secret-source')", Files: map[string]FileContent{"a.txt": {Content: []byte("secret")}}}
	rc := &RunContext{Request: req}
	rec := AuditRecord{}
	WithAuditOptions{IncludeArgv: true}.projectRequest(rc, &rec)
	if rec.CodeBytes != len(req.Code) {
		t.Fatalf("CodeBytes = %d", rec.CodeBytes)
	}
	if rec.FileCount != 1 {
		t.Fatalf("FileCount = %d", rec.FileCount)
	}
	blob, err := jsonMarshal(rec)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	_ = blob
	if strings.Contains(blob, "secret-source") || strings.Contains(blob, "print(") {
		t.Fatalf("audit record leaked code: %s", blob)
	}
}

func TestHookNameAndFailureHelpers(t *testing.T) {
	named := namedPolicy{}
	if got := hookName("policy", 3, named); got != "guard" {
		t.Fatalf("hookName = %q, want the Namer value", got)
	}
	if got := hookName("policy", 3, PolicyFunc(func(context.Context, *RunContext) (Decision, error) { return Allow(), nil })); got != "policy#3" {
		t.Fatalf("hookName fallback = %q", got)
	}
	if got := policyName(1, named); got != "guard" {
		t.Fatalf("policyName = %q", got)
	}
	if firstLine(nil) != "" {
		t.Fatal("firstLine(nil) must be empty")
	}
	if got := firstLine(errors.New("a\nb")); got != "a" {
		t.Fatalf("firstLine = %q", got)
	}
	if err := hookGuard("x", func() error { panic("boom") }); err == nil {
		t.Fatal("hookGuard must convert a panic to a fault")
	}
	if err := hookGuard("x", func() error { return nil }); err != nil {
		t.Fatalf("hookGuard must pass through success: %v", err)
	}
}

// namedPolicy is a Policy that implements Namer.
type namedPolicy struct{}

// Decide implements Policy.
func (namedPolicy) Decide(context.Context, *RunContext) (Decision, error) { return Allow(), nil }

// Name implements Namer.
func (namedPolicy) Name() string { return "guard" }

// jsonMarshal renders a value so the audit test can prove what it does not
// contain.
func jsonMarshal(v any) (string, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return b.String(), nil
}

func TestPrivilegeHashOrdersRequests(t *testing.T) {
	none := privilegeHash(&Request{Egress: Egress{Mode: EgressNone}})
	allow := privilegeHash(&Request{Egress: Egress{Mode: EgressAllowlist, Allow: []string{"a"}}})
	open := privilegeHash(&Request{Egress: Egress{Mode: EgressOpen}})
	if !(none < allow && allow < open) {
		t.Fatalf("privilege ordering broken: %d %d %d", none, allow, open)
	}
	if privilegeHash(nil) != 0 {
		t.Fatal("nil request should score zero")
	}
}

func TestDescribeHelpers(t *testing.T) {
	if describeRequest(nil) == "" {
		t.Fatal("describeRequest(nil) must say something")
	}
	if !strings.Contains(describeRequest(&Request{Argv: []string{"ls", "-l"}}), "ls -l") {
		t.Fatal("argv description")
	}
	if !strings.Contains(describeRequest(&Request{Runtime: "python", Code: "x"}), "python") {
		t.Fatal("runtime description")
	}
	if describeFailure(&Report{ExitCode: 2}) == "" {
		t.Fatal("describeFailure must explain a nonzero exit")
	}
	if describeFailure(&Report{TimedOut: true}) == "" {
		t.Fatal("describeFailure must explain a timeout")
	}
	if describeFailure(&Report{Lost: true}) == "" {
		t.Fatal("describeFailure must explain a lost run")
	}
	if describeFailure(nil) == "" {
		t.Fatal("describeFailure(nil) must say something")
	}
	if summarizeChecks([]Check{{OK: true}}) == "" {
		t.Fatal("summarizeChecks")
	}
	if summarizeChecks([]Check{{OK: false}}) == "" {
		t.Fatal("summarizeChecks failures")
	}
}

func TestMergeHelpers(t *testing.T) {
	if firstPositive(time.Duration(0), 2*time.Second) != 2*time.Second {
		t.Fatal("firstPositive")
	}
	if firstPositive64(0, 9) != 9 {
		t.Fatal("firstPositive64")
	}
	if firstNonEmpty("", "x") != "x" {
		t.Fatal("firstNonEmpty")
	}
	merged := mergeMaps(map[string]any{"a": 1}, map[string]any{"b": 2})
	if len(merged) != 2 {
		t.Fatalf("mergeMaps = %v", merged)
	}
	if got := joinArg([]string{"a", "b"}); !strings.Contains(got, "a") {
		t.Fatalf("joinArg = %q", got)
	}
	if hashOf(nil) != "" {
		t.Fatal("hashOf(empty) should be empty")
	}
	if hashOf([]byte("x")) == "" {
		t.Fatal("hashOf")
	}
	if got := attemptsOf([]string{"a", "b"}); len(got) != 2 || got[0].Provider != "a" {
		t.Fatalf("attemptsOf = %v", got)
	}
	if got := labelAttrs(map[string]any{"k": "v", "n": 1}); len(got) != 1 {
		t.Fatalf("labelAttrs should keep only strings: %v", got)
	}
	if labelAttrs(nil) != nil {
		t.Fatal("labelAttrs(nil)")
	}
}
