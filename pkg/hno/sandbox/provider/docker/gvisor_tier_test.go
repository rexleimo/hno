package docker

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/rexleimo/agno-go/pkg/hno/sandbox"
)

// TestGVisorTierIsExercisedOrExplicitlyAbsent is the syscall-filtered rung of the design's
// isolation ladder (§12.2 lists gVisor as the local tier between containers and micro-VMs).
// The rung is a runtime choice, not a second provider: the same CLI asks for the sandboxed
// runtime through --runtime.
//
// It is reported as a visible skip when runsc is not installed, because a suite that
// quietly passed without ever touching a tier would leave the design looking implemented
// when only the container rung exists on this machine.
func TestGVisorTierIsExercisedOrExplicitlyAbsent(t *testing.T) {
	ctx := context.Background()
	if os.Getenv("HNO_SANDBOX_DOCKER") == "" {
		t.Skip("B2 real-engine gate not exercised: set HNO_SANDBOX_DOCKER=1")
	}
	bin := dockerBinary()
	// The question goes to the engine, not to a file on disk: a runtime binary that the
	// daemon has not registered is not usable, and a registered one may live in the
	// daemon's own filesystem.
	out, err := runCLI(bin, "info", "--format", "{{json .Runtimes}}")
	if err != nil {
		t.Skipf("engine %s did not answer a runtime list: %v", bin, err)
	}
	if !strings.Contains(out, "runsc") {
		t.Skipf("no gVisor runtime registered with %s; runsc availability on this host: %v — the syscall-filtered rung is unverified here, not proven working",
			bin, lookup("runsc"))
	}
	if _, err := exec.LookPath("runsc"); err != nil {
		t.Skipf("runsc is registered but not on PATH: %v", err)
	}
	// Present and registered: the tier must actually boot a box.
	p, err := New(Config{
		Image:   testImage(t),
		Binary:  bin,
		Runtime: "runsc",
	})
	if err != nil {
		t.Fatalf("New with the gVisor runtime: %v", err)
	}
	defer func() { _ = p.Close(context.Background()) }()
	env, err := p.NewEnvironment(ctx, sandbox.EnvSpec{SessionID: "gvisor-tier", Image: testImage(t)})
	if err != nil {
		t.Fatalf("the engine has a runsc runtime but cannot start under it: %v", err)
	}
	defer func() { _ = env.Close(context.Background()) }()
	rep, err := env.Exec(ctx, sandbox.RunSpec{
		RunID:   "gvisor-echo",
		Request: sandbox.Shell("echo filtered"),
	}, nil)
	if err != nil {
		t.Fatalf("Exec under runsc: %v", err)
	}
	if rep.ExitCode != 0 || !strings.Contains(rep.Stdout, "filtered") {
		t.Fatalf("syscall-filtered run produced exit %d and output %q", rep.ExitCode, rep.Stdout)
	}
}

func lookup(name string) error {
	_, err := exec.LookPath(name)
	return err
}
