package boxlite

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rexleimo/agno-go/pkg/hno/sandbox"
)

// fakeRunner records what was asked of the engine and answers from a table.
//
// It records arguments rather than only outcomes on purpose: a fake that just replays
// "success" would agree with a provider that built the wrong command line, and the
// whole point of these tests is the command line.
type fakeRunner struct {
	calls   [][]string
	stdins  []string
	replies map[string]Outcome
	errs    map[string]error
}

func (f *fakeRunner) Run(ctx context.Context, cmd Command) (Outcome, error) {
	f.calls = append(f.calls, cmd.Args)
	f.stdins = append(f.stdins, cmd.Stdin)
	key := verbOf(cmd.Args)
	if cmd.OnChunk != nil {
		if out, ok := f.replies[key]; ok && out.Stdout != "" {
			cmd.OnChunk(sandbox.StreamStdout, []byte(out.Stdout))
		}
	}
	if err, ok := f.errs[key]; ok {
		return Outcome{}, err
	}
	if out, ok := f.replies[key]; ok {
		if cmd.OnChunk != nil {
			out.Streamed = int64(len(out.Stdout))
		}
		return out, nil
	}
	return Outcome{}, nil
}

// verbOf finds the CLI verb in an argument list. Keying on Args[0] only works while no
// global flag precedes the verb; with a registry configured the first token is
// --registry, and a double that mis-keys silently answers nothing.
func verbOf(args []string) string {
	verbs := map[string]bool{"info": true, "images": true, "create": true, "start": true, "exec": true, "list": true, "rm": true, "network": true, "stop": true, "run": true}
	for _, a := range args {
		if verbs[a] {
			return a
		}
	}
	if len(args) > 0 {
		return args[0]
	}
	return ""
}

func (f *fakeRunner) lastCall() []string {
	if len(f.calls) == 0 {
		return nil
	}
	return f.calls[len(f.calls)-1]
}

