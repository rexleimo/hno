package docker

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rexleimo/agno-go/pkg/hno/sandbox"
)

// probeCommands records what a readiness probe asked the engine to do, so the answer can
// be traced to a real in-guest check instead of a claim.
func probeCommands(fr *fakeRunner) []string {
	fr.mu.Lock()
	defer fr.mu.Unlock()
	var out []string
	for _, c := range fr.seen {
		if len(c.Args) > 0 && c.Args[0] == "exec" {
			out = append(out, strings.Join(c.Args, " "))
		}
	}
	return out
}

func TestProbePortAsksTheGuest(t *testing.T) {
	fr := &fakeRunner{respond: func(c Command) (Outcome, bool) {
		if strings.Contains(strings.Join(c.Args, " "), "/dev/tcp/127.0.0.1/8080") {
			return Outcome{Exit: 0, Stdout: "connected\n"}, true
		}
		return defaultResponse(c), true
	}}
	p := newTestProvider(t, fr)
	env, err := p.NewEnvironment(context.Background(), sandbox.EnvSpec{SessionID: "rd", Image: "golang:1.26"})
	if err != nil {
		t.Fatalf("NewEnvironment: %v", err)
	}
	ready, observed, err := p.ProbeReady(context.Background(), env, sandbox.ReadyProbe{Port: 8080})
	if err != nil {
		t.Fatalf("ProbeReady: %v", err)
	}
	if !ready {
		t.Fatalf("a guest reporting a successful connect must be ready, observed %q", observed)
	}
	// The port has to appear in the command the engine ran, otherwise the probe
	// checked something else.
	var asked bool
	for _, line := range probeCommands(fr) {
		if strings.Contains(line, "8080") {
			asked = true
		}
	}
	if !asked {
		t.Fatalf("probe never named the port to the guest: %v", probeCommands(fr))
	}
}

func TestProbePortFallsBackToListeningTable(t *testing.T) {
	// No bash: the connect strategy fails, so the /proc/net/tcp table must answer.
	fr := &fakeRunner{respond: func(c Command) (Outcome, bool) {
		joined := strings.Join(c.Args, " ")
		if strings.Contains(joined, "/dev/tcp") {
			return Outcome{Exit: 127, Stderr: "bash: not found"}, true
		}
		if strings.Contains(joined, "/proc/net/tcp") && strings.Contains(joined, "1F90") {
			return Outcome{Exit: 0, Stdout: "listening\n"}, true
		}
		if strings.Contains(joined, "test -r /proc/net/tcp") {
			return Outcome{Exit: 0}, true
		}
		return defaultResponse(c), true
	}}
	p := newTestProvider(t, fr)
	env, _ := p.NewEnvironment(context.Background(), sandbox.EnvSpec{SessionID: "rd2", Image: "golang:1.26"})
	ready, _, err := p.ProbeReady(context.Background(), env, sandbox.ReadyProbe{Port: 8080})
	if err != nil {
		t.Fatalf("ProbeReady: %v", err)
	}
	if !ready {
		t.Fatal("the listening table must answer when bash is absent")
	}
}

func TestProbePortNotReadyIsNotAnError(t *testing.T) {
	// A service still starting is the normal case: the probe is polled, so "no
	// listener yet" has to be an answer rather than a failure.
	fr := &fakeRunner{respond: func(c Command) (Outcome, bool) {
		joined := strings.Join(c.Args, " ")
		if strings.Contains(joined, "test -r /proc/net/tcp") {
			return Outcome{Exit: 0}, true
		}
		if len(c.Args) > 0 && c.Args[0] == "exec" {
			return Outcome{Exit: 1}, true
		}
		return defaultResponse(c), true
	}}
	p := newTestProvider(t, fr)
	env, err := p.NewEnvironment(context.Background(), sandbox.EnvSpec{SessionID: "rd3", Image: "golang:1.26"})
	if err != nil {
		t.Fatalf("NewEnvironment: %v", err)
	}
	ready, observed, err := p.ProbeReady(context.Background(), env, sandbox.ReadyProbe{Port: 9})
	if err != nil {
		t.Fatalf("not-ready must not be an error: %v", err)
	}
	if ready {
		t.Fatal("a guest with no listener must not report ready")
	}
	if observed == "" {
		t.Fatal("the probe must say what it saw so a timeout can be explained")
	}
}

