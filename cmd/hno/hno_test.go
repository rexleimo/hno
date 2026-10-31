package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/rexleimo/agno-go/pkg/hno/sandbox"
	"github.com/rexleimo/agno-go/pkg/hno/sandbox/sandboxtest"
)

// The CLI is a thin shell, so the tests are behavioural: they run the same functions main
// runs, against the in-memory provider, and assert on exit codes and rendered text. Nothing
// here spawns a process, and nothing here fakes the runtime's own decisions.

func TestMain(m *testing.M) {
	if err := sandboxtest.Register(); err != nil {
		panic(err)
	}
	if err := reclaimFixture(); err != nil {
		panic(err)
	}
	// The memory provider is not container isolation, and several tests ask the CLI for
	// the floor it actually satisfies.
	m.Run()
}

func TestAttrFlagsParsing(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	cfg := commonFlags(fs)
	if err := fs.Parse([]string{"--attr", "runtime=runsc", "--attr", "network=none"}); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got := cfg.attrs.String(); got != "network=none runtime=runsc" {
		t.Errorf("attr flags render sorted, got %q", got)
	}

	bad := []string{"novalue", "=nokey"}
	for _, raw := range bad {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		cfg := commonFlags(fs)
		fs.Parse([]string{"--attr", raw})
		if err := fs.Parse([]string{"--attr", raw}); err == nil {
			t.Errorf("--attr %q accepted; a malformed engine attribute must not be dropped silently", raw)
		}
		_ = cfg
	}
}

func TestIsolationTypoIsRefusedNotIgnored(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	cfg := commonFlags(fs)
	if err := fs.Parse([]string{"--isolation", "container-ish"}); err != nil {
		t.Fatalf("flags: %v", err)
	}
	opts, err := cfg.options()
	if err == nil {
		t.Fatalf("options() accepted an unknown isolation tier; silently running below the floor the operator typed is the failure the floor exists to prevent")
	}
	if !strings.Contains(err.Error(), "--isolation") {
		t.Errorf("refusal should name the flag the user typed, got %q", err)
	}
	if len(opts) != 0 {
		t.Errorf("no options may be returned alongside a refusal, got %d", len(opts))
	}
}

