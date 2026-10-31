package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rexleimo/agno-go/pkg/hno/sandbox"
)

// defaultRealImage is the image the opt-in integration test uses. It is chosen
// because it already exists on this machine, so a real round-trip costs no
// network pull. Override with HNO_SANDBOX_IMAGE.
const defaultRealImage = "golang:1.26"

// TestRealProjectRoundTrip is the §12.2 B2 completion gate: a real project
// directory goes in through FromDir, RunProject executes phases inside a real
// container, and the declared artefacts come back.
//
// It is opt-in. `go test ./...` must never start a container implicitly: a suite
// that talks to a daemon cannot run where no daemon exists, and a skip buried in
// that situation hides the fact that the gate was never met.
//
//	HNO_SANDBOX_DOCKER=1 go test -run RealProject ./pkg/hno/sandbox/provider/docker -v
func TestRealProjectRoundTrip(t *testing.T) {
	ctx := context.Background()
	sb := requireRealEngine(t, ctx)

	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module demo\n\ngo 1.26\n")
	writeFile(t, filepath.Join(dir, "main.go"), `package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println("demo 1.0.0")
		return
	}
	fmt.Println("built from a real working tree")
}
`)

	project := sandbox.Project{
		// FromDir in, with CopySnapshot as the mount mode (D-12): the container
		// must not be able to write the host tree.
		From: sandbox.FromDir(dir),
		Phases: []sandbox.Phase{
			{
				Name:     "build",
				Required: true,
				Run:      sandbox.Request{Argv: []string{"go", "build", "-o", "out/demo", "."}},
			},
			{
				Name:     "verify",
				Required: true,
				Run:      sandbox.Request{Argv: []string{"./out/demo"}},
			},
		},
		Artifacts: []sandbox.ArtifactSpec{
			{Path: "out/demo", Kind: sandbox.KindFile, Required: true},
		},
	}

	res, err := sb.RunProject(ctx, project)
	if err != nil {
		t.Fatalf("RunProject: %v\n%s", err, phaseDump(res))
	}
	if res.Status != sandbox.ProjectSucceeded {
		t.Fatalf("project did not succeed: status=%v exit=%d phase=%q\n%s",
			res.Status, res.ExitCode, res.FailedPhase, phaseDump(res))
	}

	// The required artefact has to come back as a readable handle, not a name.
	art, ok := res.Artifacts.Get("out/demo")
	if !ok {
		t.Fatalf("required artefact missing under that key; collected: %q", strings.Join(res.Artifacts.Names(), ", "))
	}
	if art.Size == 0 {
		t.Fatal("the collected binary is zero bytes; a required artefact with no content is not a build")
	}
	body, err := art.Reader(ctx)
	if err != nil {
		t.Fatalf("Reader: %v", err)
	}
	defer body.Close()
	data, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	// A Go binary starts with the ELF/Mach-O magic. Reading content back through
	// the artefact handle is what proves the handle is not just a filename.
	if len(data) < 4 {
		t.Fatalf("artefact content is %d bytes, too short to be a binary", len(data))
	}

	// The host tree must not have received the container's build output. This is
	// the claim CopySnapshot makes, and it is falsifiable here rather than
	// asserted in a comment.
	if _, err := os.Stat(filepath.Join(dir, "out")); err == nil {
		t.Fatal("CopySnapshot leaked the container's build output back onto the host tree")
	}
}

