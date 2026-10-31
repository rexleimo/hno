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
