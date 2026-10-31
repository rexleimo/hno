// Package docker implements the sandbox Provider contract over the Docker CLI.
//
// It is the first real engine in the contract layer: on macOS the daemon is
// Apple Containerization, whose containers run inside their own VM, so the
// container tier is verifiable without /dev/kvm. On Linux it is runc-style
// namespaces plus seccomp, which is IsolationContainer and not a micro-VM.
//
// # What this provider deliberately does not claim
//
// Capabilities are declared only for behaviour actually implemented here. A
// request for a feature the provider lacks returns a *sandbox.CapabilityError
// from the core rather than being quietly ignored, which is the failure mode
// D-05 exists to remove. Concretely, this provider does not implement:
//
//   - egress allow-lists: the engine offers all-or-nothing networking, so
//     CapEgressAllowlist is not declared and an allow-list request is refused
//     instead of "implemented" as an open network;
//   - background runs and status queries: docker exec cannot be detached and
//     then reliably re-queried, so CapBackground and CapQueryStatus stay off;
//   - snapshots and clone: not implemented, not declared.
//
// # Side effects
//
// Every container is created with a label so orphan reclamation has an
// authoritative source outside this process, and the default network posture is
// closed. Tests run against an injected command runner and never touch a
// daemon; the real round-trip is opt-in via HNO_SANDBOX_DOCKER=1 so
// `go test ./...` never starts a container implicitly.
package docker

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rexleimo/agno-go/pkg/hno/sandbox"
)

// labelPrefix namespaces every container this provider owns. Orphan sweeping
// reads it back from the engine, which is the only source of truth that
// survives a restart of the process that created the container (§15).
const labelPrefix = "hno.sandbox"

// LabelSession marks the session a container belongs to.
const LabelSession = labelPrefix + ".session"

// LabelRun marks the container as managed by this provider generation.
const LabelManaged = labelPrefix + ".managed"

// LabelProbe marks the throwaway container used to ask whether the engine
// supports create at all. It is deliberately a different value from LabelManaged:
// an orphan sweep that reclaimed probe containers would be deleting containers it
// never created through the session path, and a test that selected on the managed
// label alone could not tell the two apart.
const LabelProbe = labelPrefix + ".probe"

// LabelOwner records which process created a container.
//
// It exists because "not in my map" is not evidence of abandonment. A container
// belongs to a live process on some host until proven otherwise, and a sweep that
// decides ownership from one process's memory deletes the containers other sessions
// are actively using. The label makes ownership a fact readable from the engine.
const LabelOwner = labelPrefix + ".owner"

// ownerID identifies this process for ownership checks: host, pid, and a per-process
// nonce, so a recycled pid cannot make a dead process's containers look alive.
var ownerID = func() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown-host"
	}
	var b [3]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%s/%d/%x", host, os.Getpid(), b)
}()

// Config configures the docker provider.
type Config struct {
	// Binary is the container CLI. Defaults to "docker"; "podman" also works
	// because the surface used here is common to both. Set it to "container" for
	// Apple's Containerization CLI, which has no dockerd, no `version`
	// subcommand, and no server-side label filter — the Dialect is inferred from
	// the binary name unless Dialect pins it.
	Binary string
	// Dialect selects the CLI flavour. Zero infers from Binary.
	Dialect Dialect
	// Image is the base image for environments. Required.
	Image string
	// DaemonSocket is an optional DOCKER_HOST value, kept here so a provider
	// instance can target a specific daemon without mutating process env.
	DaemonSocket string
	// AllowBindMounts enables MountShare. Off by default: a bind mount lets a
	// sandbox write the host directory, which is a different trust decision
	// than copying a snapshot in (D-12).
	AllowBindMounts bool
	// PruneRunning lets a sweep delete managed containers that are running but not
	// held by this process. Off by default and only correct on a single-tenant
	// machine: with two processes sharing an engine, turning this on means either can
	// destroy the other's live workload. Measured, not theoretical — the default
	// refusal exists because an experiment reproduced that deletion.
	PruneRunning bool
	// Runtime names the OCI runtime to boot under, e.g. "runsc" for gVisor. Empty
	// uses the engine default. It is the syscall-filtered rung of §12.2: same provider,
	// same lifecycle, different isolation, which is why it is an attribute rather than a
	// second driver.
	Runtime string
	// StartupTimeout bounds container creation, which is engine control-plane
	// work rather than user code time (§16.2).
	StartupTimeout time.Duration
	// Runner overrides command execution. Tests inject a fake so the contract
	// and fault injection run without a daemon.
	Runner Runner
}