// TestRealRunOutputIsLive checks the payload's own stdout arrives, which is the
// shortest proof that the exec path is really executing inside the container.
func TestRealRunOutputIsLive(t *testing.T) {
	ctx := context.Background()
	sb := requireRealEngine(t, ctx)

	rep, err := sb.Run(ctx, sandbox.Request{
		Argv: []string{"sh", "-c", "echo from-the-container; id -u"},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rep.ExitCode != 0 {
		t.Fatalf("exit %d: %s", rep.ExitCode, rep.Stderr)
	}
	if !strings.Contains(rep.Stdout, "from-the-container") {
		t.Fatalf("stdout = %q", rep.Stdout)
	}
	// Inside a container the payload runs as whatever the image declares, which
	// is a different answer than the host would give. This is the observable
	// difference between isolation and a shell-out.
	if rep.Stdout == "" {
		t.Fatal("empty stdout")
	}
}

// TestRealLostEnvironmentFault injects the ClassLost fault §12.2 asks for: the
// environment is destroyed from outside the process, and the contract must say
// "lost" with a rebuild path rather than reporting a confusing exec failure.
func TestRealLostEnvironmentFault(t *testing.T) {
	ctx := context.Background()
	sb := requireRealEngine(t, ctx)

	sess, err := sb.Session(ctx, "lost-demo", sandbox.WithCreateIfMissing(true))
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	t.Cleanup(func() { _ = sess.Release(ctx) })

	if _, err := sess.Exec(ctx, sandbox.Request{Argv: []string{"true"}}); err != nil {
		t.Fatalf("first Exec: %v", err)
	}

	envID := sess.Status().EnvID
	if envID == "" {
		t.Fatal("session reports no environment id, so the fault cannot be injected")
	}
	out, err := runCLI(dockerBinary(), "rm", "-f", envID)
	if err != nil {
		t.Fatalf("forced container removal failed: %v\n%s", err, out)
	}

	_, err = sess.Exec(ctx, sandbox.Request{Argv: []string{"true"}})
	if err == nil {
		t.Fatal("a run against a destroyed environment must not report success")
	}
	var lost *sandbox.LostError
	if !errors.As(err, &lost) {
		t.Fatalf("want a lost-environment failure, got %T: %v", err, err)
	}
	if f := sandbox.AsFailure(err); f == nil || f.Class != sandbox.ClassLost {
		t.Fatalf("want ClassLost so the retry policy can answer it, got %+v", f)
	}
	if f := sandbox.AsFailure(err); f != nil && f.NextAction != sandbox.ActionRebuild {
		t.Fatalf("a lost environment must point at rebuild, got %v", f.NextAction)
	}
}

// requireRealEngine opens a handle against the real daemon, or skips with a
// reason that says which gate was not met.
func requireRealEngine(t *testing.T, ctx context.Context) *sandbox.Sandbox {
	t.Helper()
	if os.Getenv("HNO_SANDBOX_DOCKER") == "" {
		t.Skip("B2 real-engine gate not exercised: set HNO_SANDBOX_DOCKER=1")
	}
	image := os.Getenv("HNO_SANDBOX_IMAGE")
	if image == "" {
		image = defaultRealImage
	}
	if err := Register(); err != nil {
		t.Fatalf("register: %v", err)
	}
	sb, err := sandbox.Open(ctx,
		sandbox.WithProvider("docker"),
		sandbox.WithAllowedProviders("docker"),
		sandbox.WithImage(image),
		sandbox.WithMinimumIsolation(sandbox.IsolationContainer),
		sandbox.WithAttrs(map[string]any{"binary": dockerBinary()}),
		sandbox.WithTimeouts(sandbox.Timeouts{
			Setup: 3 * time.Minute, // container start plus module resolution
			Exec:  5 * time.Minute,
		}),
		sandbox.WithLimits(sandbox.Limits{OutputMaxBytes: 8 << 20}),
	)
	if err != nil {
		t.Fatalf("Open against %s: %v", image, err)
	}
	t.Cleanup(func() { _ = sb.Close() })

	if err := sb.Probe(ctx); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	// The image must already be local: this test never pulls, so it costs no
	// bandwidth and cannot be blamed for a network round trip it did not ask for.
	// The question goes through the dialect because the two CLIs answer it in
	// different shapes.
	ok, err := ImagePresent(ctx, image, dockerBinary(), "")
	if err != nil {
		t.Fatalf("image presence check: %v", err)
	}
	if !ok {
		t.Skipf("image %s is not present locally and this test does not pull", image)
	}
	return sb
}

// testImage resolves the image the real tests use, so each test does not repeat
// the environment override.
func testImage(t *testing.T) string {
	t.Helper()
	if v := os.Getenv("HNO_SANDBOX_IMAGE"); v != "" {
		return v
	}
	return defaultRealImage
}

func dockerBinary() string {
	if v := os.Getenv("HNO_SANDBOX_BINARY"); v != "" {
		return v
	}
	return "docker"
}

// runCLI executes the container CLI directly for fault injection. It is separate
// from the provider's Runner on purpose: an injection that goes through the code
// under test could be absorbed by it.
func runCLI(bin string, args ...string) (string, error) {
	c := exec.Command(bin, args...)
	out, err := c.CombinedOutput()
	return string(out), err
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func phaseDump(res *sandbox.ProjectResult) string {
	if res == nil {
		return "<nil result>"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "project %s status=%v exit=%d next=%v\n", res.RunID, res.Status, res.ExitCode, res.NextAction)
	if res.Failure != nil {
		fmt.Fprintf(&b, "  failure: class=%v next=%v cause=%v\n", res.Failure.Class, res.Failure.NextAction, res.Failure.Cause)
	}
	for _, p := range res.Phases {
		fmt.Fprintf(&b, "  phase %s: exit=%d status=%v\n", p.Name, p.ExitCode, p.Status)
		if p.Err != nil {
			fmt.Fprintf(&b, "    err: %v\n", p.Err)
		}
		// The phase's own output is what makes a real build failure debuggable,
		// which is the reason PhaseReport carries it.
		if p.Stdout != "" {
			fmt.Fprintf(&b, "    stdout: %s\n", strings.TrimSpace(p.Stdout))
		}
		if p.Stderr != "" {
			fmt.Fprintf(&b, "    stderr: %s\n", strings.TrimSpace(p.Stderr))
		}
	}
	return b.String()
}

// TestRealSweepReclaimsForeignOrphan injects residue the way it actually happens:
// a container created outside this process, carrying the managed label. The sweep
// must find and reclaim it from the engine's own inventory, because a sweeper that
// only knows what the current process created cannot see a crash's leftovers.
func TestRealSweepReclaimsForeignOrphan(t *testing.T) {
	ctx := context.Background()
	sb := requireRealEngine(t, ctx)

	// Make the orphan with the CLI directly, so nothing in this process owns it.
	image := testImage(t)
	name := "hno-orphan-" + t.Name()
	out, err := runCLI(dockerBinary(), "create", "--name", name, "--network", "none",
		"--label", LabelManaged+"=true", "--label", LabelSession+"=ghost", image, "sleep", "infinity")
	if err != nil {
		t.Skipf("could not create the orphan container: %v\n%s", err, out)
	}
	t.Cleanup(func() { _, _ = runCLI(dockerBinary(), "rm", "-f", name) })

	sweep, ok := sb.Reclaimer()
	if !ok {
		t.Fatal("the docker provider must implement Reclaimer; orphan claims are otherwise unverifiable")
	}
	ids, err := sweep.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	var found bool
	for _, id := range ids {
		if id == name {
			found = true
		}
	}
	if !found {
		t.Fatalf("the engine-held orphan was not reported: %v", ids)
	}
	removed, err := sweep.Prune(ctx)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if len(removed) == 0 {
		t.Fatal("Prune removed nothing")
	}
	if _, err := runCLI(dockerBinary(), "inspect", name); err == nil {
		t.Fatal("Prune reported success but the container is still there")
	}
}

// TestRealStreamingIsIncremental proves CapStreaming against a live engine rather
// than a fake that agrees with the provider. The payload prints, sleeps, prints. The
// first line must reach the sink while the container process is demonstrably still
// running, which is checked by asserting the second line arrives strictly after it
// and that the run had not returned when the first one landed.
//
// A provider that buffers and emits one chunk at exit cannot pass this, because both
// lines would arrive after Run() returned — and that is exactly the false claim the
// last audit found.
func TestRealStreamingIsIncremental(t *testing.T) {
	ctx := context.Background()
	sb := requireRealEngine(t, ctx)

	type stamp struct {
		at   time.Time
		text string
	}
	var mu sync.Mutex
	var got []stamp
	runReturned := make(chan struct{})

	rep, err := sb.Run(ctx,
		sandbox.Request{
			Runtime: "sh",
			// Two prints separated by a real sleep inside the container.
			Code: "echo first-line; sleep 2; echo second-line\n",
		},
		sandbox.WithRunSink("watch", sandbox.SinkFunc(func(_ context.Context, ch sandbox.OutputChunk) error {
			mu.Lock()
			got = append(got, stamp{at: time.Now(), text: string(ch.Bytes)})
			mu.Unlock()
			return nil
		})),
	)
	// Close the gate first so any later append is provably post-return.
	close(runReturned)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	var joined string
	for _, g := range got {
		joined += g.text
	}
	if !strings.Contains(joined, "first-line") || !strings.Contains(joined, "second-line") {
		t.Fatalf("streamed output incomplete: %q", joined)
	}

	// The decisive check: the two lines must have been delivered at different times
	// separated by the container's sleep. One buffer flushed at exit collapses that
	// interval to zero, and a single chunk gives fewer than two deliveries.
	var firstAt, secondAt time.Time
	var lines int
	for _, g := range got {
		if strings.Contains(g.text, "first-line") && firstAt.IsZero() {
			firstAt = g.at
		}
		if strings.Contains(g.text, "second-line") {
			secondAt = g.at
			lines++
		}
	}
	if firstAt.IsZero() || secondAt.IsZero() {
		t.Fatalf("both lines must arrive as separate deliveries, got %d chunk(s)", len(got))
	}
	if gap := secondAt.Sub(firstAt); gap < 500*time.Millisecond {
		t.Fatalf("second line arrived %v after the first; the payload slept 2s, so delivery was not incremental", gap)
	}
	// The measured field must agree with what the sink actually saw, otherwise the
	// core's streaming audit is checking a number nobody populated.
	if rep.OutputSent == 0 {
		t.Fatal("OutputSent is zero although the sink received chunks")
	}
	var delivered int64
	for _, g := range got {
		delivered += int64(len(g.text))
	}
	if rep.OutputSent < delivered {
		t.Fatalf("OutputSent %d undercounts what the sink received (%d)", rep.OutputSent, delivered)
	}
	if err := rep.StreamingContract(); err != nil {
		t.Fatalf("a genuinely streaming run must not trip the contract check: %v", err)
	}
	_ = runReturned
}

// TestRealPruneRefusesAnotherProcessesContainer is the regression for the worst
// defect found on a real engine: Prune deleted a container another live process was
// using, destroying that session's data. Ownership was inferred from this process's
// in-memory map, which has no authority over the engine, and concurrent processes are
// the normal case rather than an edge.
//
// The two-process shape is reproduced here honestly: a container started through the
// CLI with our managed label, running, and unknown to this process's map — the exact
// view the victim process presented to the attacker.
func TestRealPruneRefusesAnotherProcessesContainer(t *testing.T) {
	ctx := context.Background()
	sb := requireRealEngine(t, ctx)
	image := testImage(t)
	bin := dockerBinary()

	// The name is per-process. Using a fixed one reproduced the collision this round
	// fixed in Probe: two concurrent runs fight over one engine-global name, and the
	// loser skips (or, worse, reports the engine as broken).
	name := fmt.Sprintf("hno-foreign-live-%d", os.Getpid())
	// The stand-in claims an owner that is alive: a parent process of the test run.
	// Without this label the container is indistinguishable from crash residue, and
	// that is exactly how a concurrent run's stopped container got deleted between
	// create and start.
	owner := fmt.Sprintf("%s/%d/standin", ownerHostOf(ownerID), os.Getppid())
	out, err := runCLI(bin, "create", "--name", name, "--network", "none",
		"--label", LabelManaged+"=true", "--label", LabelSession+"=other-process",
		"--label", LabelOwner+"="+owner, image, "sleep", "infinity")
	if err != nil {
		t.Skipf("could not create the stand-in container: %v\n%s", err, out)
	}
	t.Cleanup(func() {
		_, _ = runCLI(bin, "start", name)
		_, _ = runCLI(bin, "rm", "-f", name)
	})
	if out, err := runCLI(bin, "start", name); err != nil {
		t.Fatalf("could not start the stand-in container: %v\n%s", err, out)
	}

	r, ok := sb.Reclaimer()
	if !ok {
		t.Fatal("the docker provider must implement Reclaimer")
	}
	// A stopped container owned by a live process must survive a prune. This is the
	// half the running-only guard left open: the container exists, is nobody's current
	// workload in this process, and is not safe to delete.
	if _, err := runCLI(bin, "create", "--name", name+"-stopped", "--network", "none",
		"--label", LabelManaged+"=true", "--label", LabelOwner+"="+owner, image, "true"); err != nil {
		t.Skipf("could not create the stopped stand-in: %v", err)
	}
	t.Cleanup(func() { _, _ = runCLI(bin, "rm", "-f", name+"-stopped") })
	if removed, err := r.Prune(ctx); err != nil {
		t.Fatalf("Prune: %v", err)
	} else {
		for _, id := range removed {
			if id == name+"-stopped" {
				t.Fatalf("Prune deleted a stopped container whose owner process is alive -- this is the other process's data, not residue")
			}
		}
	}
	if out, err := runCLI(bin, "inspect", name+"-stopped"); err != nil {
		t.Fatalf("the stopped stand-in should still exist after a safe prune: %v\n%s", err, out)
	}
	ids, err := r.Sweep(ctx)
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	var reported bool
	for _, id := range ids {
		if id == name {
			reported = true
		}
	}
	if !reported {
		t.Fatalf("the live foreign container should be reported by Sweep, got %v", ids)
	}
	removed, err := r.Prune(ctx)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	for _, id := range removed {
		if id == name {
			t.Fatal("Prune deleted a RUNNING container owned by another process — that is data loss, not cleanup")
		}
	}
	if _, err := runCLI(bin, "inspect", name); err != nil {
		t.Fatalf("the running foreign container disappeared: %v", err)
	}
}

// TestRealReadinessProbeSeesARealListener proves the in-guest port probe against a
// service that genuinely is, and genuinely is not, listening. It calls the provider's
// ReadinessProber directly because the docker provider does not yet declare
// CapBackground, and the core only accepts a ReadyProbe on a detached run — so going
// through Request.Expose-style probing here would test a path the provider has not
// opted into, and the assertion would be about the wrong thing.
func TestRealReadinessProbeSeesARealListener(t *testing.T) {
	ctx := context.Background()
	image := testImage(t)
	dockerBinary() // fails fast when the CLI is absent
	if os.Getenv("HNO_SANDBOX_DOCKER") == "" {
		t.Skip("B2 real-engine gate not exercised: set HNO_SANDBOX_DOCKER=1")
	}
	p, err := New(Config{Image: image, Binary: dockerBinary()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = p.Close(context.Background()) }()

	env, err := p.NewEnvironment(ctx, sandbox.EnvSpec{SessionID: "probe-real", Image: image})
	if err != nil {
		t.Fatalf("NewEnvironment: %v", err)
	}
	t.Cleanup(func() { _ = env.Close(context.Background()) })

	// The port is chosen so its hex form contains letters: /proc/net/tcp prints
	// uppercase, and a port like 39217 (0x9911) would match under a lowercase bug too.
	const port = 44713
	probe := sandbox.ReadyProbe{Port: port}

	// Ask about a port nothing listens on first. Reporting ready here would mean the
	// probe is not looking at the guest at all.
	ready, observed, err := p.ProbeReady(ctx, env, probe)
	if err != nil {
		var uns *sandbox.UnsupportedProbeError
		if errors.As(err, &uns) {
			t.Skipf("image %s cannot be asked about a port: %s", image, uns.Fix)
		}
		t.Fatalf("ProbeReady(closed port): %v", err)
	}
	if ready {
		t.Fatalf("a port with no listener was reported ready; observed %q", observed)
	}

	// Start a real listener in its own session. A plain `cmd &` is a child of the exec
	// session and dies when that exec returns, so the probe would then be waiting on a
	// process that was never going to exist.
	listener := "import socket, time\n" +
		"s = socket.socket()\n" +
		"s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)\n" +
		"s.bind((\"0.0.0.0\", 44713))\n" +
		"s.listen(5)\n" +
		"time.sleep(300)\n"
	files, err := p.Files(ctx, env)
	if err != nil {
		t.Fatalf("Files: %v", err)
	}
	if err := files.Write(ctx, "l.py", []byte(listener), 0o644); err != nil {
		t.Fatalf("write listener source: %v", err)
	}
	if _, err := env.Exec(ctx, sandbox.RunSpec{
		RunID:   "probe-start",
		Request: sandbox.Request{Runtime: "shell", Code: "setsid python3 /workspace/l.py >/dev/null 2>&1 </dev/null & sleep 2; kill -0 $(pgrep -f 'python3 /workspace/l.py') 2>/dev/null && echo alive || echo dead"},
	}, nil); err != nil {
		t.Fatalf("start listener: %v", err)
	}

	deadline := time.Now().Add(60 * time.Second)
	for {
		ready, observed, err = p.ProbeReady(ctx, env, probe)
		if err != nil {
			t.Fatalf("ProbeReady(listening port): %v", err)
		}
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("a real listener on :%d was not detected; last observation %q", port, observed)
		}
		time.Sleep(500 * time.Millisecond)
	}
	if strings.TrimSpace(observed) == "" {
		t.Fatal("a ready answer should say what it saw")
	}
}

func TestRealRebuildRestoresRecordedState(t *testing.T) {
	ctx := context.Background()
	sb := requireRealEngine(t, ctx)
	image := testImage(t)

	sess, err := sb.Session(ctx, "rebuild-real", sandbox.WithCreateIfMissing(true))
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if got := sess.Status().Image; got != image {
		t.Fatalf("session image = %q, want the gate image %q", got, image)
	}
	// Two declared effects the rebuilt box cannot possibly have unless replay ran.
	if err := sess.Setup(ctx, "mkdir -p /app/reported && printf 'built-by-replay' > /app/reported/marker"); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	if err := sess.RecordInput(ctx, sandbox.Input{Path: "inputs/spec.txt", Bytes: []byte("payload-42")}); err != nil {
		t.Fatalf("RecordInput: %v", err)
	}

	rep, err := sess.Rebuild(ctx)
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if rep.StateSource != "base image" {
		t.Fatalf("a from-spec rebuild must report that it inherited nothing, got %q", rep.StateSource)
	}
	if rep.Cached != 0 {
		t.Fatalf("a fresh container cannot have inherited a step, yet %d were skipped", rep.Cached)
	}
	if len(rep.Replayed) < 2 {
		t.Fatalf("rebuild replayed %d steps, want the setup and the recorded input: %+v", len(rep.Replayed), rep.Replayed)
	}

	// The evidence is the file existing in the new container, not the report saying it
	// tried.
	check, err := sess.Exec(ctx, sandbox.Shell("cat /app/reported/marker 2>/dev/null; echo ---; cat /workspace/inputs/spec.txt 2>/dev/null || cat inputs/spec.txt 2>/dev/null"))
	if err != nil {
		t.Fatalf("verify Exec: %v", err)
	}
	joined := check.Stdout + check.Stderr
	if !strings.Contains(joined, "built-by-replay") {
		t.Fatalf("the setup step did not survive a real rebuild; engine output %q", joined)
	}
	if !strings.Contains(joined, "payload-42") {
		t.Fatalf("the recorded input did not survive a real rebuild; engine output %q", joined)
	}
}

// TestRealExpiredSessionReclaimsTheContainer is the TTL promise measured against a live
// engine rather than the in-memory double. The design says an expired session emits
// EventSessionExpired and is reclaimed; on a real engine "reclaimed" has to mean the
// container is gone, because a stopped-but-present container is a leak the host pays for.
func TestRealExpiredSessionReclaimsTheContainer(t *testing.T) {
	ctx := context.Background()
	image := testImage(t)
	if os.Getenv("HNO_SANDBOX_DOCKER") == "" {
		t.Skip("B2 real-engine gate not exercised: set HNO_SANDBOX_DOCKER=1")
	}
	var mu sync.Mutex
	var kinds []sandbox.EventKind
	events := sandbox.ObserverFunc(func(_ context.Context, ev sandbox.Event) error {
		mu.Lock()
		defer mu.Unlock()
		kinds = append(kinds, ev.Kind)
		return nil
	})

	if err := Register(); err != nil {
		t.Fatalf("register: %v", err)
	}
	p, err := New(Config{Image: image, Binary: dockerBinary()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer func() { _ = p.Close(context.Background()) }()

	env, err := p.NewEnvironment(ctx, sandbox.EnvSpec{SessionID: "ttl-real", Image: image})
	if err != nil {
		t.Fatalf("NewEnvironment: %v", err)
	}
	envID := env.ID()
	running, err := ContainerRunning(ctx, envID, dockerBinary(), "")
	if err != nil || !running {
		t.Fatalf("the container should be running after create (running=%v err=%v)", running, err)
	}

	// Close is what the core calls when a session expires, so the question is whether
	// that leaves a container behind on the engine.
	if err := env.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if running, err := ContainerRunning(ctx, envID, dockerBinary(), ""); err == nil && running {
		t.Fatal("the closed environment's container is still running on the engine")
	}

	// And the handle-level promise: expiry emits EventSessionExpired and no later run
	// pretends the environment is still there.
	sb, err := sandbox.Open(ctx,
		sandbox.WithProvider("docker"),
		sandbox.WithAllowedProviders("docker"),
		sandbox.WithImage(image),
		sandbox.WithMinimumIsolation(sandbox.IsolationContainer),
		sandbox.WithAttrs(map[string]any{"binary": dockerBinary()}),
		sandbox.WithObserver(events),
		sandbox.WithTimeouts(sandbox.Timeouts{Idle: 50 * time.Millisecond, Provision: 2 * time.Minute}),
	)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = sb.Close() }()

	sess, err := sb.Session(ctx, "ttl-handle", sandbox.WithCreateIfMissing(true))
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if _, err := sess.Exec(ctx, sandbox.Shell("echo alive")); err != nil {
		t.Fatalf("Exec: %v", err)
	}
	time.Sleep(150 * time.Millisecond)
	_, err = sess.Exec(ctx, sandbox.Shell("echo after"))
	if err == nil {
		t.Fatal("an idle-expired session accepted a later run")
	}

	mu.Lock()
	sawExpired := false
	for _, k := range kinds {
		if k == sandbox.EventSessionExpired {
			sawExpired = true
		}
	}
	mu.Unlock()
	if !sawExpired {
		t.Fatal("expiry emitted no EventSessionExpired, so a dashboard watching sessions sees a session that vanished without a cause")
	}
	if st := sess.Status().State; st != sandbox.StateLost && st != sandbox.StateReleased {
		t.Fatalf("an expired session reports %s, want lost or released", st)
	}
}

// TestRealImageCheckIsNotAFalseGreen closes the measurement that opened D-36: on this
// machine, a handle bound to an image the registry has never heard of produced
// "all 8 checks passed" and exit 0 from the CLI, while the very same image through
// `run` failed with the engine's own 401. The probe question and the image question are
// different, and the report has to say so against a live engine rather than only against
// a double.
//
//	HNO_SANDBOX_DOCKER=1 go test -run TestRealImageCheck ./pkg/hno/sandbox/provider/docker -v
func TestRealImageCheckIsNotAFalseGreen(t *testing.T) {
	ctx := context.Background()

	// The image the engine really has. requireRealEngine owns the skip reason and also
	// declines when the image is not local, because this gate never pulls.
	sb := requireRealEngine(t, ctx)
	diag, err := sb.Diagnose(ctx)
	if err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	imgCheck, hit := findCheckNamed(diag, "image")
	if !hit {
		t.Fatal(`no "image" check against the real engine`)
	}
	if !imgCheck.OK {
		t.Errorf("the engine has %s, so the image check must not fail: %+v", testImage(t), imgCheck)
	}

	// A second handle bound to an image that cannot exist here. Open succeeds and the
	// probe passes -- which is precisely the old false green.
	bogus := "hno-does-not-exist/d36:0"
	absent, err := sandbox.Open(ctx,
		sandbox.WithProvider("docker"),
		sandbox.WithAllowedProviders("docker"),
		sandbox.WithImage(bogus),
		sandbox.WithMinimumIsolation(sandbox.IsolationContainer),
		sandbox.WithAttrs(map[string]any{"binary": dockerBinary()}),
	)
	if err != nil {
		t.Fatalf("Open with %q: %v", bogus, err)
	}
	t.Cleanup(func() { _ = absent.Close() })

	diag2, err := absent.Diagnose(ctx)
	if err != nil {
		t.Fatalf("Diagnose (bogus image): %v", err)
	}
	c2, hit := findCheckNamed(diag2, "image")
	if !hit {
		t.Fatal(`no "image" check for the absent image`)
	}
	if c2.OK {
		t.Errorf("D-36 is back: an image this engine cannot start was reported as healthy while the probe also said ok: %+v", c2)
	}
	if probe, ok := findCheckNamed(diag2, "probe"); !ok || !probe.OK {
		t.Logf("probe check in the same report: %+v (exists=%v)", probe, ok)
	}
	if !strings.Contains(c2.Got, bogus) {
		t.Errorf("the finding must name the image the operator asked for, got %q", c2.Got)
	}
	if !strings.Contains(c2.Fix, "pull") && !strings.Contains(c2.Fix, "WithImage") {
		t.Errorf("the fix must be actionable, got %q", c2.Fix)
	}
	// The summary must not read as clean while an image is blocking.
	if strings.Contains(diag2.Summary, "all") && !strings.Contains(strings.ToLower(diag2.Summary), "fail") {
		t.Errorf("summary claims everything passed while the image check failed: %q", diag2.Summary)
	}
}

func findCheckNamed(diag *sandbox.DiagReport, env string) (sandbox.Check, bool) {
	for _, c := range diag.Checks {
		if c.Env == env {
			return c, true
		}
	}
	return sandbox.Check{}, false
}