func newTestProvider(t *testing.T, f *fakeRunner) *Provider {
	t.Helper()
	p, err := New(Config{Image: "alpine:latest", Runner: f})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

func TestProbeAcceptsAnEngineThatReportsVirtualization(t *testing.T) {
	f := &fakeRunner{replies: map[string]Outcome{
		"info": {Stdout: "boxlite 0.11.0\nos: linux\narch: x86_64\nvirtualization: available\n"},
	}}
	if err := newTestProvider(t, f).Probe(context.Background()); err != nil {
		t.Fatalf("Probe on a virtualisation-capable engine: %v", err)
	}
}

// The three answers must stay three answers. Collapsing "the engine said no" into "the
// engine said nothing" is how a host without KVM gets treated as ready.
func TestProbeDistinguishesNoFromNoAnswer(t *testing.T) {
	cases := []struct {
		name    string
		stdout  string
		wantErr bool
		wantIn  string
	}{
		{name: "answered unavailable", stdout: "virtualization: unavailable\n", wantErr: true, wantIn: "not available"},
		{name: "answered available", stdout: "virtualization: available\n", wantErr: false},
		{name: "never answered", stdout: "boxlite 0.11.0\nos: linux\n", wantErr: true, wantIn: "did not report whether virtualisation is available"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newTestProvider(t, &fakeRunner{replies: map[string]Outcome{"info": {Stdout: tc.stdout}}})
			err := p.Probe(context.Background())
			if tc.wantErr != (err != nil) {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if tc.wantErr && !strings.Contains(err.Error(), tc.wantIn) {
				t.Fatalf("error %q does not mention %q", err, tc.wantIn)
			}
		})
	}
}

func TestProbeNamesTheInstallFixWhenTheBinaryIsMissing(t *testing.T) {
	f := &fakeRunner{errs: map[string]error{"info": errors.New(`exec: "boxlite": executable file not found in $PATH`)}}
	err := newTestProvider(t, f).Probe(context.Background())
	if err == nil {
		t.Fatal("Probe succeeded with no binary; a missing engine is not a passing host")
	}
	var ue *sandbox.UnavailableError
	if !errors.As(err, &ue) {
		t.Fatalf("error is %T, want *sandbox.UnavailableError so a caller can render remediation", err)
	}
	if rem := strings.Join(ue.Remediation(), " "); !strings.Contains(rem, "install boxlite") {
		t.Fatalf("remediation %q does not name the install step", rem)
	}
}

// A capability set is a claim about the engine, and the core refuses a claim with no
// interface behind it. This asserts the claim survives the core's own audit.
func TestCapabilitiesSurviveTheCoresAudit(t *testing.T) {
	p := newTestProvider(t, &fakeRunner{})
	if errs := p.Capabilities().VerifyClaims(p); len(errs) > 0 {
		t.Fatalf("the core rejected this provider's capability claim: %v", errs)
	}
	for _, absent := range []sandbox.Capability{sandbox.CapSnapshot, sandbox.CapClone, sandbox.CapRebuild, sandbox.CapFileBridge} {
		if p.Capabilities().Has(absent) {
			t.Fatalf("claimed %s, which the CLI surface does not back", absent)
		}
	}
}

func TestCreateUsesTheMeasuredCLIShape(t *testing.T) {
	f := &fakeRunner{replies: map[string]Outcome{
		"create": {Stdout: "b0x1d\n"},
		"start":  {},
	}}
	p := newTestProvider(t, f)
	if _, err := p.NewEnvironment(context.Background(), sandbox.EnvSpec{
		SessionID: "s-1", Image: "alpine:latest",
	}); err != nil {
		t.Fatalf("NewEnvironment: %v", err)
	}
	// `create` rejects `--` outright (measured: `run` and `create` parse the first word
	// after the image as an executable), and it takes no --detach, so the box must be
	// kept alive by an idle entrypoint instead.
	create := f.calls[0]
	if contains(create, "--") {
		t.Fatalf("create args %v contain `--`, which boxlite run/create parse as an executable", create)
	}
	if contains(create, "--detach") {
		t.Fatalf("create args %v pass --detach, which `boxlite create` does not accept", create)
	}
	if len(create) < 3 || create[len(create)-3] != "alpine:latest" || create[len(create)-1] != "infinity" {
		t.Fatalf("create args %v do not end with an idle entrypoint; the box would exit immediately", create)
	}
	if got := f.calls[1]; len(got) != 2 || got[0] != "start" || got[1] != "b0x1d" {
		t.Fatalf("start call = %v, want [start b0x1d]", got)
	}
}

// A box that is created but will not start must not be adopted, and must not be left
// behind either. Both halves matter: adopting it hands the caller an unusable
// environment, and abandoning it leaks a VM.
func TestStartFailureRemovesTheHalfCreatedBox(t *testing.T) {
	f := &fakeRunner{replies: map[string]Outcome{
		"create": {Stdout: "b0x1d\n"},
		"start":  {Exit: 1, Stderr: "cannot start: no disk space\n"},
		"rm":     {},
	}}
	p := newTestProvider(t, f)
	env, err := p.NewEnvironment(context.Background(), sandbox.EnvSpec{Image: "alpine:latest"})
	if err == nil {
		t.Fatalf("NewEnvironment returned %v with a start failure", env)
	}
	if !contains(f.lastCall(), "b0x1d") || f.lastCall()[0] != "rm" {
		t.Fatalf("last call %v is not `rm b0x1d`: the half-created box was left behind", f.lastCall())
	}
	// The remediation, not Error(): a fix that only exists in Remediation() is
	// invisible to Error(), and asserting Error() here would pass on an error that
	// tells an operator nothing.
	var ue *sandbox.UnavailableError
	if !errors.As(err, &ue) {
		t.Fatalf("error is %T, want *sandbox.UnavailableError", err)
	}
	if rem := strings.Join(ue.Remediation(), " "); !strings.Contains(rem, "would not start") {
		t.Fatalf("remediation %q does not say the box would not start", rem)
	}
}

func TestExecFeedsThePayloadOnStdinNotArgv(t *testing.T) {
	f := &fakeRunner{replies: map[string]Outcome{
		"create": {Stdout: "b0x1d\n"},
		"start":  {},
		"list":   {Stdout: "┌──────┬───────────────┬─────────┐\n│ ID   │ IMAGE         │ STATUS  │\n│ b0x1d│ alpine:latest │ Running │\n└──────┴───────────────┴─────────┘\n"},
		"exec":   {Stdout: "hi\n"},
	}}
	p := newTestProvider(t, f)
	env, err := p.NewEnvironment(context.Background(), sandbox.EnvSpec{Image: "alpine:latest"})
	if err != nil {
		t.Fatalf("NewEnvironment: %v", err)
	}
	rep, err := env.Exec(context.Background(), sandbox.RunSpec{
		RunID:   "r1",
		Request: sandbox.Shell("echo hi"),
	}, nil)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if rep.ExitCode != 0 || rep.Stdout != "hi\n" {
		t.Fatalf("report = exit %d stdout %q, want 0 / \"hi\\n\"", rep.ExitCode, rep.Stdout)
	}
	exec := f.lastCall()
	if !contains(exec, "--") {
		t.Fatalf("exec args %v have no `--` separator; boxlite exec needs it before the command", exec)
	}
	if strings.Join(exec, " ") == "" || contains(exec, "echo hi") {
		t.Fatalf("exec args %v carry the payload on the command line", exec)
	}
	if f.stdins[len(f.stdins)-1] != "echo hi" {
		t.Fatalf("payload on stdin = %q, want the shell script", f.stdins[len(f.stdins)-1])
	}
}

// The contract that keeps a toolkit from retrying a typo forever: a nonzero exit is the
// payload speaking, so it is a populated report with a nil error.
func TestExecNonzeroExitIsAReportNotAnError(t *testing.T) {
	f := &fakeRunner{replies: map[string]Outcome{
		"create": {Stdout: "b0x1d\n"},
		"start":  {},
		"list":   {Stdout: "│ b0x1d │ alpine:latest │ Running │\n"},
		"exec":   {Exit: 7, Stderr: "boom\n"},
	}}
	p := newTestProvider(t, f)
	env, err := p.NewEnvironment(context.Background(), sandbox.EnvSpec{Image: "alpine:latest"})
	if err != nil {
		t.Fatalf("NewEnvironment: %v", err)
	}
	rep, err := env.Exec(context.Background(), sandbox.RunSpec{RunID: "r1", Request: sandbox.Command("false")}, nil)
	if err != nil {
		t.Fatalf("Exec returned error %v for a payload exit; that is the user-code case", err)
	}
	if rep.ExitCode != 7 {
		t.Fatalf("exit = %d, want 7", rep.ExitCode)
	}
	if rep.Failure == nil || rep.Failure.Class != sandbox.ClassUserCode {
		t.Fatalf("failure = %+v, want ClassUserCode", rep.Failure)
	}
}

// A box the engine no longer lists is a lost environment, not a broken provider. The
// distinction decides whether the caller retries the same sandbox or rebuilds it.
func TestExecOnAVanishedBoxReportsLost(t *testing.T) {
	f := &fakeRunner{replies: map[string]Outcome{
		"create": {Stdout: "b0x1d\n"},
		"start":  {},
		"list":   {Stdout: "│ ID │ IMAGE │ STATUS │\n"},
		"exec":   {Stdout: "should not run\n"},
	}}
	p := newTestProvider(t, f)
	env, err := p.NewEnvironment(context.Background(), sandbox.EnvSpec{Image: "alpine:latest"})
	if err != nil {
		t.Fatalf("NewEnvironment: %v", err)
	}
	rep, err := env.Exec(context.Background(), sandbox.RunSpec{RunID: "r1", Request: sandbox.Command("true")}, nil)
	var le *sandbox.LostError
	if !errors.As(err, &le) {
		t.Fatalf("Exec error = %v, want *sandbox.LostError", err)
	}
	if !rep.Lost {
		t.Fatal("report does not say Lost, so the caller would retry the same dead box")
	}
	if contains(f.lastCall(), "exec") {
		t.Fatal("exec was dispatched into a box the engine does not list")
	}
}

// A refused allowlist must stop the run. Running anyway would execute unrestricted while
// the caller believes it is restricted, which is worse than failing.
func TestEgressRefusalStopsTheRun(t *testing.T) {
	f := &fakeRunner{replies: map[string]Outcome{
		"create":  {Stdout: "b0x1d\n"},
		"start":   {},
		"list":    {Stdout: "│ b0x1d │ alpine:latest │ Running │\n"},
		"network": {Exit: 1, Stderr: "unknown flag\n"},
		"exec":    {Stdout: "leaked\n"},
	}}
	p := newTestProvider(t, f)
	env, err := p.NewEnvironment(context.Background(), sandbox.EnvSpec{Image: "alpine:latest"})
	if err != nil {
		t.Fatalf("NewEnvironment: %v", err)
	}
	req := sandbox.Command("curl", "http://example.com")
	req.Egress = sandbox.Egress{Mode: sandbox.EgressAllowlist, Allow: []string{"example.com"}}
	rep, err := env.Exec(context.Background(), sandbox.RunSpec{RunID: "r1", Request: req}, nil)
	if err == nil {
		t.Fatalf("Exec ran with an unapplied allowlist: report %+v", rep)
	}
	if contains(f.lastCall(), "exec") {
		t.Fatalf("exec ran after the allowlist was refused: %v", f.lastCall())
	}
	if rep.ExitCode != -1 || rep.Failure == nil {
		t.Fatalf("report = exit %d failure %+v, want -1 with a failure", rep.ExitCode, rep.Failure)
	}
}

func TestStreamingCountsTheBytesItDelivered(t *testing.T) {
	f := &fakeRunner{replies: map[string]Outcome{
		"create": {Stdout: "b0x1d\n"},
		"start":  {},
		"list":   {Stdout: "│ b0x1d │ alpine:latest │ Running │\n"},
		"exec":   {Stdout: "chunk-1\nchunk-2\n"},
	}}
	p := newTestProvider(t, f)
	env, err := p.NewEnvironment(context.Background(), sandbox.EnvSpec{Image: "alpine:latest"})
	if err != nil {
		t.Fatalf("NewEnvironment: %v", err)
	}
	sink := &recordingSink{}
	rep, err := env.Exec(context.Background(), sandbox.RunSpec{RunID: "r1", Streaming: true, Request: sandbox.Shell("echo hi")}, sink)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if len(sink.chunks) == 0 {
		t.Fatal("the provider declared CapStreaming but delivered no chunks to the sink")
	}
	if rep.OutputSent == 0 {
		t.Fatal("OutputSent is zero, so \"the caller received output\" is unproven")
	}
	if sink.chunks[0].RunID != "r1" || sink.chunks[0].Sequence == 0 {
		t.Fatalf("chunk %+v is missing run id or sequence", sink.chunks[0])
	}
}

type recordingSink struct{ chunks []sandbox.OutputChunk }

func (s *recordingSink) Report(c sandbox.OutputChunk) error {
	s.chunks = append(s.chunks, c)
	return nil
}

// Prune refusing is the answer, not a gap. Without labels the provider cannot prove
// which boxes are ours, and deleting on a guess destroys somebody else's work.
func TestPruneRefusesAndSaysWhy(t *testing.T) {
	f := &fakeRunner{replies: map[string]Outcome{"rm": {}}}
	p := newTestProvider(t, f)
	removed, err := p.Prune(context.Background())
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if len(removed) != 0 {
		t.Fatalf("Prune deleted %v without ownership evidence", removed)
	}
	if len(f.calls) != 0 {
		t.Fatalf("Prune issued %v; a refusal must not touch the engine", f.calls)
	}
	skipped := p.SkippedPrune()
	if len(skipped) == 0 || !strings.Contains(skipped[0], "no ownership label") {
		t.Fatalf("SkippedPrune = %v, want the ownership reason; a silent zero reads as \"nothing to delete\"", skipped)
	}
}

func TestImageListedAcceptsTheWaysAnImageIsWritten(t *testing.T) {
	table := "┌────────────┬────────┬──────┐\n│ NAME       │ TAG    │ SIZE │\n│ alpine     │ latest │ 3MB  │\n└────────────┴────────┴──────┘\n"
	for _, ref := range []string{"alpine", "alpine:latest", "library/alpine:latest", "docker.io/library/alpine:latest"} {
		if !imageListed(table, ref) {
			t.Fatalf("%q was not found in the image table", ref)
		}
	}
	for _, ref := range []string{"alpine:3.20", "ubuntu:latest"} {
		if imageListed(table, ref) {
			t.Fatalf("%q was reported present", ref)
		}
	}
}

// An unknown engine attribute must be refused, not ignored: otherwise a typo reads as
// configuration that took effect while the provider silently used a default.
func TestUnknownEngineAttributesAreRefused(t *testing.T) {
	_, err := NewProvider(sandbox.ProviderConfig{Image: "alpine", Raw: map[string]any{"registries": "x"}})
	if err == nil {
		t.Fatal("a misspelled attribute was accepted")
	}
	if !strings.Contains(err.Error(), "registries") || !strings.Contains(err.Error(), "registry") {
		t.Fatalf("error %q does not name both the typo and the known keys", err)
	}
}

func TestBoxNameStaysWithinTheEnginesAlphabet(t *testing.T) {
	got := boxName("sess/with spaces:and:colons")
	if strings.ContainsAny(got, "/ :") {
		t.Fatalf("box name %q contains characters the engine rejects", got)
	}
	if !strings.HasPrefix(got, "hno-") {
		t.Fatalf("box name %q is not recognisable as ours in `boxlite list`", got)
	}
	if long := boxName(strings.Repeat("x", 200)); len(long) > 52 {
		t.Fatalf("box name length %d exceeds the engine limit", len(long))
	}
	// The runtime's own boundary sessions start with hno-; prefixing again produced
	// boxes named hno-hno-boundary-<pid>-0 in `boxlite list`.
	if got := boxName("hno-boundary-647-0"); got != "hno-boundary-647-0" {
		t.Fatalf("boxName double-prefixed to %q", got)
	}
}

func contains(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// The three defects the real gate found, each pinned so it cannot come back silently.

// 1. Without --interactive the guest's `sh -s` never sees EOF: the exec hangs and holds
// the engine's home-directory lock until the process is killed. Measured before the fix.
func TestExecPassesInteractiveWhenItHasAPayload(t *testing.T) {
	f := &fakeRunner{replies: map[string]Outcome{
		"create": {Stdout: "b0x1d\n"},
		"start":  {},
		"list":   {Stdout: "│ b0x1d │ alpine:latest │ Running │\n"},
		"exec":   {Stdout: "ok\n"},
	}}
	p := newTestProvider(t, f)
	env, err := p.NewEnvironment(context.Background(), sandbox.EnvSpec{Image: "alpine:latest"})
	if err != nil {
		t.Fatalf("NewEnvironment: %v", err)
	}
	if _, err := env.Exec(context.Background(), sandbox.RunSpec{RunID: "r", Request: sandbox.Shell("echo hi")}, nil); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !contains(f.lastCall(), "--interactive") {
		t.Fatalf("exec args %v omit --interactive; a payload on stdin would hang the guest", f.lastCall())
	}
	// An argv-only run has nothing on stdin, so it must not demand interactive stdin: that
	// would make every command wait on a terminal it does not need.
	if _, err := env.Exec(context.Background(), sandbox.RunSpec{RunID: "r2", Request: sandbox.Command("true")}, nil); err != nil {
		t.Fatalf("Exec argv: %v", err)
	}
	if contains(f.lastCall(), "--interactive") {
		t.Fatalf("argv-only exec args %v ask for interactive stdin; it would wait on a terminal", f.lastCall())
	}
}

// 2. `boxlite rm` refuses a running box without --force and the box survives the call,
// so a close that looks successful leaks a VM.
func TestCloseForcesRemoval(t *testing.T) {
	f := &fakeRunner{replies: map[string]Outcome{
		"create": {Stdout: "b0x1d\n"},
		"start":  {},
		"rm":     {},
	}}
	p := newTestProvider(t, f)
	env, err := p.NewEnvironment(context.Background(), sandbox.EnvSpec{Image: "alpine:latest"})
	if err != nil {
		t.Fatalf("NewEnvironment: %v", err)
	}
	if err := env.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	last := f.lastCall()
	if len(last) < 2 || last[0] != "rm" || !contains(last, "--force") {
		t.Fatalf("rm call = %v, want --force so a running box is actually removed", last)
	}
}

// 3. --registry is a pull-time flag that does not persist into the box record, so a
// mirror-configured provider must carry the registry in the image reference or `start`
// pulls from the default registry and fails.
func TestCreateCarriesTheRegistryIntoTheImageReference(t *testing.T) {
	f := &fakeRunner{replies: map[string]Outcome{
		"create": {Stdout: "b0x1d\n"},
		"start":  {},
	}}
	p, err := New(Config{Image: "alpine:latest", Runner: f, Registry: []string{"mirror.example"}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := p.NewEnvironment(context.Background(), sandbox.EnvSpec{Image: "alpine:latest"}); err != nil {
		t.Fatalf("NewEnvironment: %v", err)
	}
	if !contains(f.calls[0], "mirror.example/alpine:latest") {
		t.Fatalf("create args %v do not name the mirror in the image reference; start would pull from the default registry", f.calls[0])
	}
	// An already-qualified reference is left alone: doubling the host would ask the
	// mirror for an image called mirror.example/....
	if got := p.qualifyImage("quay.io/org/img:1"); got != "quay.io/org/img:1" {
		t.Fatalf("qualifyImage rewrote a qualified reference to %q", got)
	}
}
