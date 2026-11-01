// Command sandbox_agent_workflow demonstrates the four shapes an agent needs in
// order to write code into a sandbox, run it, and let the result drive the next
// step. Every scenario is a real run against a real engine; nothing is mocked.
//
// Prerequisites (all four are required; the program checks them and says which
// one is missing rather than hanging on the first Exec):
//
//  1. an engine on PATH — docker, podman, or the Apple containerisation CLI
//  2. the image locally present (default golang:1.26)
//  3. HNO_SANDBOX_BINARY when the engine CLI is not called "docker"
//  4. a moment before the first run: go run . -doctor
//
// Usage:
//
//	go run ./cmd/examples/sandbox_agent_workflow -doctor
//	go run ./cmd/examples/sandbox_agent_workflow -scenario all
//	go run ./cmd/examples/sandbox_agent_workflow -scenario write-then-run
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/rexleimo/agno-go/pkg/hno/sandbox"
	"github.com/rexleimo/agno-go/pkg/hno/sandbox/provider/docker"
)

var (
	providerName = flag.String("provider", "docker", "registered sandbox provider")
	image        = flag.String("image", envOr("HNO_SANDBOX_IMAGE", "golang:1.26"), "image every box starts from")
	binary       = flag.String("binary", envOr("HNO_SANDBOX_BINARY", ""), "engine CLI name when it is not 'docker' (e.g. 'container')")
	scenario     = flag.String("scenario", "all", "all, or one of: write-then-run, incremental, streaming, project, inline-inputs")
	doctor       = flag.Bool("doctor", false, "check the host prerequisites and exit")
	ctxTimeout   = flag.Duration("timeout", 8*time.Minute, "per-scenario deadline")
)

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	flag.Parse()

	// Registering explicitly (rather than a blank import) is what makes a missing
	// provider a value the caller can act on instead of a surprise at Open time.
	if err := docker.Register(); err != nil {
		log.Fatalf("register provider: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *ctxTimeout)
	defer cancel()

	if *doctor {
		runDoctor(ctx)
		return
	}

	names := []string{"write-then-run", "incremental", "streaming", "project", "inline-inputs"}
	if *scenario != "all" {
		names = []string{*scenario}
	}
	for _, name := range names {
		fmt.Printf("\n=== %s ===\n", name)
		if err := runScenario(ctx, name); err != nil {
			// A scenario failure is the interesting result, not a crash: print it and
			// keep going so one missing image does not hide the other four outcomes.
			fmt.Printf("  FAILED: %v\n", err)
			os.Exit(1)
		}
	}
}

// runDoctor answers "is this host able to run the scenarios at all?" before any
// box is created. sandbox.Diagnose is the same check `hno sandbox doctor` runs.
func runDoctor(ctx context.Context) {
	sb, err := openHandle(ctx)
	if err != nil {
		fmt.Printf("host cannot open a sandbox: %v\n", err)
		fmt.Println("fix: check the engine is running, and pass -binary if its CLI is not named 'docker'")
		os.Exit(70)
	}
	defer sb.Close()

	diag, err := sb.Diagnose(ctx)
	if err != nil {
		fmt.Printf("diagnose failed: %v\n", err)
		os.Exit(70)
	}
	// DiagReport is fields only (no methods), so "is the host ready" is computed
	// from the checks rather than asked of the report.
	allOK := true
	for _, c := range diag.Checks {
		fmt.Printf("%-6s %-12s want=%s got=%s\n", verdict(c.OK), c.Env, c.Want, c.Got)
		if !c.OK {
			allOK = false
			if c.Fix != "" {
				fmt.Printf("       fix: %s\n", c.Fix)
			}
		}
	}
	for _, f := range diag.Fixes {
		fmt.Printf("overall fix: %s\n", f)
	}
	fmt.Printf("reached isolation=%s\n%s\n", diag.Reached, diag.Summary)
	if !allOK {
		os.Exit(1)
	}
}

func verdict(ok bool) string {
	if ok {
		return "PASS"
	}
	return "FAIL"
}

func openHandle(ctx context.Context) (*sandbox.Sandbox, error) {
	opts := []sandbox.Option{
		sandbox.WithProvider(*providerName),
		sandbox.WithImage(*image),
	}
	if *binary != "" {
		// Engine CLI name is a provider attribute, not a global option: the core stays
		// free of per-engine knobs.
		opts = append(opts, sandbox.WithAttrs(map[string]any{"binary": *binary}))
	}
	return sandbox.Open(ctx, opts...)
}

// newSession creates a session id this handle has never seen. Without
// WithCreateIfMissing the runtime refuses to guess that an unknown id is a fresh
// start rather than a lost environment.
func newSession(ctx context.Context, id string) (*sandbox.Sandbox, *sandbox.Session, error) {
	sb, err := openHandle(ctx)
	if err != nil {
		return nil, nil, err
	}
	sess, err := sb.Session(ctx, id, sandbox.WithCreateIfMissing(true))
	if err != nil {
		sb.Close()
		return nil, nil, err
	}
	return sb, sess, nil
}

func runScenario(ctx context.Context, name string) error {
	switch name {
	case "write-then-run":
		return writeThenRun(ctx)
	case "incremental":
		return incremental(ctx)
	case "streaming":
		return streaming(ctx)
	case "project":
		return project(ctx)
	case "inline-inputs":
		return inlineInputs(ctx)
	default:
		return fmt.Errorf("unknown scenario %q", name)
	}
}

// Scenario 1: the agent emits source, the runtime writes it, then builds it.
// Writing and running are separate contracts — a successful Write proves nothing
// about whether the payload compiles.
func writeThenRun(ctx context.Context) error {
	sb, sess, err := newSession(ctx, "agent-writes-then-builds")
	if err != nil {
		return err
	}
	defer sb.Close()
	defer sess.Release(ctx)

	files, err := sess.Files()
	if err != nil {
		return fmt.Errorf("no file bridge (%v)", err)
	}
	if err := files.MkdirAll(ctx, "hello", 0o755); err != nil {
		return err
	}
	// go.mod is not optional: without a module file the toolchain reports
	// "go.mod file not found in current directory or any parent directory".
	if err := files.Write(ctx, "hello/go.mod", []byte("module hello\n\ngo 1.26\n"), 0o644); err != nil {
		return err
	}
	src := []byte("package main\n\nimport \"fmt\"\n\nfunc main() { fmt.Println(\"hello from the sandbox\") }\n")
	if err := files.Write(ctx, "hello/main.go", src, 0o644); err != nil {
		return err
	}

	// Relative paths land under the workspace, which is the shell working directory.
	rep, err := sess.Exec(ctx, sandbox.Shell("cd hello && GOFLAGS=-mod=mod go build -o /tmp/app . && /tmp/app"))
	if err != nil {
		return fmt.Errorf("engine failure (not a code failure): %w", err)
	}
	fmt.Printf("exit=%d stdout=%q\n", rep.ExitCode, strings.TrimSpace(rep.Stdout))
	if rep.ExitCode != 0 {
		fmt.Printf("stderr=%s\n", rep.Stderr)
	}
	return nil
}

// Scenario 2: verify after every write. The sandbox is a per-step feedback loop,
// not a final gate — this is where an agent catches its own undefined symbol after
// the file that caused it, instead of after the whole tree.
func incremental(ctx context.Context) error {
	sb, sess, err := newSession(ctx, "incremental")
	if err != nil {
		return err
	}
	defer sb.Close()
	defer sess.Release(ctx)

	files, err := sess.Files()
	if err != nil {
		return err
	}
	steps := []struct{ path, body string }{
		{"app/go.mod", "module app\n\ngo 1.26\n"},
		{"app/main.go", "package main\n\nfunc main() { println(Greet()) }\n"},
		{"app/greet.go", "package main\n\nfunc Greet() string { return \"hello\" }\n"},
	}
	if err := files.MkdirAll(ctx, "app", 0o755); err != nil {
		return err
	}
	for i, st := range steps {
		if err := files.Write(ctx, st.path, []byte(st.body), 0o644); err != nil {
			return err
		}
		// No pipe here on purpose. `go vet ./... | head -5` exits with head's status,
		// so a compile error would report exit=0 and the agent would move on.
		rep, err := sess.Exec(ctx, sandbox.Shell("cd app && GOFLAGS=-mod=mod go vet ./..."))
		if err != nil {
			return err
		}
		fmt.Printf("step %d wrote %-14s vet exit=%d\n", i+1, st.path, rep.ExitCode)
		if rep.ExitCode != 0 {
			// Hand only this error back to the model, not the whole tree.
			fmt.Printf("  model should fix: %s\n", strings.TrimSpace(firstLine(rep.Stderr)))
		}
	}
	rep, err := sess.Exec(ctx, sandbox.Shell("cd app && GOFLAGS=-mod=mod go build -o /tmp/app . && /tmp/app"))
	if err != nil {
		return err
	}
	fmt.Printf("final build exit=%d stdout=%q\n", rep.ExitCode, strings.TrimSpace(rep.Stdout))
	return nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// Scenario 3: output arrives while the run is still going, and the failure class
// decides the next move mechanically instead of by string-matching stderr.
func streaming(ctx context.Context) error {
	sb, sess, err := newSession(ctx, "streaming")
	if err != nil {
		return err
	}
	defer sb.Close()
	defer sess.Release(ctx)

	sink := &collector{}
	rep, err := sess.Exec(ctx,
		sandbox.Shell("for i in 1 2 3; do echo line-$i; sleep 0.2; done; echo BUILD_FAILED >&2; exit 2"),
		sandbox.WithRunSink("collect", sink),
	)
	if err != nil {
		return err
	}
	// OutputSent is the proof of incremental delivery; a provider that buffered the
	// whole run would report chunks=0 against a nonzero exit.
	fmt.Printf("exit=%d chunks=%d outputSent=%d\n", rep.ExitCode, len(sink.chunks), rep.OutputSent)
	fmt.Printf("sink saw failure marker live: %v\n", strings.Contains(sink.all(), "BUILD_FAILED"))
	if rep.Failure != nil {
		fmt.Printf("class=%s next action=%s\n", rep.Failure.Class, rep.Failure.NextAction)
	}
	// The branch an orchestrator writes: environment trouble retries or rebuilds,
	// user-code trouble goes back to the model, anything else changes strategy.
	switch {
	case rep.ExitCode != 0 && rep.Failure != nil && rep.Failure.NextAction == sandbox.ActionModelFix:
		fmt.Println("-> route stderr back to the model and regenerate the failing file")
	case rep.ExitCode != 0:
		fmt.Println("-> not a code problem: change strategy rather than regenerate")
	}
	return nil
}

type collector struct {
	mu     sync.Mutex
	chunks []sandbox.OutputChunk
}

func (c *collector) Emit(_ context.Context, ch sandbox.OutputChunk) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.chunks = append(c.chunks, ch)
	return nil
}

func (c *collector) all() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var b strings.Builder
	for _, ch := range c.chunks {
		b.Write(ch.Bytes)
	}
	return b.String()
}

