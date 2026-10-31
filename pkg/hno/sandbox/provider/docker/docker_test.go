package docker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rexleimo/agno-go/pkg/hno/sandbox"
)

// fakeRunner records invocations and answers from a script. It exists so the
// provider's contract is testable without a daemon: the alternative is either a
// test that cannot run in CI or a test that silently skips.
type fakeRunner struct {
	mu   sync.Mutex
	seen []Command
	// fs models the container filesystem for the exec shapes this provider uses,
	// so a write/read round trip tests the provider's path handling rather than
	// the fake's default answer.
	fs map[string][]byte
	// respond maps a command to a canned outcome.
	respond func(Command) (Outcome, bool)
	// failFor injects an infrastructure failure: the CLI could not run at all,
	// which is a different thing from a payload exiting nonzero.
	failFor func(Command) error
}

func (f *fakeRunner) Run(_ context.Context, cmd Command) (Outcome, error) {
	f.mu.Lock()
	f.seen = append(f.seen, cmd)
	if f.fs == nil {
		f.fs = map[string][]byte{}
	}
	f.mu.Unlock()
	if out, ok := f.simulate(cmd); ok {
		return streamOut(cmd, out), nil
	}
	if f.failFor != nil {
		if err := f.failFor(cmd); err != nil {
			return Outcome{}, err
		}
	}
	if f.respond != nil {
		if out, ok := f.respond(cmd); ok {
			return streamOut(cmd, out), nil
		}
	}
	return streamOut(cmd, defaultResponse(cmd)), nil
}

// simulate answers the two exec shapes the file service uses: a write-through
// stdin pipe and a plain cat read. Everything else falls through to the script.
func (f *fakeRunner) simulate(cmd Command) (Outcome, bool) {
	if len(cmd.Args) == 0 || cmd.Args[0] != "exec" {
		return Outcome{}, false
	}
	joined := strings.Join(cmd.Args, " ")
	switch {
	case strings.Contains(joined, "cat >"):
		// Args: exec -i <id> sh -c "cat > '/workspace/path'"
		target := extractRedirectTarget(joined)
		if target == "" {
			return Outcome{}, false
		}
		f.mu.Lock()
		f.fs[target] = append([]byte(nil), cmd.Stdin...)
		f.mu.Unlock()
		return Outcome{}, true
	case lastArg(cmd.Args) != "" && strings.HasPrefix(lastArg(cmd.Args), "/workspace/"):
		key := lastArg(cmd.Args)
		f.mu.Lock()
		data, ok := f.fs[key]
		f.mu.Unlock()
		if ok {
			return Outcome{Stdout: string(data)}, true
		}
		return Outcome{Exit: 1, Stderr: "cat: " + key + ": No such file or directory"}, true
	}
	return Outcome{}, false
}

// extractRedirectTarget pulls the destination out of a `cat > '<path>'` command.
func extractRedirectTarget(joined string) string {
	i := strings.Index(joined, "cat > ")
	if i < 0 {
		return ""
	}
	rest := joined[i+len("cat > "):]
	rest = strings.Trim(rest, "'\"")
	if sp := strings.IndexByte(rest, ' '); sp > 0 {
		rest = rest[:sp]
	}
	return rest
}

func lastArg(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[len(args)-1]
}

// envCreate returns the create call that carries the managed label, which is how
// a test can tell the environment's container apart from the probe's.
func envCreate(t *testing.T, fr *fakeRunner) *Command {
	t.Helper()
	for i, c := range fr.seen {
		joined := strings.Join(c.Args, " ")
		if len(c.Args) > 0 && c.Args[0] == "create" && strings.Contains(joined, LabelSession) && !strings.Contains(joined, LabelProbe) {
			return &fr.seen[i]
		}
	}
	t.Fatal("no environment create recorded")
	return nil
}

func (f *fakeRunner) calls(prefix ...string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.seen {
		if hasArgPrefix(c.Args, prefix) {
			n++
		}
	}
	return n
}

func hasArgPrefix(args, prefix []string) bool {
	if len(args) < len(prefix) {
		return false
	}
	for i := range prefix {
		if args[i] != prefix[i] {
			return false
		}
	}
	return true
}

// defaultResponse answers the docker surface this provider uses in a way that
// keeps a happy-path test readable.
// streamOut replays an outcome's output through Command.OnChunk, so a fake Runner
// honours the streaming channel the way execRunner does. A fake that ignores OnChunk
// makes every streaming assertion test the fake instead of the provider, which is
// how a false CapStreaming survived: the test double and the provider were lying in
// the same direction.
func streamOut(cmd Command, out Outcome) Outcome {
	if cmd.OnChunk != nil {
		if out.Stdout != "" {
			cmd.OnChunk(sandbox.StreamStdout, []byte(out.Stdout))
		}
		if out.Stderr != "" {
			cmd.OnChunk(sandbox.StreamStderr, []byte(out.Stderr))
		}
	}
	out.Streamed = int64(len(out.Stdout) + len(out.Stderr))
	return out
}

func defaultResponse(cmd Command) Outcome {
	joined := strings.Join(cmd.Args, " ")
	switch {
	case strings.HasPrefix(joined, "version"):
		return Outcome{Stdout: "27.0.0"}
	case strings.HasPrefix(joined, "create"):
		return Outcome{Stdout: "container-1\n"}
	case strings.HasPrefix(joined, "start container-1"), strings.HasPrefix(joined, "start "):
		return Outcome{Stdout: "container-1\n"}
	case strings.HasPrefix(joined, "inspect"):
		return Outcome{Stdout: "true"}
	case strings.HasPrefix(joined, "ps"):
		return Outcome{Stdout: ""}
	case strings.HasPrefix(joined, "exec"):
		return Outcome{Stdout: "ok\n"}
	case strings.HasPrefix(joined, "rm"):
		return Outcome{}
	default:
		return Outcome{}
	}
}