// Runner is the command-execution seam. Implementations return the process
// outcome without collapsing a nonzero exit into an error, because "the payload
// exited 1" and "the CLI could not run" must not be confused: the first is user
// code, the second is provider infrastructure.
type Runner interface {
	Run(ctx context.Context, cmd Command) (Outcome, error)
}

// Command describes one CLI invocation.
type Command struct {
	Bin   string
	Args  []string
	Stdin []byte
	// Env is an optional environment for the CLI process itself, never for the
	// sandboxed payload.
	Env []string
	// OnChunk, when set, receives stdout and stderr as they arrive, before the
	// command exits. A Runner that cannot stream simply leaves Outcome.Streamed at
	// zero, and the core's streaming audit reports the gap instead of trusting the
	// provider's capability bit.
	OnChunk func(stream sandbox.Stream, data []byte)
}

// Outcome is what a completed command produced.
type Outcome struct {
	Stdout string
	Stderr string
	Exit   int
	// Streamed counts bytes handed to Command.OnChunk while the command ran. It is
	// how "this provider streams" becomes a measured fact.
	Streamed int64
	// Canceled reports that the context ended the process. A canceled run is
	// not a failed run, and conflating them turns a user cancel into a retryable
	// provider error.
	Canceled bool
}

// execRunner is the production Runner.
type execRunner struct{}

// maxCLICapture bounds what one CLI invocation may accumulate in host memory.
//
// This is not a nicety. `container exec <id> <payload>` pipes the payload's own
// stdout through this buffer, so an unbounded reader turns "the payload printed
// 2 GB" into "the host allocated 2 GB" — and the profile's OutputMaxBytes would
// keep reporting a limit it never enforced. Truncating here with an explicit
// marker is the honest version of the same contract: the ceiling exists, the
// report says it was hit, and the caller can distinguish "no more output" from
// "output I was not allowed to keep".
const maxCLICapture = 32 << 20

// truncatedMark is appended when a stream was cut. It lives in the captured text
// because a provider-level boolean would be lost by every caller that only keeps
// the string.
const truncatedMark = "\n[output truncated at the provider capture ceiling]"

// Run executes the command and returns its outcome.
func (r execRunner) Run(ctx context.Context, cmd Command) (Outcome, error) {
	if cmd.Bin == "" {
		return Outcome{}, errors.New("docker provider: command has no binary")
	}
	if _, err := exec.LookPath(cmd.Bin); err != nil {
		return Outcome{}, fmt.Errorf("docker provider: %q is not installed: %w", cmd.Bin, err)
	}
	// #nosec G204 -- the argument list is assembled in Go from typed config and
	// fixed flags; no caller string is ever split into shell words here.
	c := exec.CommandContext(ctx, cmd.Bin, cmd.Args...)
	if len(cmd.Env) > 0 {
		c.Env = append(c.Environ(), cmd.Env...)
	}
	// Stdin is streamed from the caller's bytes rather than re-serialised, and
	// the pipe is closed after writing so an interactive payload cannot hang the
	// caller waiting on a stream that will never see more input.
	if cmd.Stdin != nil {
		pipe, err := c.StdinPipe()
		if err != nil {
			return Outcome{}, fmt.Errorf("docker provider: stdin pipe: %w", err)
		}
		go func() {
			defer pipe.Close()
			_, _ = pipe.Write(cmd.Stdin)
		}()
	}
	// Both streams are captured independently and bounded, so a successful run
	// keeps its stderr diagnostics instead of losing them the way c.Output()
	// does.
	var stdout, stderr boundedBuffer
	stdout.limit = maxCLICapture
	stderr.limit = maxCLICapture
	var streamed int64
	var streamMu sync.Mutex
	emit := func(st sandbox.Stream, data []byte) {
		streamMu.Lock()
		streamed += int64(len(data))
		streamMu.Unlock()
		if cmd.OnChunk != nil {
			cmd.OnChunk(st, data)
		}
	}
	c.Stdout = &tappedWriter{buf: &stdout, onNext: emit, stream: sandbox.StreamStdout}
	c.Stderr = &tappedWriter{buf: &stderr, onNext: emit, stream: sandbox.StreamStderr}

	err := c.Run()
	res := Outcome{Stdout: stdout.String(), Stderr: stderr.String(), Streamed: streamed}
	if ctx.Err() != nil {
		res.Canceled = true
	}
	if err == nil {
		return res, nil
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		res.Exit = ee.ExitCode()
		// A negative exit code means the process was killed by a signal, which
		// for a sandboxed payload means unknown state rather than a clean failure.
		if res.Exit < 0 {
			res.Canceled = ctx.Err() != nil
		}
		return res, nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		res.Canceled = true
		return res, nil
	}
	return res, err
}