func TestProbeGuestWithoutShellRefusesRatherThanPasses(t *testing.T) {
	fr := &fakeRunner{respond: func(c Command) (Outcome, bool) {
		joined := strings.Join(c.Args, " ")
		if strings.Contains(joined, "test -r /proc/net/tcp") {
			return Outcome{Exit: 1, Stderr: "no shell"}, true
		}
		if len(c.Args) > 0 && c.Args[0] == "exec" {
			return Outcome{Exit: 1}, true
		}
		return defaultResponse(c), true
	}}
	p := newTestProvider(t, fr)
	env, _ := p.NewEnvironment(context.Background(), sandbox.EnvSpec{SessionID: "rd4", Image: "scratch"})
	ready, _, err := p.ProbeReady(context.Background(), env, sandbox.ReadyProbe{Port: 8080})
	if ready {
		t.Fatal("a guest that cannot be asked must never report ready")
	}
	var uns *sandbox.UnsupportedProbeError
	if !errors.As(err, &uns) {
		t.Fatalf("an unaskable guest must refuse explicitly, got %T: %v", err, err)
	}
	if uns.Fix == "" {
		t.Fatal("the refusal must name what the caller can do instead")
	}
}

func TestProbeURLNeedsAClient(t *testing.T) {
	fr := &fakeRunner{respond: func(c Command) (Outcome, bool) {
		if strings.Contains(strings.Join(c.Args, " "), "no-http-client") {
			return Outcome{Exit: 97, Stdout: "no-http-client"}, true
		}
		if len(c.Args) > 0 && c.Args[0] == "exec" {
			return Outcome{Exit: 1}, true
		}
		return defaultResponse(c), true
	}}
	p := newTestProvider(t, fr)
	env, _ := p.NewEnvironment(context.Background(), sandbox.EnvSpec{SessionID: "rd5", Image: "golang:1.26"})
	ready, _, err := p.ProbeReady(context.Background(), env, sandbox.ReadyProbe{URL: "http://127.0.0.1:8080/healthz"})
	if ready {
		t.Fatal("a guest with no HTTP client must not report a URL as answered")
	}
	var uns *sandbox.UnsupportedProbeError
	if !errors.As(err, &uns) {
		t.Fatalf("want *UnsupportedProbeError, got %T: %v", err, err)
	}
}

func TestProbeURLTreatsNon2xxAsNotReady(t *testing.T) {
	fr := &fakeRunner{respond: func(c Command) (Outcome, bool) {
		if strings.Contains(strings.Join(c.Args, " "), "curl") {
			return Outcome{Exit: 0, Stdout: "503"}, true
		}
		return defaultResponse(c), true
	}}
	p := newTestProvider(t, fr)
	env, _ := p.NewEnvironment(context.Background(), sandbox.EnvSpec{SessionID: "rd6", Image: "golang:1.26"})
	ready, observed, err := p.ProbeReady(context.Background(), env, sandbox.ReadyProbe{URL: "http://127.0.0.1:8080/"})
	if err != nil {
		t.Fatalf("a 503 while warming up is an answer, not an error: %v", err)
	}
	if ready {
		t.Fatal("503 is not 2xx")
	}
	if !strings.Contains(observed, "503") {
		t.Fatalf("the probe must report what the service answered, got %q", observed)
	}
}

func TestProbePatternIsLeftToTheCore(t *testing.T) {
	fr := &fakeRunner{}
	p := newTestProvider(t, fr)
	env, _ := p.NewEnvironment(context.Background(), sandbox.EnvSpec{SessionID: "rd7", Image: "golang:1.26"})
	_, _, err := p.ProbeReady(context.Background(), env, sandbox.ReadyProbe{Pattern: "ready"})
	var uns *sandbox.UnsupportedProbeError
	if !errors.As(err, &uns) {
		t.Fatalf("a provider that matched patterns would duplicate the core's own matcher, got %v", err)
	}
}

func TestProbeForeignEnvironmentIsRefused(t *testing.T) {
	p := newTestProvider(t, &fakeRunner{})
	if _, _, err := p.ProbeReady(context.Background(), foreignEnv{}, sandbox.ReadyProbe{Port: 1}); err == nil {
		t.Fatal("a foreign environment must be refused, not probed blind")
	}
}

type foreignEnv struct{}

func (foreignEnv) ID() string                   { return "foreign" }
func (foreignEnv) Image() string                { return "x" }
func (foreignEnv) Isolation() sandbox.Isolation { return sandbox.IsolationContainer }
func (foreignEnv) Alive() bool                  { return true }
func (foreignEnv) Close(context.Context) error  { return nil }
func (foreignEnv) Exec(context.Context, sandbox.RunSpec, sandbox.ChunkReporter) (*sandbox.Report, error) {
	return nil, nil
}