// The mapping from flags to options is read back off the opened handle, never off the
// values remembered at construction time.
func TestFlagsLandOnTheOpenedHandle(t *testing.T) {
	ctx := context.Background()
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	cfg := commonFlags(fs)
	if err := fs.Parse([]string{"--provider", "memory", "--image", "alpine:3.20", "--isolation", "none"}); err != nil {
		t.Fatalf("flags: %v", err)
	}
	opts, err := cfg.options()
	if err != nil {
		t.Fatalf("options: %v", err)
	}
	sb, err := openHandle(ctx, opts...)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer sb.Close()
	st := sb.Status()
	if st.Provider != "memory" {
		t.Errorf("Status().Provider = %q, want memory", st.Provider)
	}
	if st.Image != "alpine:3.20" {
		t.Errorf("Status().Image = %q, want alpine:3.20", st.Image)
	}
	// --isolation is a floor, so the resolved tier is whatever the provider really is
	// (the memory double is container-class). What must never happen is the CLI
	// reporting a tier it did not get.
	if st.Isolation < sandbox.IsolationNone {
		t.Errorf("resolved isolation %v is below the floor that was asked for", st.Isolation)
	}

	// A floor the engine cannot reach has to stop the command, not be quietly dropped.
	fs = flag.NewFlagSet("t2", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	cfg = commonFlags(fs)
	if err := fs.Parse([]string{"--provider", "memory", "--isolation", "microvm"}); err != nil {
		t.Fatalf("flags: %v", err)
	}
	opts, err = cfg.options()
	if err != nil {
		t.Fatalf("options: %v", err)
	}
	unreachable, err := openHandle(ctx, opts...)
	if err == nil {
		unreachable.Close()
		t.Fatalf("opened a handle below the requested floor; --isolation micro-vm on a container provider must fail, not downgrade")
	}
	if !strings.Contains(err.Error(), "microvm") && !strings.Contains(err.Error(), "isolation") {
		t.Errorf("refusal should name the isolation gap, got %v", err)
	}
}

func TestBuildRequestKeepsTheTwoPayloadsApart(t *testing.T) {
	sh, err := buildRequest("deploy", nil)
	if err != nil {
		t.Fatalf("shell: %v", err)
	}
	if sh.Code != "deploy" || len(sh.Argv) != 0 {
		t.Errorf("--shell must produce a script payload, got Code=%q Argv=%v", sh.Code, sh.Argv)
	}
	av, err := buildRequest("", []string{"go", "test", "./..."})
	if err != nil {
		t.Fatalf("argv: %v", err)
	}
	if av.Code != "" || strings.Join(av.Argv, " ") != "go test ./..." {
		t.Errorf("argv must produce an exec payload, got Code=%q Argv=%v", av.Code, av.Argv)
	}
	if _, err := buildRequest("deploy", []string{"ls"}); err == nil {
		t.Errorf("both --shell and argv accepted; the CLI would have to guess which the operator meant")
	}
	if _, err := buildRequest("", nil); err == nil {
		t.Errorf("empty payload accepted; running nothing and reporting success is worse than refusing")
	}
}

func TestDoctorExitCodeTracksChecks(t *testing.T) {
	if allChecksOK(nil) {
		t.Errorf("a nil diagnosis must not read as healthy")
	}
	if allChecksOK(&sandbox.DiagReport{}) {
		t.Errorf("zero checks is not proof of health; an empty report used to exit 0 like a passing one")
	}
	ok := &sandbox.DiagReport{Checks: []sandbox.Check{{Env: "engine", OK: true}}}
	if !allChecksOK(ok) {
		t.Errorf("all checks passed should be healthy")
	}
	bad := &sandbox.DiagReport{Checks: []sandbox.Check{{Env: "engine", OK: true}, {Env: "image", OK: false, Want: "present", Got: "absent", Fix: "pull it"}}}
	if allChecksOK(bad) {
		t.Errorf("a failed check must decide the exit code")
	}
	var buf bytes.Buffer
	printDiag(&buf, bad)
	out := buf.String()
	for _, want := range []string{"engine", "image", "present", "absent", "pull it"} {
		if !strings.Contains(out, want) {
			t.Errorf("doctor output missing %q:\n%s", want, out)
		}
	}
	if strings.Count(out, "fix:") != 1 {
		t.Errorf("only failing checks should carry a fix line, got:\n%s", out)
	}
}

func TestDoctorJSONIsTheRuntimeReport(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := sandboxMain([]string{"doctor", "--provider", "memory", "--isolation", "none", "--json"}, &stdout, &stderr)
	if code != exitOK && code != exitDiagFailed {
		t.Fatalf("doctor exit = %d, want a health verdict (0 or %d); stderr=%s", code, exitDiagFailed, stderr.String())
	}
	var diag sandbox.DiagReport
	if err := json.Unmarshal(stdout.Bytes(), &diag); err != nil {
		t.Fatalf("--json did not emit a DiagReport: %v\n%s", err, stdout.String())
	}
	if len(diag.Checks) == 0 {
		t.Errorf("json diagnosis carried no checks; the machine-readable path must not be emptier than the human one")
	}
	verdict := 0
	for _, c := range diag.Checks {
		if !c.OK {
			verdict = exitDiagFailed
		}
	}
	if code != verdict {
		t.Errorf("exit %d disagrees with the checks it just printed (%v)", code, diag.Checks)
	}
}

// D-36 reached the command line: doctor used to print "all checks passed" for an image
// the engine cannot start, and exit 0. That green light is now a failing verdict, because
// a health report that promises less than the engine can deliver is worse than no report.
func TestDoctorFailsOnAnImageTheEngineCannotStart(t *testing.T) {
	const bogus = "hno-clinic/absent:1"
	recFixture.absentImages = []string{bogus}
	t.Cleanup(func() { recFixture.absentImages = nil })

	var stdout, stderr bytes.Buffer
	code := sandboxMain([]string{"doctor", "--provider", reclaimProviderName, "--image", bogus}, &stdout, &stderr)
	if code != exitDiagFailed {
		t.Fatalf("doctor exit = %d, want %d for an image that cannot start; output:\n%s", code, exitDiagFailed, stdout.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "image") || !strings.Contains(out, bogus) {
		t.Errorf("doctor must name the image it refused:\n%s", out)
	}
	if strings.Contains(out, "all ") && strings.Contains(out, "checks passed") {
		t.Errorf("doctor still reads as clean while an image is blocking:\n%s", out)
	}

	// And the image verdict alone flips to healthy for an image the engine has. The
	// overall exit code is not the evidence here: this fixture provider also trips the
	// capabilities-without-behaviour check, which is that check working correctly.
	recFixture.absentImages = nil
	stdout.Reset()
	stderr.Reset()
	if code := sandboxMain([]string{"doctor", "--provider", reclaimProviderName, "--image", "present:1", "--json"}, &stdout, &stderr); code != exitOK && code != exitDiagFailed {
		t.Fatalf("doctor exit = %d: %s", code, stderr.String())
	}
	var diag sandbox.DiagReport
	if err := json.Unmarshal(stdout.Bytes(), &diag); err != nil {
		t.Fatalf("--json after clearing the absent image: %v\n%s", err, stdout.String())
	}
	img, hit := false, false
	for _, c := range diag.Checks {
		if c.Env == "image" {
			hit, img = true, c.OK
		}
	}
	if !hit {
		t.Fatal("the image check disappeared once the image was available")
	}
	if !img {
		t.Errorf("a present image was still reported as unhealthy:\n%s", stdout.String())
	}
}

func TestRunPassesThePayloadStatusThrough(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := sandboxMain([]string{"run", "--provider", "memory", "--isolation", "none", "--shell", "#!exit 3"}, &stdout, &stderr)
	if code != 3 {
		t.Fatalf("exit = %d, want the payload's 3; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
}

// The distinction this CLI exists to keep: a program that failed is not an environment that
// failed, and the shell must be able to tell them apart.
func TestEnvironmentFailureIsNotReportedAsAPayloadFailure(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := sandboxMain([]string{"run", "--provider", "no-such-provider", "--shell", "exit 3"}, &stdout, &stderr)
	if code == 3 {
		t.Fatalf("exit 3 would blame the program for an environment that never opened")
	}
	if code != exitEnvFailure {
		t.Fatalf("exit = %d, want %d; stderr=%q", code, exitEnvFailure, stderr.String())
	}
	if !strings.Contains(stderr.String(), "status is unknown") && !strings.Contains(stderr.String(), "cannot open") {
		t.Errorf("environment failure must say the program's status is unknown, got %q", stderr.String())
	}
}

// timeout(1) reserves 124 for "ran but too slow". Collapsing it into 70 would tell a
// wrapping script the sandbox was broken when the program simply exceeded its budget.
func TestTimeoutIsTheProgramsVerdictNotTheEnvironments(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := sandboxMain([]string{"run", "--provider", "memory", "--isolation", "none", "--timeout", "50ms", "--shell", "#!timeout"}, &stdout, &stderr)
	if code == exitEnvFailure {
		t.Fatalf("a timed-out payload reported an environment failure")
	}
	if code != exitTimedOut {
		t.Fatalf("exit = %d, want %d; stderr=%q", code, exitTimedOut, stderr.String())
	}
	if !strings.Contains(stderr.String(), "timeout") {
		t.Errorf("must say the budget was hit, got %q", stderr.String())
	}

	// A lost environment stays an environment failure: no verdict was observed.
	stdout.Reset()
	stderr.Reset()
	if code := sandboxMain([]string{"run", "--provider", "memory", "--isolation", "none", "--shell", "#!lost"}, &stdout, &stderr); code != exitEnvFailure {
		t.Fatalf("lost environment exit = %d, want %d; stderr=%q", code, exitEnvFailure, stderr.String())
	}
}

func TestRunRefusesAnAmbiguousCommandLine(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := sandboxMain([]string{"run", "--provider", "memory", "--shell", "deploy", "--", "ls"}, &stdout, &stderr); code != exitUsage {
		t.Fatalf("both payload shapes accepted, exit = %d", code)
	}
	if code := sandboxMain([]string{"run", "--provider", "memory"}, &stdout, &stderr); code != exitUsage {
		t.Fatalf("nothing to run accepted, exit = %d", code)
	}
}

func TestUnknownVerbIsRefusedWithWhatExists(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := sandboxMain([]string{"healthcheck"}, &stdout, &stderr); code != exitUsage {
		t.Fatalf("exit = %d, want %d", code, exitUsage)
	}
	for _, want := range []string{"doctor", "prune", "run"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("refusal should list the verbs that exist, missing %q in %q", want, stderr.String())
		}
	}
	if code := run([]string{"nonsense"}, &stdout, &stderr); code != exitUsage {
		t.Fatalf("top-level unknown command exit = %d", code)
	}
	if code := run(nil, &stdout, &stderr); code != exitOK {
		t.Fatalf("no arguments should print usage and succeed, exit = %d", code)
	}
}

func TestPruneIsReadOnlyUnlessToldOtherwise(t *testing.T) {
	// Without --yes the destructive branch must not be reached at all.
	recStats.reset()
	var stdout, stderr bytes.Buffer
	if code := sandboxMain([]string{"prune", "--provider", reclaimProviderName}, &stdout, &stderr); code != exitOK {
		t.Fatalf("read-only prune exit = %d: %s", code, stderr.String())
	}
	rec := recStats.snapshot()
	// The runtime may sweep on its own when a handle opens, so assert the CLI added a
	// sweep rather than asserting an absolute count.
	if rec.sweeps < 1 {
		t.Errorf("Sweep calls = %d, want at least the CLI's own 1", rec.sweeps)
	}
	if rec.prunes != 0 {
		t.Errorf("Prune called %d times without --yes; a reclaim command must not delete on a guess", rec.prunes)
	}
	if !strings.Contains(stdout.String(), "--yes") {
		t.Errorf("read-only output must say how to make it effective, got %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "orphan-1") {
		t.Errorf("swept environments must be listed, got %q", stdout.String())
	}
	// What the provider declined is reported even in read-only mode.
	if !strings.Contains(stdout.String(), "declined to delete") {
		t.Errorf("refusals must be visible, got %q", stdout.String())
	}

	recStats.reset()
	stdout.Reset()
	stderr.Reset()
	if code := sandboxMain([]string{"prune", "--provider", reclaimProviderName, "--yes"}, &stdout, &stderr); code != exitOK {
		t.Fatalf("prune --yes exit = %d: %s", code, stderr.String())
	}
	rec = recStats.snapshot()
	if rec.prunes != 1 {
		t.Errorf("Prune calls = %d with --yes, want 1", rec.prunes)
	}
	if !strings.Contains(stdout.String(), "reclaimed 1") {
		t.Errorf("must report what was reclaimed, got %q", stdout.String())
	}
}

// A provider without a reclaimer has no orphans by construction, and saying "0" would look
// identical to a successful sweep. The CLI has to name the difference.
func TestPruneWithoutAReclaimerSaysSo(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := sandboxMain([]string{"prune", "--provider", "memory"}, &stdout, &stderr); code != exitOK {
		t.Fatalf("exit = %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "does not reclaim") {
		t.Errorf("got %q", stdout.String())
	}
	if strings.Contains(stdout.String(), "0 environment") {
		t.Errorf("must not imitate a clean sweep, got %q", stdout.String())
	}
}

// ---- fixtures ----

const reclaimProviderName = "fixture-reclaim"

type reclaimStats struct {
	mu     sync.Mutex
	sweeps int
	prunes int
}

type fixtureProvider struct {
	*sandboxtest.Provider
	stats        *reclaimStats
	absentImages []string
}

// ImageAvailable makes the fixture answer the image question, so the CLI's doctor
// behaviour on an absent image is exercised offline against the same interface the
// docker provider implements.
func (p *fixtureProvider) ImageAvailable(_ context.Context, image string) (bool, error) {
	for _, absent := range p.absentImages {
		if absent == image {
			return false, nil
		}
	}
	return true, nil
}

func (p *fixtureProvider) Sweep(ctx context.Context) ([]string, error) {
	p.stats.mu.Lock()
	p.stats.sweeps++
	p.stats.mu.Unlock()
	return []string{"orphan-1"}, nil
}

func (p *fixtureProvider) Prune(ctx context.Context) ([]string, error) {
	p.stats.mu.Lock()
	p.stats.prunes++
	p.stats.mu.Unlock()
	return []string{"orphan-1"}, nil
}

// SkippedPrune mirrors the docker provider's refusal report so the CLI's reading of it is
// exercised against the same shape the real engine produces.
func (p *fixtureProvider) SkippedPrune() []string {
	return []string{"orphan-2 belongs to another process (pid 4242)"}
}

var recStats = &reclaimStats{}
var recFixture *fixtureProvider

func (s *reclaimStats) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweeps, s.prunes = 0, 0
}

func (s *reclaimStats) snapshot() reclaimStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return reclaimStats{sweeps: s.sweeps, prunes: s.prunes}
}

func reclaimFixture() error {
	base, err := sandboxtest.NewProvider(sandbox.ProviderConfig{})
	if err != nil {
		return err
	}
	recFixture = &fixtureProvider{Provider: base.(*sandboxtest.Provider), stats: recStats}
	return sandbox.RegisterProvider(reclaimProviderName, func(sandbox.ProviderConfig) (sandbox.Provider, error) {
		return recFixture, nil
	})
}

// The CLI advertises a vocabulary in its flag help. If the names it prints are not the
// names the parser accepts, the help text is a defect even when every code path works --
// so the round trip is pinned here and the printed list is compared against it.
var vmLikeRe = regexp.MustCompile(`\b[a-z][a-z0-9_-]*vm[a-z0-9_-]*\b`)

func TestIsolationVocabularyRoundTrips(t *testing.T) {
	tiers := []sandbox.Isolation{
		sandbox.IsolationNone,
		sandbox.IsolationContainer,
		sandbox.IsolationSyscallFiltered,
		sandbox.IsolationMicroVM,
		sandbox.IsolationMicroVMHosted,
	}
	names := make([]string, 0, len(tiers))
	for _, iso := range tiers {
		back, err := sandbox.ParseIsolation(iso.String())
		if err != nil {
			t.Errorf("Isolation %v renders %q, which ParseIsolation rejects: %v", iso, iso.String(), err)
			continue
		}
		if back != iso {
			t.Errorf("%q parsed back as %v, want %v", iso.String(), back, iso)
		}
		names = append(names, iso.String())
	}

	fs := flag.NewFlagSet("vocab", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	commonFlags(fs)
	var help bytes.Buffer
	fs.SetOutput(&help)
	fs.PrintDefaults()
	for _, n := range names {
		if !strings.Contains(help.String(), n) {
			t.Errorf("--isolation help does not list %q, so an operator cannot discover it:\n%s", n, help.String())
		}
	}
	// Any word in the help that looks like a tier name has to parse. This is what catches
	// an edited help string that advertises a vocabulary the runtime refuses -- the class
	// of defect the rest of this file is about. flag.PrintDefaults wraps at column ~40,
	// which would split "micro-vm" across lines and make the scan blind, so whitespace is
	// flattened first.
	flat := strings.Join(strings.Fields(help.String()), " ")
	for _, cand := range vmLikeRe.FindAllString(flat, -1) {
		if _, err := sandbox.ParseIsolation(cand); err != nil {
			t.Errorf("--isolation help advertises %q, which the parser refuses: %v", cand, err)
		}
	}
}