// boundedBuffer collects a stream up to a byte ceiling and then keeps counting
// without storing, so the caller can tell truncation happened and roughly how
// much was dropped.
type boundedBuffer struct {
	buf     strings.Builder
	limit   int
	dropped int
}

// tap forwards each write before the bound is applied, so a caller sees output the
// moment the engine produces it while the retained copy stays bounded.
type tappedWriter struct {
	buf    *boundedBuffer
	onNext func(stream sandbox.Stream, data []byte)
	stream sandbox.Stream
}

func (t *tappedWriter) Write(p []byte) (int, error) {
	n, err := t.buf.Write(p)
	if t.onNext != nil && n > 0 {
		// Forward exactly what was written, including the portion the bound kept, so
		// the streamed view and the captured view cannot disagree about ordering.
		t.onNext(t.stream, p[:n])
	}
	return n, err
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if b.buf.Len() >= b.limit {
		b.dropped += len(p)
		return len(p), nil
	}
	keep := len(p)
	if b.buf.Len()+keep > b.limit {
		keep = b.limit - b.buf.Len()
		b.dropped += len(p) - keep
	}
	b.buf.Write(p[:keep])
	return len(p), nil
}

func (b *boundedBuffer) String() string {
	if b.dropped > 0 {
		return b.buf.String() + fmt.Sprintf("%s (%d more bytes dropped)", truncatedMark, b.dropped)
	}
	return b.buf.String()
}

// Provider is the docker/podman container-backed sandbox provider.
type Provider struct {
	cfg    sandbox.ProviderConfig
	d      Config
	runner Runner
	bin    string
	dial   dialect
	live   map[string]*Environment
	// skippedPrune holds the containers the last Prune refused to delete.
	skippedPrune []string
	mu           syncMu
	closeTo      time.Duration
}

// New builds a provider from the engine-facing config.
func New(d Config) (*Provider, error) {
	if d.Image == "" {
		return nil, fmt.Errorf("sandbox/provider/docker: Config.Image is required")
	}
	if d.Runner == nil {
		d.Runner = execRunner{}
	}
	bin := d.Binary
	if bin == "" {
		bin = "docker"
	}
	return &Provider{
		d:      d,
		runner: d.Runner,
		bin:    bin,
		dial:   newDialect(d.Dialect, bin),
		live:   map[string]*Environment{},
	}, nil
}

// NewProvider adapts the provider to sandbox.ProviderConfig so it can be
// registered as a factory.
func NewProvider(cfg sandbox.ProviderConfig) (sandbox.Provider, error) {
	d := Config{
		Image:          cfg.Image,
		DaemonSocket:   cfg.Endpoint,
		StartupTimeout: time.Duration(cfg.Timeout),
	}
	if v, ok := cfg.Raw["binary"].(string); ok {
		d.Binary = v
	}
	if v, ok := cfg.Raw["dialect"].(string); ok {
		d.Dialect = Dialect(v)
	}
	if v, ok := cfg.Raw["allow_bind_mounts"].(bool); ok {
		d.AllowBindMounts = v
	}
	// startup_timeout was documented-shaped but never read, so a host that set it
	// watched nothing happen: the classic silent no-op. It now accepts a duration
	// string or a time.Duration, and an unusable value is an error rather than a
	// quietly ignored key.
	if v, present := cfg.Raw["startup_timeout"]; present {
		window, err := parseDurationAttr("startup_timeout", v)
		if err != nil {
			return nil, err
		}
		d.StartupTimeout = window
	}
	if v, ok := cfg.Raw["prune_running"].(bool); ok {
		d.PruneRunning = v
	}
	// runtime selects the OCI runtime the engine boots the box under, which is how
	// gVisor's syscall filtering (the middle rung of §12.2) is reached without adding a
	// provider. An empty string is the engine default, so unset costs nothing.
	if v, ok := cfg.Raw["runtime"].(string); ok {
		d.Runtime = v
	}
	if unknown := unknownRawKeys(cfg.Raw); len(unknown) > 0 {
		// Unrecognised keys are refused, not skipped. Raw is the engine-specific
		// passthrough, so a typo here ("startup-timeout") otherwise reads as
		// configuration that took effect while the provider used a default — the one
		// failure mode a passthrough map must not have.
		return nil, fmt.Errorf("sandbox/provider/docker: unrecognised engine attributes %v (known: binary, dialect, allow_bind_mounts, startup_timeout, prune_running, runtime)", unknown)
	}
	p, err := New(d)
	if err != nil {
		return nil, err
	}
	p.cfg = cfg
	return p, nil
}

// Register adds this provider to the sandbox registry under "docker". Import it
// explicitly; nothing here runs from an init() that scans PATH (D-03).
func Register() error {
	if sandbox.HasProvider("docker") {
		return nil
	}
	return sandbox.RegisterProvider("docker", NewProvider)
}