func newTestProvider(t *testing.T, fr *fakeRunner) *Provider {
	t.Helper()
	p, err := New(Config{Image: "golang:1.26", Runner: fr, StartupTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

// openHandle wires the provider into the sandbox registry under a unique name so
// tests exercise the real core pipeline rather than a hand-rolled approximation.
func openHandle(t *testing.T, fr *fakeRunner, opts ...sandbox.Option) *sandbox.Sandbox {
	t.Helper()
	name := "docker-fake"
	sandbox.UnregisterProvider(name)
	if err := sandbox.RegisterProvider(name, func(cfg sandbox.ProviderConfig) (sandbox.Provider, error) {
		return newTestProvider(t, fr), nil
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	t.Cleanup(func() { sandbox.UnregisterProvider(name) })

	base := []sandbox.Option{
		sandbox.WithProvider(name),
		sandbox.WithAllowedProviders(name),
		sandbox.WithImage("golang:1.26"),
	}
	sb, err := sandbox.Open(context.Background(), append(base, opts...)...)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = sb.Close() })
	return sb
}

// --- capability honesty ----------------------------------------------------

func TestCapabilitiesMatchWhatIsImplemented(t *testing.T) {
	p := newTestProvider(t, &fakeRunner{})
	caps := p.Capabilities()
	// Anything declared must have a working implementation behind it, and
	// anything not implemented must not be declared. These two lists are the
	// contract a caller reads, so an over-claim here is a production incident.
	for _, declared := range []sandbox.Capability{
		sandbox.CapSession, sandbox.CapFileBridge, sandbox.CapStreaming, sandbox.CapRebuild,
	} {
		if !caps.Has(declared) {
			t.Errorf("provider should declare %v", declared)
		}
	}
	for _, notHere := range []sandbox.Capability{
		sandbox.CapEgressAllowlist, sandbox.CapBackground, sandbox.CapQueryStatus,
		sandbox.CapSnapshot, sandbox.CapClone, sandbox.CapPTY, sandbox.CapPortForward,
		sandbox.CapSecretSubstitution,
	} {
		if caps.Has(notHere) {
			t.Errorf("provider declares %v but does not implement it", notHere)
		}
	}
	if p.Isolation() != sandbox.IsolationContainer {
		t.Fatalf("isolation = %v, want container", p.Isolation())
	}
}

func TestEgressAllowlistIsRefusedNotIgnored(t *testing.T) {
	fr := &fakeRunner{}
	sb := openHandle(t, fr)
	// The design forbids the silent no-op: asking a container provider for an
	// allow-list must fail loudly, because "we tried the best we could with open
	// network" is the outcome this whole redesign is meant to make impossible.
	_, err := sb.Run(context.Background(), sandbox.Request{
		Runtime: "sh",
		Code:    "#!echo x",
		Egress:  sandbox.Egress{Mode: sandbox.EgressAllowlist, Allow: []string{"pypi.org"}},
	})
	var capErr *sandbox.CapabilityError
	if !errors.As(err, &capErr) {
		t.Fatalf("err = %v, want *CapabilityError", err)
	}
	if capErr.Missing != sandbox.CapEgressAllowlist {
		t.Fatalf("missing capability = %v, want egress-allowlist", capErr.Missing)
	}
}

// --- container construction ------------------------------------------------

func TestContainerIsCreatedClosedByDefault(t *testing.T) {
	fr := &fakeRunner{}
	sb := openHandle(t, fr)
	if _, err := sb.Run(context.Background(), sandbox.Request{Runtime: "sh", Code: "#!echo hi"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Pick the environment create (the one carrying the managed label), not the
	// probe's capability create.
	create := envCreate(t, fr)
	joined := strings.Join(create.Args, " ")
	// Both spellings are valid CLI syntax; what matters is that no network is
	// attached, so match the flag and its value rather than the separator.
	if !hasFlag(create.Args, "--network", "none") {
		t.Fatalf("container was created without a closed network:\n%s", joined)
	}
	if !strings.Contains(joined, LabelManaged) {
		t.Fatalf("container is missing the managed label, so orphan sweep cannot find it:\n%s", joined)
	}
	if !strings.Contains(joined, "golang:1.26") {
		t.Fatalf("image not applied:\n%s", joined)
	}
}

func TestPayloadNeverEntersArgv(t *testing.T) {
	fr := &fakeRunner{}
	sb := openHandle(t, fr)
	// Code that begins with a dash must not be parseable as a flag, and shell
	// metacharacters in code must not be re-interpreted by the container CLI.
	tricky := "#!echo hi\n--rf-root / -- $(whoami) `id`"
	if _, err := sb.Run(context.Background(), sandbox.Request{Runtime: "sh", Code: tricky}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var exec *Command
	for i, c := range fr.seen {
		if len(c.Args) > 0 && c.Args[0] == "exec" {
			exec = &fr.seen[i]
			break
		}
	}
	if exec == nil {
		t.Fatal("no exec invocation recorded")
	}
	for _, a := range exec.Args {
		if strings.Contains(a, "whoami") || strings.Contains(a, "--rf-root") {
			t.Fatalf("payload leaked into argv: %v", exec.Args)
		}
	}
	if !strings.Contains(string(exec.Stdin), "whoami") {
		t.Fatalf("payload should travel on stdin, got %q", exec.Stdin)
	}
}

// --- failure semantics -----------------------------------------------------

func TestNonzeroExitIsReportNotError(t *testing.T) {
	fr := &fakeRunner{respond: func(c Command) (Outcome, bool) {
		if len(c.Args) > 0 && c.Args[0] == "exec" {
			return Outcome{Stderr: "boom", Exit: 2}, true
		}
		return defaultResponse(c), true
	}}
	sb := openHandle(t, fr)
	rep, err := sb.Run(context.Background(), sandbox.Request{Runtime: "sh", Code: "#!exit 2"})
	if err != nil {
		t.Fatalf("a failing payload must not surface as a provider error: %v", err)
	}
	if rep.ExitCode != 2 {
		t.Fatalf("exit = %d, want 2", rep.ExitCode)
	}
	if rep.Failure == nil || rep.Failure.Class != sandbox.ClassUserCode {
		t.Fatalf("want ClassUserCode, got %+v", rep.Failure)
	}
	if rep.Failure.NextAction != sandbox.ActionModelFix {
		t.Fatalf("want the model to fix it, got %v", rep.Failure.NextAction)
	}
}

func TestEngineFailureIsTypedError(t *testing.T) {
	fr := &fakeRunner{failFor: func(c Command) error {
		// The Runner contract keeps "the CLI could not run" separate from "the
		// payload exited nonzero": infrastructure arrives as an error, payload
		// failures arrive as an Outcome.
		if len(c.Args) > 0 && c.Args[0] == "create" {
			return errors.New("docker: daemon not running")
		}
		return nil
	}}
	sb := openHandle(t, fr)
	_, err := sb.Run(context.Background(), sandbox.Request{Runtime: "sh", Code: "#!echo x"})
	if err == nil {
		t.Fatal("an engine failure must come back as an error")
	}
	if f := sandbox.AsFailure(err); f == nil || f.Class != sandbox.ClassTransient {
		t.Fatalf("want transient, got %+v", f)
	}
}

func TestLostContainerIsClassifiedLost(t *testing.T) {
	// Fault injection required by §12.2: the environment disappears mid-session.
	// The answer must be ClassLost with a rebuild path, never "exec exited 1".
	fr := &fakeRunner{respond: func(c Command) (Outcome, bool) {
		joined := strings.Join(c.Args, " ")
		if strings.HasPrefix(joined, "inspect") {
			return Outcome{Exit: 1, Stderr: "Error: No such object: container-1"}, true
		}
		return defaultResponse(c), true
	}}
	sb := openHandle(t, fr)
	sess, err := sb.Session(context.Background(), "s1", sandbox.WithCreateIfMissing(true))
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	_, err = sess.Exec(context.Background(), sandbox.Request{Runtime: "sh", Code: "#!echo x"})
	var lost *sandbox.LostError
	if !errors.As(err, &lost) {
		t.Fatalf("err = %v, want *LostError", err)
	}
	if lost.SessionID != "s1" {
		t.Fatalf("lost error should name the session, got %+v", lost)
	}
}

func TestCloseRemovesTheContainer(t *testing.T) {
	fr := &fakeRunner{}
	sb := openHandle(t, fr)
	sess, err := sb.Session(context.Background(), "s2", sandbox.WithCreateIfMissing(true))
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if err := sess.Release(context.Background()); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if fr.calls("rm") == 0 {
		t.Fatal("Release did not remove the container: this is the leak orphan reclamation was added for")
	}
}

func TestProviderCloseRemovesEverythingItCreated(t *testing.T) {
	fr := &fakeRunner{}
	p := newTestProvider(t, fr)
	ctx := context.Background()
	env, err := p.NewEnvironment(ctx, sandbox.EnvSpec{SessionID: "s3", Image: "golang:1.26"})
	if err != nil {
		t.Fatalf("NewEnvironment: %v", err)
	}
	if env.ID() != "container-1" {
		t.Fatalf("id = %q", env.ID())
	}
	if err := p.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if fr.calls("rm", "-f", "container-1") != 1 {
		t.Fatalf("provider close must remove live containers, saw %d rm calls", fr.calls("rm"))
	}
}

// --- orphan reclamation ----------------------------------------------------

func TestSweepFindsEngineSideOrphans(t *testing.T) {
	fr := &fakeRunner{respond: func(c Command) (Outcome, bool) {
		if len(c.Args) > 0 && c.Args[0] == "ps" {
			// Two containers exist in the engine; this process knows neither.
			return Outcome{Stdout: "aaa\nbbb\n"}, true
		}
		return defaultResponse(c), true
	}}
	p := newTestProvider(t, fr)
	s := sweeper{p: p}
	orphans, err := s.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if strings.Join(orphans, ",") != "aaa,bbb" {
		t.Fatalf("orphans = %v, want the two engine containers", orphans)
	}
	removed, err := s.Prune(context.Background())
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if len(removed) != 2 {
		t.Fatalf("removed = %v", removed)
	}
	if fr.calls("rm", "-f", "aaa") != 1 {
		t.Fatal("Prune must actually delete")
	}
}

func TestSweepIgnoresLiveContainers(t *testing.T) {
	fr := &fakeRunner{respond: func(c Command) (Outcome, bool) {
		if len(c.Args) > 0 && c.Args[0] == "ps" {
			return Outcome{Stdout: "container-1\n"}, true
		}
		return defaultResponse(c), true
	}}
	p := newTestProvider(t, fr)
	if _, err := p.NewEnvironment(context.Background(), sandbox.EnvSpec{SessionID: "s4", Image: "golang:1.26"}); err != nil {
		t.Fatalf("NewEnvironment: %v", err)
	}
	orphans, err := sweeper{p: p}.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(orphans) != 0 {
		t.Fatalf("a container this process is holding must not be reported as an orphan: %v", orphans)
	}
}

// --- file bridge -----------------------------------------------------------

func TestFileServiceRoundTripInsideWorkspace(t *testing.T) {
	fr := &fakeRunner{}
	sb := openHandle(t, fr)
	sess, err := sb.Session(context.Background(), "files", sandbox.WithCreateIfMissing(true))
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	ctx := context.Background()
	fs, err := sess.Files()
	if err != nil {
		t.Fatalf("Files: %v", err)
	}
	if err := fs.Write(ctx, "src/main.go", []byte("package main\n"), 0o644); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := fs.Read(ctx, "src/main.go")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if string(got) != "package main\n" {
		t.Fatalf("round trip = %q", got)
	}
}

func TestFileServiceRefursesEscapes(t *testing.T) {
	fs := &fileService{env: &Environment{id: "c", provider: newTestProvider(t, &fakeRunner{})}}
	ctx := context.Background()
	for _, p := range []string{"/etc/passwd", "../escape", "a/../../b", "dir\\sub", ""} {
		if err := fs.Write(ctx, p, []byte("x"), 0); err == nil {
			t.Errorf("Write(%q) should be refused", p)
		}
	}
	// The refusal must be a typed path error, so a caller can tell "bad path"
	// from "engine broken".
	err := fs.Write(ctx, "/etc/passwd", []byte("x"), 0)
	var pe *sandbox.PathError
	if !errors.As(err, &pe) {
		t.Fatalf("want *PathError, got %T %v", err, err)
	}
}

func TestRemoveRefusesWorkspaceRoot(t *testing.T) {
	fs := &fileService{env: &Environment{id: "c", provider: newTestProvider(t, &fakeRunner{})}}
	if err := fs.Remove(context.Background(), "."); err == nil {
		t.Fatal("removing the workspace root must be refused")
	}
}

func TestWriteHandleCommitsOnClose(t *testing.T) {
	fr := &fakeRunner{}
	fs := &fileService{env: &Environment{id: "c", provider: newTestProvider(t, fr)}}
	h, err := fs.Open(context.Background(), "out.json", sandbox.OpenOptions{Write: true})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := h.Write([]byte("{}")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := h.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if fr.calls("exec", "-i", "c", "sh", "-c") == 0 {
		t.Fatal("a buffered write handle must commit its content on Close")
	}
}

// --- rebuild + spec replay (§17 gate) --------------------------------------

func TestReplayRunsOnlyUncachedSteps(t *testing.T) {
	fr := &fakeRunner{}
	p := newTestProvider(t, fr)
	spec := sandbox.SessionSpec{Base: "golang:1.26"}
	_, err := p.NewEnvironment(context.Background(), sandbox.EnvSpec{
		SessionID: "replay",
		Image:     "golang:1.26",
		Spec:      spec,
		ReplayFrom: []sandbox.Step{
			{Kind: sandbox.StepInstall, Tool: "go", Argv: []string{"go", "get", "example.com/x"}},
		},
	})
	if err != nil {
		t.Fatalf("NewEnvironment: %v", err)
	}
	// One create, one start, and exactly the replayed install.
	if fr.calls("exec", "container-1", "go", "get", "example.com/x") != 1 {
		calls := []string{}
		for _, c := range fr.seen {
			calls = append(calls, strings.Join(c.Args, " "))
		}
		t.Fatalf("replay step did not run:\n%s", strings.Join(calls, "\n"))
	}
}

func TestReplayFailureIsTypedStepError(t *testing.T) {
	fr := &fakeRunner{respond: func(c Command) (Outcome, bool) {
		if len(c.Args) > 0 && c.Args[0] == "exec" {
			return Outcome{Exit: 1, Stderr: "dial tcp: no network"}, true
		}
		return defaultResponse(c), true
	}}
	p := newTestProvider(t, fr)
	_, err := p.NewEnvironment(context.Background(), sandbox.EnvSpec{
		SessionID: "replay-fail",
		Image:     "golang:1.26",
		ReplayFrom: []sandbox.Step{
			{Kind: sandbox.StepInstall, Tool: "go", Argv: []string{"go", "get", "x"}},
		},
	})
	var stepErr *sandbox.StepReplayError
	if !errors.As(err, &stepErr) {
		t.Fatalf("err = %v, want *StepReplayError", err)
	}
	// A container left behind after a failed replay is the leak that turns one
	// bad session into a full disk.
	if fr.calls("rm", "-f") == 0 {
		t.Fatal("a failed environment must be removed, not abandoned")
	}
}

// --- bind mount policy -----------------------------------------------------

func TestMountShareNeedsExplicitEnablement(t *testing.T) {
	fr := &fakeRunner{}
	p := newTestProvider(t, fr)
	_, err := p.NewEnvironment(context.Background(), sandbox.EnvSpec{
		SessionID: "share",
		Image:     "golang:1.26",
		MountMode: sandbox.MountShare,
		Workspace: sandbox.FromDir("/srv/app"),
	})
	var denied *sandbox.DeniedError
	if !errors.As(err, &denied) {
		t.Fatalf("MountShare without enablement must be refused, got %v", err)
	}
	if !strings.Contains(denied.Reason, "CopySnapshot") {
		t.Fatalf("refusal should name the safe alternative, got %q", denied.Reason)
	}
}

func TestMountShareWorksWhenEnabled(t *testing.T) {
	fr := &fakeRunner{}
	p, err := New(Config{Image: "golang:1.26", Runner: fr, AllowBindMounts: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := p.NewEnvironment(context.Background(), sandbox.EnvSpec{
		SessionID: "share2",
		Image:     "golang:1.26",
		MountMode: sandbox.MountShare,
		Workspace: sandbox.FromDir("/srv/app"),
	}); err != nil {
		t.Fatalf("NewEnvironment: %v", err)
	}
	joined := strings.Join(envCreate(t, fr).Args, " ")
	if !strings.Contains(joined, "--volume /srv/app:/workspace") {
		t.Fatalf("bind mount missing:\n%s", joined)
	}
}

// --- probe -----------------------------------------------------------------

func TestProbeReportsDaemonDownWithFixes(t *testing.T) {
	fr := &fakeRunner{respond: func(c Command) (Outcome, bool) {
		if len(c.Args) > 0 && c.Args[0] == "version" {
			return Outcome{Exit: 1, Stderr: "Cannot connect to the Docker daemon"}, true
		}
		return defaultResponse(c), true
	}}
	p := newTestProvider(t, fr)
	err := p.Probe(context.Background())
	var un *sandbox.UnavailableError
	if !errors.As(err, &un) {
		t.Fatalf("err = %v, want *UnavailableError", err)
	}
	if len(un.Remediation()) == 0 {
		t.Fatal("a daemon-down probe must carry a fix, not just a failure")
	}
}

func TestConfigRequiresImage(t *testing.T) {
	if _, err := New(Config{Runner: &fakeRunner{}}); err == nil {
		t.Fatal("an empty image is not a usable provider config")
	}
}

func TestStreamingSinkReceivesOutput(t *testing.T) {
	fr := &fakeRunner{respond: func(c Command) (Outcome, bool) {
		if len(c.Args) > 0 && c.Args[0] == "exec" {
			return Outcome{Stdout: "chunk-a\nchunk-b\n"}, true
		}
		return defaultResponse(c), true
	}}
	sb := openHandle(t, fr)
	var got []byte
	rep, err := sb.Run(context.Background(),
		sandbox.Request{Runtime: "sh", Code: "#!echo x"},
		sandbox.WithRunSink("test", sandbox.SinkFunc(func(_ context.Context, ch sandbox.OutputChunk) error {
			got = append(got, ch.Bytes...)
			return nil
		})),
	)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(string(got), "chunk-a") {
		t.Fatalf("sink received %q, report has %q", got, rep.Stdout)
	}
}

func TestOctalModeRendering(t *testing.T) {
	for mode, want := range map[uint32]string{0o644: "644", 0o755: "755", 0o600: "600", 0: "644", 0o4755: "4755"} {
		if got := octal(mode); got != want {
			t.Errorf("octal(%o) = %q, want %q", mode, got, want)
		}
	}
}

func TestRuntimeEntrypointRejectsUnknown(t *testing.T) {
	if _, err := runtimeEntrypoint("brainfuck"); err == nil {
		t.Fatal("an unknown runtime must be refused with a message naming the known ones")
	}
	argv, err := runtimeEntrypoint("python")
	if err != nil || len(argv) == 0 || argv[0] != "python3" {
		t.Fatalf("python entrypoint = %v %v", argv, err)
	}
}

func TestEngineErrorIsDistinctFromPayloadError(t *testing.T) {
	// Both are "nonzero exit". One means the sandbox broke; the other means the
	// model's code failed. Collapsing them is the current run path's central bug,
	// so the distinction needs a test and not a comment.
	pe := newEngineError("daemon died")
	var ee *engineError
	if !errors.As(fmt.Errorf("wrapped: %w", pe), &ee) {
		t.Fatal("engine errors must be identifiable")
	}
	if pe.Error() == "" {
		t.Fatal("empty engine error text is useless in a report")
	}
	if newEngineError("  ").Error() == "" {
		t.Fatal("a blank daemon message still needs to say something")
	}
}

// TestProbeDetectsShimThatCannotCreate guards the false-positive found on a real
// machine: a docker-compatible shim answered `version` happily and then refused
// every create. A probe that trusts the handshake reports a healthy provider that
// cannot run anything, so the caller stops investigating.
func TestProbeDetectsShimThatCannotCreate(t *testing.T) {
	fr := &fakeRunner{respond: func(c Command) (Outcome, bool) {
		joined := strings.Join(c.Args, " ")
		switch {
		case strings.HasPrefix(joined, "version"):
			return Outcome{Stdout: "0.9.4"}, true // the handshake succeeds, as it did for real
		case strings.HasPrefix(joined, "create"):
			return Outcome{
				Exit:   1,
				Stderr: "Error: Error response from daemon: container create is not supported by Apple Containerization runtime. Use 'mocker run' instead.",
			}, true
		}
		return defaultResponse(c), true
	}}
	p := newTestProvider(t, fr)

	err := p.Probe(context.Background())
	if err == nil {
		t.Fatal("a provider that cannot create anything must not report itself available")
	}
	var un *sandbox.UnavailableError
	if !errors.As(err, &un) {
		t.Fatalf("want *UnavailableError, got %T: %v", err, un)
	}
	if len(un.Remediation()) == 0 {
		t.Fatalf("the finding is useless without the next step: %+v", un)
	}
	if !strings.Contains(strings.Join(un.Remediation(), " "), "shim") {
		t.Fatalf("remediation should name the shim situation, got %v", un.Remediation())
	}
}

func TestProbeAsksCreateWithoutTouchingNetwork(t *testing.T) {
	fr := &fakeRunner{}
	p := newTestProvider(t, fr)
	if err := p.Probe(context.Background()); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	var create *Command
	for i, c := range fr.seen {
		if len(c.Args) > 0 && c.Args[0] == "create" {
			create = &fr.seen[i]
			break
		}
	}
	if create == nil {
		t.Fatal("Probe must ask the create-capability question")
	}
	if !hasArgPrefix(create.Args, []string{"create", "--pull=never"}) {
		t.Fatalf("Probe pulled implicitly: %v", create.Args)
	}
	// Probing must never leave a running container behind.
	if fr.calls("start") != 0 {
		t.Fatal("Probe must not start containers")
	}
}

func TestMissingImageIsNotReportedAsUnsupportedEngine(t *testing.T) {
	// "image not local" and "runtime cannot create" are different findings with
	// different fixes; collapsing them sends the user to reinstall the engine.
	fr := &fakeRunner{respond: func(c Command) (Outcome, bool) {
		if strings.HasPrefix(strings.Join(c.Args, " "), "create") {
			return Outcome{Exit: 1, Stderr: "Error: No such image: golang:1.26"}, true
		}
		return defaultResponse(c), true
	}}
	p := newTestProvider(t, fr)
	if err := p.Probe(context.Background()); err != nil {
		t.Fatalf("a missing image is a usable engine; Probe should not fail: %v", err)
	}
}

func TestRefusesCreateRecognition(t *testing.T) {
	yes := []string{
		"container create is not supported by Apple Containerization runtime",
		"method Unimplemented",
		"unknown shorthand flag: 'q'",
	}
	no := []string{
		"No such image: golang:1.26",
		"error while pulling",
		"daemon is busy",
	}
	for _, m := range yes {
		if !refusesCreate(m) {
			t.Errorf("should recognise %q", m)
		}
	}
	for _, m := range no {
		if refusesCreate(m) {
			t.Errorf("should not treat %q as an engine limitation", m)
		}
	}
}

// hasFlag reports whether args carries `flag` with `value`, in either the
// separated or =-joined form.
func hasFlag(args []string, flag, value string) bool {
	for i, a := range args {
		if a == flag && i+1 < len(args) && args[i+1] == value {
			return true
		}
		if a == flag+"="+value {
			return true
		}
	}
	return false
}

// TestAppleDialectUsesItsOwnSurfaces locks the differences that would otherwise
// fail a healthy engine: Apple's CLI has no `version` subcommand, no server-side
// label filter, and no `inspect --format`.
func TestAppleDialectUsesItsOwnSurfaces(t *testing.T) {
	fr := &fakeRunner{respond: func(c Command) (Outcome, bool) {
		joined := strings.Join(c.Args, " ")
		switch {
		case joined == "--version":
			return Outcome{Stdout: "container 1.5.0"}, true
		case strings.HasPrefix(joined, "list"):
			// Schema taken from a real `container list --all --format json`, where
			// labels sit under configuration.labels and there is no top-level name.
			return Outcome{Stdout: `[{"id":"abc","configuration":{"labels":{"` + LabelManaged + `":"true"}},"status":{"state":"running"}}]`}, true
		case strings.HasPrefix(joined, "create"):
			return Outcome{Stdout: "hno-x\n"}, true
		case strings.HasPrefix(joined, "start"):
			return Outcome{}, true
		}
		return defaultResponse(c), true
	}}
	cfg := Config{Image: "golang:1.26", Binary: "container", Dialect: DialectApple, Runner: fr}
	p, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := p.Probe(context.Background()); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	for _, c := range fr.seen {
		if len(c.Args) > 0 && c.Args[0] == "version" {
			t.Fatal("Apple CLI has no `version` subcommand; the probe used the docker shape")
		}
	}

	ids, err := p.dial.ids(context.Background(), fr, "container")
	if err != nil {
		t.Fatalf("ids: %v", err)
	}
	if len(ids) != 1 || ids[0] != "abc" {
		t.Fatalf("label filtering failed: %v", ids)
	}
	run, err := p.dial.running(context.Background(), fr, "container", "abc")
	if err != nil || !run {
		t.Fatalf("running = %v, %v", run, err)
	}
}

func TestCreateArgsRenderResourceCeilings(t *testing.T) {
	d := dockerDialect{}
	args := d.createArgs(createSpec{
		Image:     "img",
		CPUCount:  2,
		MemoryMB:  512,
		PidsLimit: 128,
		Argv:      []string{"sleep", "infinity"},
	})
	if !hasFlag(args, "--cpus", "2") {
		t.Fatalf("cpu ceiling missing: %v", args)
	}
	if !hasFlag(args, "--memory", "512M") {
		t.Fatalf("memory ceiling missing: %v", args)
	}
	if !hasFlag(args, "--pids-limit", "128") {
		t.Fatalf("pids ceiling missing: %v", args)
	}
	// Zero means "engine default", which must render as no flag at all: an
	// explicit 0 would be a limit, and not passing one is a different statement.
	bare := d.createArgs(createSpec{Image: "img", Argv: []string{"true"}})
	for _, a := range bare {
		if strings.HasPrefix(a, "--cpus") || strings.HasPrefix(a, "--memory") || strings.HasPrefix(a, "--pids") {
			t.Fatalf("zero limits must not be sent: %v", bare)
		}
	}
	// Apple's CLI has no --pids-limit, so the apple dialect must not claim it.
	ad := appleDialect{}
	withPids := ad.createArgs(createSpec{Image: "img", PidsLimit: 64, Argv: []string{"true"}})
	for _, a := range withPids {
		if strings.HasPrefix(a, "--pids") {
			t.Fatal("apple dialect emitted a flag the engine does not accept")
		}
	}
}

func TestEnvCreateAppliesProfileLimits(t *testing.T) {
	fr := &fakeRunner{}
	sb := openHandle(t, fr, sandbox.WithLimits(sandbox.Limits{
		CPUCount: 1,
		MemoryMB: 256,
	}))
	if _, err := sb.Run(context.Background(), sandbox.Request{Runtime: "sh", Code: "#!echo hi"}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	args := envCreate(t, fr).Args
	if !hasFlag(args, "--cpus", "1") || !hasFlag(args, "--memory", "256M") {
		t.Fatalf("profile limits never reached the engine: %v", strings.Join(args, " "))
	}
}

// TestBoundedBufferMarksTruncation covers the capture ceiling. An unbounded
// reader turns "the payload printed a lot" into "the host allocated a lot", and
// the profile's output limit would report a ceiling it never enforced.
func TestBoundedBufferMarksTruncation(t *testing.T) {
	b := &boundedBuffer{limit: 16}
	// Writing 40 bytes must keep 16 and report the rest.
	for i := 0; i < 40; i++ {
		if _, err := b.Write([]byte("x")); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	got := b.String()
	if len(got) <= 16 {
		t.Fatalf("truncation marker missing, so the caller cannot tell the difference: %q", got)
	}
	if !strings.Contains(got, "24 more bytes dropped") {
		t.Fatalf("dropped count wrong: %q", got)
	}
	if !strings.HasPrefix(got, strings.Repeat("x", 16)) {
		t.Fatalf("the kept prefix changed: %q", got)
	}
	// Under the limit nothing is marked, because "complete" and "truncated" have
	// to stay distinguishable.
	small := &boundedBuffer{limit: 16}
	_, _ = small.Write([]byte("abc"))
	if small.String() != "abc" {
		t.Fatalf("clean read got marked: %q", small.String())
	}
}

// TestSuccessfulRunKeepsStderr guards the c.Output() habit that discards stderr
// on a zero exit. Tooling that warns on stderr and exits 0 is common, and losing
// it means the report claims silence the payload never produced.
func TestSuccessfulRunKeepsStderr(t *testing.T) {
	fr := &fakeRunner{respond: func(c Command) (Outcome, bool) {
		return Outcome{Stdout: "payload output", Stderr: "a warning from the compiler"}, true
	}}
	p, err := New(Config{Image: "img", Runner: fr})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	res, err := p.runner.Run(context.Background(), Command{Bin: "img", Args: []string{"x"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Stderr == "" {
		t.Fatal("stderr vanished on a successful run")
	}
	if !strings.Contains(res.Stderr, "warning") {
		t.Fatalf("unexpected stderr: %q", res.Stderr)
	}
}

// TestAppleIDsReadNestedLabels pins the schema measured from a real engine. A
// parser written against the obvious top-level keys returns an empty set, which
// is the worst possible failure for an orphan sweep: it looks correct, reports
// "no residue", and leaks every container.
func TestAppleIDsReadNestedLabels(t *testing.T) {
	real := `[{"id":"abc","configuration":{"labels":{"` + LabelManaged +
		`":"true","` + LabelSession + `":"s7"}},"status":{"state":"running"}},` +
		`{"id":"unrelated","configuration":{"labels":{}},"status":{"state":"running"}}]`
	fr := &fakeRunner{respond: func(c Command) (Outcome, bool) {
		if strings.HasPrefix(strings.Join(c.Args, " "), "list") {
			return Outcome{Stdout: real}, true
		}
		return defaultResponse(c), true
	}}
	p := &Provider{d: Config{Image: "img"}, runner: fr, bin: "container", dial: appleDialect{}}
	ids, err := p.dial.ids(context.Background(), fr, "container")
	if err != nil {
		t.Fatalf("ids: %v", err)
	}
	if len(ids) != 1 || ids[0] != "abc" {
		t.Fatalf("sweep would reclaim %v; it must see only the managed container", ids)
	}
	// And an empty list is a legitimate answer, not an error: a machine with no
	// residue must not make Diagnose fail.
	empty := &fakeRunner{respond: func(c Command) (Outcome, bool) {
		if strings.HasPrefix(strings.Join(c.Args, " "), "list") {
			return Outcome{Stdout: "[]"}, true
		}
		return defaultResponse(c), true
	}}
	// Parentheses are required: a composite literal with an empty brace inline in
	// an if statement parses as a block.
	if got, err := (appleDialect{}).ids(context.Background(), empty, "container"); err != nil || len(got) != 0 {
		t.Fatalf("empty list mishandled: %v, %v", got, err)
	}
}

// TestStatProbeIsQuotableForSH catches the defect that made every Stat report
// "file not found" on files that existed: the probe embedded a single-quoted
// string inside a script that is itself passed as one `sh -c` argv, which produced
// "Unterminated quoted string" in the container. Because a nonzero exit is also
// how a missing file is signalled, the failure looked like empty artefacts rather
// than a broken shell command.
func TestStatProbeIsQuotableForSH(t *testing.T) {
	probe := statProbe("/workspace/a file.txt")

	// The real invariant: the script is handed to the container's `sh -c` as one
	// argv, so it has to parse by itself. It does not travel inside a host shell
	// quote, so single quotes are legal — but an unbalanced one is a syntax
	// error, and the container's nonzero exit was being read as "file not found".
	for _, q := range []byte{39, 34} {
		if n := countUnescaped(probe, q); n%2 != 0 {
			t.Fatalf("probe has %d unescaped %c quotes, so sh cannot parse it:\n%s", n, q, probe)
		}
	}
	// A path with a space must survive as one argument rather than split.
	if !strings.Contains(probe, shellQuote("/workspace/a file.txt")) {
		t.Fatalf("target path was not quoted as a unit:\n%s", probe)
	}

	// And parse round-trips the shape the probe promises, including a type
	// description that contains spaces.
	for _, tc := range []struct {
		line   string
		size   int64
		dir    bool
		symbol bool
	}{
		{"f 12 regular file", 12, false, false},
		{"d 0 directory", 0, true, false},
		{"f 7 symbolic link", 7, false, true},
	} {
		fi, err := parseStat("x", tc.line)
		if err != nil {
			t.Fatalf("parseStat(%q): %v", tc.line, err)
		}
		if fi.Size != tc.size || fi.Dir != tc.dir || fi.Symlink != tc.symbol {
			t.Fatalf("parseStat(%q) = %+v", tc.line, fi)
		}
	}
	if _, err := parseStat("x", "garbage"); err == nil {
		t.Fatal("unparseable stat must be an error, not a zero-size file")
	}
}

// countUnescaped counts quote characters that are not backslash-escaped.
func countUnescaped(script string, q byte) int {
	var n int
	for i := 0; i < len(script); i++ {
		if script[i] == q && (i == 0 || script[i-1] != '\\') {
			n++
		}
	}
	return n
}

// TestProviderImplementsReclaimer pins the wiring the core asserts on. A provider
// that only reaches its sweeper through a helper type fails the assertion silently:
// Reclaimer() reports false and the caller concludes the engine cannot sweep, when
// the real problem was a missing method on the provider itself.
func TestProviderImplementsReclaimer(t *testing.T) {
	probe := &recRunner{}
	p, err := New(Config{Image: "golang:1.26", Runner: probe})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	r, ok := interface{}(p).(sandbox.Reclaimer)
	if !ok {
		t.Fatal("*docker.Provider must implement sandbox.Reclaimer")
	}
	if _, err := r.Sweep(context.Background()); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if _, err := r.Prune(context.Background()); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	// Sweep must be an engine query, not a local bookkeeping read: the whole point
	// is finding residue this process never created. The exact verb is dialect
	// owned (docker asks `ps -a`, Apple asks `list --all`), so the invariant is
	// "the engine was asked to enumerate", not one fixed argv.
	var sawInventory bool
	for _, c := range probe.commands() {
		if len(c.Args) > 0 && (c.Args[0] == "ps" || c.Args[0] == "list") {
			sawInventory = true
		}
	}
	if !sawInventory {
		t.Fatalf("Sweep did not ask the engine for its inventory; commands: %+v", probe.commands())
	}
}

// recRunner records every command shape and answers with empty successes.
type recRunner struct {
	mu   sync.Mutex
	seen []Command
}

func (r *recRunner) Run(_ context.Context, c Command) (Outcome, error) {
	r.mu.Lock()
	r.seen = append(r.seen, c)
	r.mu.Unlock()
	return streamOut(c, Outcome{}), nil
}

func (r *recRunner) commands() []Command {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Command, len(r.seen))
	copy(out, r.seen)
	return out
}

// TestStderrStreamsToo guards the path that was 100% silent before: the provider
// emitted one stdout chunk after exit and nothing for stderr, so a build whose only
// output is compiler errors streamed nothing at all while CapStreaming stayed
// declared. A caller watching a build saw a dead pane.
func TestStderrStreamsToo(t *testing.T) {
	fr := &fakeRunner{respond: func(c Command) (Outcome, bool) {
		if len(c.Args) > 0 && c.Args[0] == "exec" {
			return Outcome{Stderr: "undefined: foo\n", Exit: 1}, true
		}
		return defaultResponse(c), true
	}}
	sb := openHandle(t, fr)
	var got map[sandbox.Stream]string
	got = map[sandbox.Stream]string{}
	rep, err := sb.Run(context.Background(),
		sandbox.Request{Runtime: "sh", Code: "#!fail 1 undefined: foo"},
		sandbox.WithRunSink("watch", sandbox.SinkFunc(func(_ context.Context, ch sandbox.OutputChunk) error {
			got[ch.Stream] += string(ch.Bytes)
			return nil
		})))
	if err != nil {
		t.Fatalf("a nonzero exit is data, not an error: %v", err)
	}
	if !strings.Contains(got[sandbox.StreamStderr], "undefined: foo") {
		t.Fatalf("stderr never streamed; sink saw %v, report stderr %q, exit %d", got, rep.Stderr, rep.ExitCode)
	}
}

// TestChunksArriveBeforeExit is the falsifiable form of "incrementally instead of
// only at exit". It is deliberately ordering-deterministic rather than time-based:
// the fake refuses to finish the command until a chunk has been observed, so if the
// provider could only deliver at exit the run would deadlock and the test would fail
// on its own timeout instead of passing on a flaky flag.
func TestChunksArriveBeforeExit(t *testing.T) {
	chunkSeen := make(chan string, 4)
	release := make(chan struct{})
	fr := &fakeRunner{respond: func(c Command) (Outcome, bool) {
		if len(c.Args) > 0 && c.Args[0] == "exec" {
			// Emit from inside the run, exactly where execRunner's tap fires.
			if c.OnChunk != nil {
				c.OnChunk(sandbox.StreamStdout, []byte("mid-run\n"))
			}
			<-release // the command is still executing here by construction
			return Outcome{Stdout: "mid-run\n"}, true
		}
		return defaultResponse(c), true
	}}
	sb := openHandle(t, fr)
	var mu sync.Mutex
	var order []string
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := sb.Run(context.Background(),
			sandbox.Request{Runtime: "sh", Code: "#!echo x"},
			sandbox.WithRunSink("watch", sandbox.SinkFunc(func(_ context.Context, ch sandbox.OutputChunk) error {
				mu.Lock()
				order = append(order, string(ch.Bytes))
				mu.Unlock()
				select {
				case chunkSeen <- string(ch.Bytes):
				default:
				}
				return nil
			}))); err != nil {
			t.Errorf("Run: %v", err)
		}
	}()

	select {
	case got := <-chunkSeen:
		if got != "mid-run\n" {
			t.Fatalf("chunk = %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the sink never received a chunk while the command was still executing")
	}
	// Reaching here means a chunk arrived while respond() was still blocked inside
	// the run. Nothing about the run has returned yet, so delivery was incremental.
	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after the command was released")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(order) == 0 || order[0] != "mid-run\n" {
		t.Fatalf("chunk order = %v", order)
	}
}

// TestSinkFaultIsNotSwallowed pins that a Sink bound with OverflowFault (available
// through WithOverflow) reaches the caller. The provider used to ignore the
// reporter's return value entirely, which made that policy unreachable in practice:
// a consumer that rejected chunks looked identical to one that received them.
func TestSinkFaultIsNotSwallowed(t *testing.T) {
	fr := &fakeRunner{respond: func(c Command) (Outcome, bool) {
		if len(c.Args) > 0 && c.Args[0] == "exec" {
			if c.OnChunk != nil {
				c.OnChunk(sandbox.StreamStdout, []byte("noisy\n"))
			}
			return Outcome{Stdout: "noisy\n", Exit: 0}, true
		}
		return defaultResponse(c), true
	}}
	sb := openHandle(t, fr)
	rep, err := sb.Run(context.Background(),
		sandbox.Request{Runtime: "sh", Code: "#!echo x"},
		sandbox.WithRunSink("strict", sandbox.SinkFunc(func(context.Context, sandbox.OutputChunk) error {
			return errors.New("consumer rejected the chunk")
		}), sandbox.WithOverflow(sandbox.OverflowFault)))
	if err == nil {
		t.Fatal("OverflowFault must reach the caller")
	}
	var hf *sandbox.HookFaultError
	if !errors.As(err, &hf) {
		t.Fatalf("want *HookFaultError, got %T: %v", err, err)
	}
	// The payload's own output survives the consumer's problem.
	if rep != nil && !strings.Contains(rep.Stdout, "noisy") {
		t.Fatalf("captured output must survive a sink fault: %q", rep.Stdout)
	}
}

// --- sweep ownership safety --------------------------------------------

// sweepRunner answers the dialect's inventory query with a mix of running and
// stopped managed containers, none of which this process holds.
type sweepRunner struct {
	items []listedContainer
	// running marks the ids the docker-format inventory should report as running.
	states  map[string]string
	removed []string
}

func (r *sweepRunner) Run(_ context.Context, c Command) (Outcome, error) {
	joined := strings.Join(c.Args, " ")
	switch {
	case strings.HasPrefix(joined, "ps"), strings.HasPrefix(joined, "list"):
		var lines []string
		for _, it := range r.items {
			state := r.states[it.ID]
			if state == "" {
				state = "exited"
			}
			lines = append(lines, fmt.Sprintf("%s|%s|%s|%s", it.ID, it.Session, state, it.Owner))
		}
		return Outcome{Stdout: strings.Join(lines, "\n")}, nil
	case strings.HasPrefix(joined, "rm"):
		parts := strings.Fields(joined)
		if len(parts) >= 3 {
			r.removed = append(r.removed, parts[2])
		}
		return Outcome{}, nil
	default:
		return defaultResponse(c), nil
	}
}

func newSweepProvider(t *testing.T, fr *sweepRunner, pruneRunning bool) *Provider {
	t.Helper()
	p, err := New(Config{
		Image: "golang:1.26", Binary: "docker", Runner: fr,
		PruneRunning: pruneRunning,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

// TestPruneNeverDeletesAForeignRunningContainer is the unit-level guard for the
// two-process data loss measured on a real engine: a container another process is
// running must be reported, not reclaimed.
func TestPruneNeverDeletesAForeignRunningContainer(t *testing.T) {
	ctx := context.Background()
	fr := &sweepRunner{
		items: []listedContainer{
			{ID: "live-other", Session: "other", Running: true, Owner: "host/999/abc"},
			{ID: "dead-residue", Session: "crashed"},
		},
		states: map[string]string{"live-other": "running", "dead-residue": "exited"},
	}
	p := newSweepProvider(t, fr, false)

	swept, err := sweeper{p: p}.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(swept) != 2 {
		t.Fatalf("Sweep must report both foreign containers, got %v", swept)
	}
	removed, err := sweeper{p: p}.Prune(ctx)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	for _, id := range removed {
		if id == "live-other" {
			t.Fatal("Prune deleted a running container belonging to another process")
		}
	}
	if len(removed) != 1 || removed[0] != "dead-residue" {
		t.Fatalf("only the stopped residue should be reclaimed, got %v", removed)
	}
	// The refusal has to be answerable, or an empty result reads as "nothing left".
	skipped := p.SkippedPrune()
	if len(skipped) != 1 || !strings.Contains(skipped[0], "live-other") {
		t.Fatalf("the refused container must be recorded, got %v", skipped)
	}
	if !strings.Contains(skipped[0], "host/999/abc") {
		t.Fatalf("the refusal should name the owner so an operator can find the process, got %q", skipped[0])
	}
}

// TestPruneRunningIsExplicitOptIn: the destructive behaviour exists for
// single-tenant machines, but only when asked for.
func TestPruneRunningIsExplicitOptIn(t *testing.T) {
	ctx := context.Background()
	fr := &sweepRunner{
		items:  []listedContainer{{ID: "live-other", Session: "other", Running: true}},
		states: map[string]string{"live-other": "running"},
	}
	p := newSweepProvider(t, fr, true)
	removed, err := sweeper{p: p}.Prune(ctx)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if len(removed) != 1 || removed[0] != "live-other" {
		t.Fatalf("with PruneRunning the container should be reclaimed, got %v", removed)
	}
	if len(p.SkippedPrune()) != 0 {
		t.Fatalf("nothing was skipped, so nothing should be recorded: %v", p.SkippedPrune())
	}
}

// TestStartupTimeoutAttrIsHonoured catches the silent no-op where a documented
// engine attribute was read by nothing: the provider used one shared deadline for
// create and start, so a slow create starved start and the failure arrived without a
// reason.
func TestStartupTimeoutAttrIsHonoured(t *testing.T) {
	cfg := sandbox.ProviderConfig{
		Image: "golang:1.26",
		Raw:   map[string]any{"binary": "container", "startup_timeout": "2500ms"},
	}
	p, err := NewProvider(cfg)
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	prov, ok := p.(*Provider)
	if !ok {
		t.Fatalf("NewProvider returned %T", p)
	}
	if got := prov.startupWindow(); got != 2500*time.Millisecond {
		t.Fatalf("startup_timeout was ignored: window = %v", got)
	}
	// An unknown key must be refused rather than quietly dropped.
	if _, err := NewProvider(sandbox.ProviderConfig{Raw: map[string]any{"startupTimeout": "5s"}}); err == nil {
		t.Fatal("a misspelled attribute must not be silently ignored")
	}
	// A malformed duration is an error, not a default.
	if _, err := NewProvider(sandbox.ProviderConfig{Raw: map[string]any{"startup_timeout": "soon"}}); err == nil {
		t.Fatal("an unparsable duration must be an error")
	}
}

// TestEngineFailureNamesTheDeadline proves that a killed CLI call is reported as a
// deadline problem instead of "exited nonzero" with nothing to act on.
func TestEngineFailureNamesTheDeadline(t *testing.T) {
	// Killed by context, no stderr: the exact shape of the flake.
	killed := Outcome{Exit: -1, Canceled: true}
	err := engineFailure(killed, 3*time.Second, context.DeadlineExceeded)
	if !strings.Contains(err.Error(), "deadline") {
		t.Fatalf("a deadline kill must say so: %v", err)
	}
	if !strings.Contains(err.Error(), "3s") {
		t.Fatalf("the message should name the window that expired: %v", err)
	}
	// Engine wrote a real reason: that reason must survive.
	real := Outcome{Exit: 1, Stderr: "Error: no such container: abc"}
	if got := engineFailure(real, time.Minute, nil); !strings.Contains(got.Error(), "no such container") {
		t.Fatalf("engine stderr must be preserved: %v", got)
	}
	// Silent nonzero exit is reported as silence, not as a blank message.
	quiet := engineFailure(Outcome{Exit: 7}, time.Minute, nil)
	if strings.TrimSpace(quiet.Error()) == "" {
		t.Fatal("an empty cause is how an unknown fault reaches a retry layer")
	}
}