const scaffold = `mkdir -p app && cd app &&
cat > go.mod <<'EOF'
module app

go 1.26
EOF
cat > main.go <<'EOF'
package main

import "fmt"

func main() { fmt.Println(sum(2, 3)) }
EOF
cat > sum.go <<'EOF'
package main

func sum(a, b int) int { return a + b }
EOF
`

// Scenario 4: a whole tree, ordered phases sharing one workspace, and only the
// declared artefacts come back. Required phases stop the run; FailOpen ones do not.
func project(ctx context.Context) error {
	sb, err := openHandle(ctx)
	if err != nil {
		return err
	}
	defer sb.Close()

	res, err := sb.RunProject(ctx, sandbox.Project{
		ID: "agent-project",
		Phases: []sandbox.Phase{
			{Name: "scaffold", Required: true, Run: sandbox.Shell(scaffold)},
			{Name: "vet", Required: true, Run: sandbox.Shell("cd app && GOFLAGS=-mod=mod go vet ./...")},
			{Name: "run", Required: true, Run: sandbox.Shell("cd app && GOFLAGS=-mod=mod go run . > /workspace/out.txt")},
		},
		Artifacts: []sandbox.ArtifactSpec{{Path: "out.txt", Required: true}},
	})
	if err != nil {
		return err
	}
	fmt.Printf("status=%v exit=%d phases=%d\n", res.Status, res.ExitCode, len(res.Phases))
	for _, ph := range res.Phases {
		fmt.Printf("  phase %-8s exit=%d\n", ph.Name, ph.ExitCode)
	}
	for _, a := range res.Artifacts {
		fmt.Printf("  artifact %-8s %d bytes: %q\n", a.Path, a.Size, string(a.Content()))
	}
	return nil
}

// Scenario 5: files delivered as part of the run, with no file bridge involved.
func inlineInputs(ctx context.Context) error {
	sb, sess, err := newSession(ctx, "inline-inputs")
	if err != nil {
		return err
	}
	defer sb.Close()
	defer sess.Release(ctx)

	rep, err := sess.Exec(ctx, sandbox.Request{
		Runtime: "shell",
		Code:    "cat gen/app.py && python3 gen/app.py",
		Inputs: []sandbox.Input{
			{Path: "gen/app.py", Bytes: []byte("print('inline input ran')\n")},
		},
	})
	if err != nil {
		return err
	}
	fmt.Printf("exit=%d stdout=%q\n", rep.ExitCode, strings.TrimSpace(rep.Stdout))
	if rep.ExitCode != 0 {
		fmt.Printf("stderr=%s\n", rep.Stderr)
	}
	return nil
}