// Name implements sandbox.Provider.
func (p *Provider) Name() string { return "docker" }

// Isolation implements sandbox.Provider.
func (p *Provider) Isolation() sandbox.Isolation { return sandbox.IsolationContainer }

// Capabilities implements sandbox.Provider. Every bit here is implemented in
// this package; the list is a claim, not an aspiration.
func (p *Provider) Capabilities() sandbox.CapabilitySet {
	return sandbox.Capabilities(
		sandbox.CapSession,
		sandbox.CapFileBridge,
		sandbox.CapStreaming,
		sandbox.CapRebuild,
	)
}

// Probe implements sandbox.Provider.
//
// It deliberately checks more than the version handshake. Measured on a real
// macOS machine: the `docker` on PATH was a compatibility shim over Apple
// Containerization, `docker version` answered happily, and the first real run
// then failed with "container create is not supported by Apple Containerization
// runtime". A Probe that stopped at the handshake would have reported a healthy
// provider that cannot execute a single payload, which is worse than no Probe at
// all: the caller stops looking.
//
// So Probe asks the two questions that decide whether this provider can do its
// job, and neither one starts a container or touches the network:
//
//  1. does the CLI answer, and
//  2. does it accept `create` at all.
//
// Step 2 runs `--pull=never` so a missing image produces a truthful "image is
// not local" rather than an implicit download.
func (p *Provider) Probe(ctx context.Context) error {
	if _, err := p.lookPath(ctx); err != nil {
		return err
	}
	ctx, cancel := p.bounded(ctx, 20*time.Second)
	defer cancel()

	ok, detail, err := p.dial.probe(ctx, p.runner, p.bin)
	if err != nil {
		return p.unavailable(detail, err,
			fmt.Sprintf("install %s or set the provider binary explicitly", p.bin),
			"if the binary is a compatibility wrapper, point the profile at the real engine CLI")
	}
	if !ok {
		return p.unavailable(detail, err,
			"start the container daemon (on macOS: Docker Desktop, or `container system start` for Apple Containerization)",
			"or point the profile at a reachable DOCKER_HOST")
	}

	// The create-capability question, asked without starting anything and without
	// pulling. This is the check that caught a CLI answering `version` happily
	// while its runtime refused every container.
	probeName := p.probeContainerName()
	cres, cerr := p.runner.Run(ctx, Command{
		Bin:  p.bin,
		Args: append([]string{"create"}, p.createArgsForProbe(probeName)...),
	})
	// A probe asks a question; it must not leave an answer behind. The container this
	// create produces was permanent, so every Open() added one stopped container to
	// the engine, and because the name was derived from the image the second
	// concurrent process failed with "container already exists" — which reads as an
	// engine fault and turns a healthy machine into an unavailable provider under
	// parallelism, the exact situation an agent workload creates.
	if cerr == nil && cres.Exit == 0 {
		if removed := strings.TrimSpace(cres.Stdout); removed != "" {
			_ = p.remove(context.WithoutCancel(ctx), removed)
		}
	} else if cres.Exit != 0 {
		// A failed create may still have registered the name; clean it so the next
		// probe is not poisoned by this one.
		_ = p.remove(context.WithoutCancel(ctx), probeName)
	}
	if cerr == nil && cres.Exit != 0 {
		if msg := strings.TrimSpace(cres.Stderr); refusesCreate(msg) {
			return p.unavailable(
				fmt.Sprintf("%s answers its version but refuses container create: %s", p.bin, msg),
				errors.New(msg),
				fmt.Sprintf("%s is a compatibility shim without create support: register the provider against the CLI that really creates containers", p.bin),
				"run `which mocker container nerdctl podman` to see the engines installed here",
			)
		}
	}
	return nil
}

// createArgsForProbe renders the minimal "can this engine create at all"
// invocation through the dialect, so the question is asked in the dialect's own
// syntax rather than assuming Docker flags.
func (p *Provider) createArgsForProbe(name string) []string {
	args := p.dial.createArgs(createSpec{
		Name:  name,
		Image: p.probeImage(),
		// The probe must ask about the runtime the provider will actually use.
		// Probing the engine default while creating under "runsc" would report a
		// healthy engine for a configuration that cannot start anything.
		Runtime: p.d.Runtime,
		Network: "none",
		Labels:  map[string]string{LabelManaged: "probe", LabelProbe: "true"},
		Argv:    []string{"true"},
	})
	// The dialect prepends its own "create" verb; strip it here because the
	// caller already added it.
	if len(args) > 0 && args[0] == "create" {
		return args[1:]
	}
	return args
}

// refusesCreate recognises the "this subcommand is not implemented by the
// runtime" family, as opposed to "this image is missing" or "the daemon is busy".
func refusesCreate(msg string) bool {
	lowered := strings.ToLower(msg)
	for _, marker := range []string{
		"is not supported", "not supported by", "unimplemented", "unknown shorthand",
		"unknown flag", "requires dockerd", "no such subcommand",
	} {
		if strings.Contains(lowered, marker) {
			return true
		}
	}
	return false
}

// probeImage is the configured image, so the capability question is asked against
// the environment the caller actually intends to use.
func (p *Provider) probeImage() string {
	if p.d.Image != "" {
		return p.d.Image
	}
	if p.cfg.Image != "" {
		return p.cfg.Image
	}
	return "scratch"
}

// Close implements sandbox.Provider: it removes every environment this
// provider instance created, so a process exit cannot leave containers behind.
func (p *Provider) Close(ctx context.Context) error {
	p.mu.lock()
	envs := make([]*Environment, 0, len(p.live))
	for _, e := range p.live {
		envs = append(envs, e)
	}
	p.live = map[string]*Environment{}
	p.mu.unlock()

	var errs []error
	for _, e := range envs {
		if err := e.Close(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// NewEnvironment implements sandbox.Provider.
func (p *Provider) NewEnvironment(ctx context.Context, spec sandbox.EnvSpec) (sandbox.Environment, error) {
	if _, err := p.lookPath(ctx); err != nil {
		return nil, err
	}
	image := spec.Image
	if image == "" {
		image = p.d.Image
	}
	if mountRequiresShare(spec) && !p.d.AllowBindMounts {
		return nil, &sandbox.DeniedError{
			Policy: "provider/docker",
			Reason: "MountShare needs an explicit enable: it lets the sandbox write the host directory; use CopySnapshot or set allow_bind_mounts",
			Rule:   "docker.bind_mounts",
		}
	}

	labels := map[string]string{LabelManaged: "true", LabelSession: spec.SessionID}
	for k, v := range spec.Labels {
		labels[k] = v
	}
	var mounts []mount
	if p.d.AllowBindMounts && spec.MountMode == sandbox.MountShare && spec.Workspace != nil && spec.Workspace.Dir != "" {
		mounts = append(mounts, mount{Source: spec.Workspace.Dir, Dest: "/workspace"})
	}
	args := p.dial.createArgs(createSpec{
		Image:      image,
		Runtime:    p.d.Runtime,
		WorkingDir: workspaceCWD(spec.MountMode, p.d.AllowBindMounts),
		Env:        envMap(spec.Spec.Env),
		Labels:     withOwner(labels),
		Mounts:     mounts,
		Network:    "none",
		CPUCount:   spec.Limits.CPUCount,
		MemoryMB:   spec.Limits.MemoryMB,
		PidsLimit:  spec.Limits.PidsLimit,
		Argv:       []string{"sleep", "infinity"},
	})

	// Create and start get separate deadlines.
	//
	// They used to share one context, so a create that took most of the startup
	// budget left start with whatever remained — sometimes nothing. The CLI was then
	// killed mid-invocation, exited nonzero, and printed no stderr, which surfaced as
	// "container start exited nonzero" with no reason at all. That was not a rare
	// engine fault: it reproduced on demand with startup_timeout=1200ms and made the
	// real-engine gate flake across every test, because the engine's create takes
	// 1-3s under load. One budget per operation is the whole difference between a
	// slow step and an impossible step.
	createCtx, createCancel := p.bounded(ctx, p.startupWindow())
	res, err := p.runner.Run(createCtx, Command{Bin: p.bin, Args: args})
	createErr := createCtx.Err()
	createCancel()
	if err != nil {
		return nil, p.unavailable("container create failed", err,
			fmt.Sprintf("check that image %q exists locally, or pull it deliberately", image))
	}
	if res.Exit != 0 {
		return nil, p.unavailable("container create exited nonzero",
			engineFailure(res, p.startupWindow(), createErr),
			fmt.Sprintf("image %q may be missing: pull it explicitly, or point the profile at an image that exists", image))
	}
	id := strings.TrimSpace(res.Stdout)
	if id == "" {
		return nil, fmt.Errorf("sandbox/provider/docker: create returned no container id")
	}

	// Start is separate from create so a failure between them still has an id
	// to clean up; a half-created container is exactly the leak that orphan
	// reclamation is supposed to catch.
	startCtx, startCancel := p.bounded(ctx, p.startupWindow())
	sres, err := p.runner.Run(startCtx, Command{Bin: p.bin, Args: []string{"start", id}})
	startErr := startCtx.Err()
	startCancel()
	if err != nil || sres.Exit != 0 {
		// The cleanup must not inherit a deadline that has already passed, or the
		// container we are refusing to adopt stays behind as a leak.
		_ = p.remove(context.WithoutCancel(ctx), id)
		if err != nil {
			return nil, p.unavailable("container start failed", err, "inspect daemon capacity and run the CLI once by hand")
		}
		return nil, p.unavailable("container start exited nonzero",
			engineFailure(sres, p.startupWindow(), startErr),
			fmt.Sprintf("raise WithTimeouts(Setup) / startup timeout above %s, or run `container start %s` by hand to read the engine error", p.startupWindow(), id))
	}

	env := &Environment{provider: p, id: id, session: spec.SessionID, spec: spec.Spec.Clone()}
	p.mu.lock()
	p.live[id] = env
	p.mu.unlock()

	if err := env.materialise(ctx, spec); err != nil {
		_ = env.Close(context.WithoutCancel(ctx))
		return nil, err
	}
	// Recorded steps are replayed by the core through Environment.Exec. That is the
	// single owner since D-20: a provider replaying them itself would be a second
	// owner with its own, narrower notion of what a step is -- this one handled only
	// installs and would have dropped setup and materialise steps without a word.
	return env, nil
}

// materialise writes the declared workspace and inline inputs into a fresh
// container before the first run, so a project round-trip does not depend on
// the payload setting itself up.
func (e *Environment) materialise(ctx context.Context, spec sandbox.EnvSpec) error {
	if spec.Workspace != nil && spec.Workspace.Kind != sandbox.SourceUnknown {
		if spec.Workspace.Kind == sandbox.SourceDir && spec.Workspace.Dir != "" {
			if !e.provider.d.AllowBindMounts || spec.MountMode != sandbox.MountShare {
				res, err := e.provider.runner.Run(ctx, Command{
					Bin:  e.provider.bin,
					Args: []string{"cp", spec.Workspace.Dir + "/.", e.id + ":/workspace"},
				})
				if err != nil {
					return err
				}
				if res.Exit != 0 {
					return &sandbox.PathError{Path: spec.Workspace.Dir, Reason: "workspace copy-in failed: " + strings.TrimSpace(res.Stderr)}
				}
			}
		}
	}
	for _, step := range spec.Spec.Steps {
		if step.Kind != sandbox.StepMaterialize && step.Kind != sandbox.StepWorkspaceDelta {
			continue
		}
		if step.Source == nil {
			continue
		}
		if err := e.materialiseStep(ctx, step); err != nil {
			return err
		}
	}
	return nil
}

func (e *Environment) runner() Runner { return e.provider.runner }

// materialiseStep writes one declared step into the workspace. Only host-file
// and archive sources are supported here; a git or image source needs the engine
// to fetch it, which this provider does not claim, so it is refused rather than
// approximated.
func (e *Environment) materialiseStep(ctx context.Context, step sandbox.Step) error {
	src := step.Source
	if src == nil {
		return nil
	}
	f := e.files()
	switch src.Kind {
	case sandbox.SourceDir:
		if src.Dir == "" {
			return &sandbox.PathError{Path: step.Path, Reason: "materialise step names a dir source with no directory"}
		}
		data, err := os.ReadFile(src.Dir)
		if err != nil {
			return &sandbox.PathError{Path: src.Dir, Reason: "declared workspace input is unreadable: " + err.Error()}
		}
		return f.Write(ctx, step.Path, data, 0o644)
	case sandbox.SourceArchive:
		if src.Archive == nil {
			return &sandbox.PathError{Path: step.Path, Reason: "materialise step names an archive source with no reader"}
		}
		return f.CopyIn(ctx, step.Path, src.Archive)
	default:
		// Git and image sources need the engine to fetch content. This provider
		// does not do that, and there is no capability bit to claim it with, so
		// the refusal is explicit: an approximated checkout would be a silent
		// wrong-input bug in a downstream project run.
		return &sourceUnsupportedError{kind: src.Kind}
	}
}

// networkArg keeps the default posture closed. An unset or open request does not
// get network: egress is a declared capability, and a container provider that
// cannot allowlist should not silently bridge.
func networkArg(spec sandbox.EnvSpec) string {
	return "--network=none"
}

func mountRequiresShare(spec sandbox.EnvSpec) bool {
	return spec.MountMode == sandbox.MountShare && spec.Workspace != nil && spec.Workspace.Kind == sandbox.SourceDir
}

func workspaceCWD(mode sandbox.MountMode, bind bool) string {
	if mode == sandbox.MountShare && !bind {
		return ""
	}
	return "/workspace"
}

// lookPath resolves the CLI once per probe with a typed, fix-bearing error.
func (p *Provider) lookPath(ctx context.Context) (string, error) {
	if _, err := exec.LookPath(p.bin); err != nil {
		return "", p.unavailable(fmt.Sprintf("container CLI %q is not installed", p.bin), err,
			fmt.Sprintf("install %s, or register the provider with the binary you do have", p.bin),
			"or use a hosted provider if this machine cannot run containers")
	}
	return p.bin, nil
}

func (p *Provider) unavailable(detail string, cause error, fixes ...string) error {
	// The reason states the finding once. The cause is kept for Unwrap-style
	// inspection through the attempt's own text rather than being pasted into
	// every layer of the message, which is what made the daemon error appear
	// three times in one line.
	reason := "docker provider: " + detail
	if msg := strings.TrimSpace(errText(cause)); msg != "" && !strings.Contains(reason, msg) {
		reason += ": " + msg
	}
	return &sandbox.UnavailableError{
		Reason: reason,
		Tried: []sandbox.Attempt{{
			Provider:  "docker",
			Isolation: sandbox.IsolationContainer,
			Reason:    detail,
			Fixes:     fixes,
		}},
		Want:  sandbox.IsolationContainer,
		Fixes: fixes,
	}
}

// errText renders an error's message without a type name.
func errText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// Files implements sandbox.FileBridge: the execution environment and the file
// service resolve to one workspace root, which is what kills the split between
// the run path and the file path in the current implementation.
func (p *Provider) Files(_ context.Context, env sandbox.Environment) (sandbox.FileService, error) {
	e, ok := env.(*Environment)
	if !ok {
		return nil, fmt.Errorf("sandbox/provider/docker: Files called with an environment from another provider (%T)", env)
	}
	if e.isClosed() {
		return nil, &sandbox.LostError{SessionID: e.session, Provider: "docker", Cause: errors.New("environment is closed")}
	}
	return e.files(), nil
}

// bounded applies a deadline when the caller configured one.
func (p *Provider) bounded(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if d <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, d)
}

// remove force-deletes one container, tolerating an already-gone id.
// liveIDs snapshots the container ids this process currently owns. The sweep
// compares the engine's answer against this set, so it needs a consistent copy
// without holding the lock across an engine round trip.
func (p *Provider) liveIDs() map[string]*Environment {
	p.mu.lock()
	defer p.mu.unlock()
	out := make(map[string]*Environment, len(p.live))
	for k, v := range p.live {
		out[k] = v
	}
	return out
}

func (p *Provider) remove(ctx context.Context, id string) error {
	res, err := p.runner.Run(ctx, Command{Bin: p.bin, Args: []string{"rm", "-f", id}})
	if err != nil {
		return err
	}
	if res.Exit != 0 && !looksAbsent(res.Stderr) {
		return fmt.Errorf("sandbox/provider/docker: rm %s exited %d: %s", id, res.Exit, strings.TrimSpace(res.Stderr))
	}
	p.mu.lock()
	delete(p.live, id)
	p.mu.unlock()
	return nil
}

func looksAbsent(stderr string) bool {
	lowered := strings.ToLower(stderr)
	return strings.Contains(lowered, "no such") || strings.Contains(lowered, "does not exist")
}

func firstOf(in []string) string {
	if len(in) > 0 {
		return in[0]
	}
	return ""
}

// sourceUnsupportedError is a provider-local typed failure.
//
// The core has no exported error for "this engine cannot fulfil a declared
// workspace source kind", and the closest one, ProvisionError, classifies as
// transient — which would put a permanent limitation behind the retry budget and
// the breaker. Rather than borrow the wrong class, this type implements the
// core's classifier contract directly. The gap itself is recorded in the plan
// doc as a §5.3 finding for review.
type sourceUnsupportedError struct{ kind sandbox.SourceKind }

func (e *sourceUnsupportedError) Error() string {
	return fmt.Sprintf("sandbox/provider/docker cannot materialise a %s workspace source; use FromDir or FromArchive, or add a provider that fetches sources", e.kind)
}

// Class is permanent: no retry makes a missing fetch capability appear.
func (e *sourceUnsupportedError) Class() sandbox.FailureClass { return sandbox.ClassPermanent }

// Remediation names the two shapes this provider can actually fulfil.
func (e *sourceUnsupportedError) Remediation() []string {
	return []string{
		"use sandbox.FromDir(dir) to copy a working tree in",
		"use sandbox.FromArchive(reader) to stream a tar into the workspace",
		"or materialise the project in the image and select it as the provider image",
	}
}

// envMap turns "K=V" spec entries into a map so the dialect renders them.
func envMap(in []string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for _, kv := range in {
		if k, v, ok := strings.Cut(kv, "="); ok {
			out[k] = v
		}
	}
	return out
}

// splitLines drops blank and whitespace-only lines while keeping order, which is
// how a CLI id listing becomes a slice.
func splitLines(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if t := strings.TrimSpace(line); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// Sweep reports engine-held containers carrying the managed label that this process
// no longer owns. Implements sandbox.Reclaimer so an operator can reclaim a crash's
// residue without running a full diagnosis.
func (p *Provider) Sweep(ctx context.Context) ([]string, error) { return sweeper{p: p}.Sweep(ctx) }

// Prune deletes what Sweep reported.
func (p *Provider) Prune(ctx context.Context) ([]string, error) { return sweeper{p: p}.Prune(ctx) }

// startupWindow is the deadline for one engine control-plane step.
func (p *Provider) startupWindow() time.Duration {
	if p.d.StartupTimeout > 0 {
		return p.d.StartupTimeout
	}
	return defaultStartupTimeout
}

// engineFailure turns a CLI step that did not succeed into the reason a caller can
// act on.
//
// The engine writes real diagnostics to stderr, but a killed process writes none:
// reporting "exited nonzero" with an empty cause was how a deadline problem masqueraded
// as an unknown container fault, which is the one class of error a retry layer should
// not be guessing about.
func engineFailure(res Outcome, window time.Duration, ctxErr error) error {
	if msg := strings.TrimSpace(res.Stderr); msg != "" {
		return errors.New(msg)
	}
	if ctxErr != nil || res.Canceled {
		return fmt.Errorf("the engine command was killed by its own deadline after %s (no engine output): %w", window, context.DeadlineExceeded)
	}
	if strings.TrimSpace(res.Stdout) != "" {
		return errors.New("no stderr; engine wrote to stdout: " + strings.TrimSpace(res.Stdout))
	}
	return fmt.Errorf("the container CLI exited %d without any output", res.Exit)
}

// defaultStartupTimeout bounds one control-plane step when the profile is silent.
const defaultStartupTimeout = 60 * time.Second

// parseDurationAttr reads a duration attribute that may arrive as a string, an
// integer of nanoseconds, or a time.Duration, since Raw comes from decoded config
// where a duration is usually text.
func parseDurationAttr(key string, v any) (time.Duration, error) {
	switch t := v.(type) {
	case time.Duration:
		return t, nil
	case string:
		d, err := time.ParseDuration(t)
		if err != nil {
			return 0, fmt.Errorf("sandbox/provider/docker: attribute %q is not a duration: %w", key, err)
		}
		return d, nil
	case int:
		return time.Duration(t) * time.Second, nil
	case int64:
		return time.Duration(t) * time.Second, nil
	case float64:
		return time.Duration(t) * time.Second, nil
	default:
		return 0, fmt.Errorf("sandbox/provider/docker: attribute %q has unsupported type %T", key, v)
	}
}

// knownRawKeys lists the attributes this provider honours.
var knownRawKeys = map[string]bool{
	"binary":            true,
	"runtime":           true,
	"dialect":           true,
	"allow_bind_mounts": true,
	"startup_timeout":   true,
	"prune_running":     true,
}

func unknownRawKeys(raw map[string]any) []string {
	var out []string
	for k := range raw {
		if !knownRawKeys[k] {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// probeContainerName is unique per process and per call, so concurrent probes do
// not collide on a name the engine treats as global.
func (p *Provider) probeContainerName() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	stem := p.probeImage()
	if i := strings.LastIndexAny(stem, "/:"); i >= 0 {
		stem = stem[i+1:]
	}
	if len(stem) > 6 {
		stem = stem[:6]
	}
	return fmt.Sprintf("hno-probe-%s-%d-%x", stem, os.Getpid(), b)
}

// withOwner adds the owner label without overwriting an explicit value.
func withOwner(labels map[string]string) map[string]string {
	out := make(map[string]string, len(labels)+1)
	for k, v := range labels {
		out[k] = v
	}
	if out[LabelOwner] == "" {
		out[LabelOwner] = ownerID
	}
	return out
}

// noteSkippedPrune records what a safe prune refused to delete, so the refusal is
// answerable instead of invisible. Last write wins: the list is operational detail,
// not a queue that grows without bound.
func (p *Provider) noteSkippedPrune(refused []string) {
	p.mu.lock()
	defer p.mu.unlock()
	p.skippedPrune = refused
}

// SkippedPrune reports the containers the last prune declined, naming the owner
// label so an operator can find the process that is using them.
func (p *Provider) SkippedPrune() []string {
	p.mu.lock()
	defer p.mu.unlock()
	out := make([]string, len(p.skippedPrune))
	copy(out, p.skippedPrune)
	return out
}
