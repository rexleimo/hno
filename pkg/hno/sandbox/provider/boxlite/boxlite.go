// Package boxlite implements the sandbox Provider contract over the boxlite CLI.
//
// # Why a CLI and not an SDK
//
// The upstream project documents a Go SDK — its README says
// `go get github.com/boxlite-ai/boxlite/sdks/go`, "Go 1.24+ with CGO" — and that
// package does not exist. Checked against the module contents of v0.5.14, v0.8.2,
// v0.9.7, v0.10.8 and v0.11.0: every one ships sdks/{c,node,python} and nothing else,
// the only sdks/go entries in the module zip are end-to-end test programs under
// apps/e2e, and `go get .../sdks/go` fails with "module found, but does not contain
// package". So the choice is not "embed or shell out" — embedding does not exist, and
// a provider that imported it would not compile.
//
// Shelling out is also the shape this project already uses for its container
// provider, at the cost of no new module dependency, no cgo and no Rust toolchain.
//
// # What the engine actually provides
//
// Measured against boxlite 0.11.0 (see the package tests): each box gets its own
// kernel (distinct /proc/sys/kernel/random/boot_id per box, guest Linux 6.12.87), the
// CLI propagates the payload's exit code, and it has
// create/start/exec/cp/list/inspect/logs/stats/rm plus `network inbound|outbound` and
// `--publish`. That backs the capabilities claimed here and not the ones omitted:
//
//   - no `--label` exists anywhere in the CLI reference, so a box carries no ownership
//     mark this provider could later verify. Prune therefore refuses instead of
//     guessing: "not in my process's map" cannot establish abandonment.
//   - no snapshot or clone verb is documented, so CapSnapshot/CapClone/CapRebuild are
//     not claimed and the core will not offer them.
//
// # Platform
//
// The acceptance target is Linux. Nothing here branches on runtime.GOOS, because a
// name is not evidence: Probe asks the engine (`boxlite info` reports
// "virtualization: available") and refuses with a typed error naming what is missing.
// macOS does work — that was measured, and pretending otherwise would repeat the
// mistake this project already made once, when an isolation tier was inferred from a
// provider's name — but it is not what this provider is gated on.
package boxlite

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rexleimo/agno-go/pkg/hno/sandbox"
)

// ProviderName is the registry key.
const ProviderName = "boxlite"

// defaultBin is the executable name. The release installer puts it in
// $HOME/.local/bin/boxlite.
const defaultBin = "boxlite"

// Config configures the provider. Tests build it directly with a fake Runner;
// production arrives through NewProvider(ProviderConfig).
type Config struct {
	// Image is the default image for boxes created without one.
	Image string
	// Bin is the executable to run.
	Bin string
	// Home is BOXLITE_HOME: the runtime data directory. Empty means the engine default.
	Home string
	// Registry lists image registries to try, in order. It exists because the public
	// registry is not reachable from every network, and an engine that cannot pull
	// cannot start a box: measured on the development host, index.docker.io timed out
	// after 25s while a mirror answered 401 (reachable, auth required) and pulled fine.
	Registry []string
	// StartupTimeout bounds create+start. Zero means DefaultStartupTimeout.
	StartupTimeout time.Duration
	// Runner is the command seam. Nil uses the real CLI.
	Runner Runner
}

// DefaultStartupTimeout bounds one box's create+start. Boot measured ~1-2s; the
// ceiling is generous because a first pull can dominate.
const DefaultStartupTimeout = 2 * time.Minute

// Command is one CLI invocation.
type Command struct {
	// Args are the arguments after the binary name.
	Args []string
	// Stdin is fed to the process.
	Stdin string
	// OnChunk, when set, receives output as it arrives. It is what makes streaming a
	// measured fact: Outcome.Streamed counts bytes handed over while the process ran.
	OnChunk func(stream sandbox.Stream, chunk []byte)
}

// Outcome is what one invocation produced.
type Outcome struct {
	Stdout string
	Stderr string
	Exit   int
	// Streamed counts bytes delivered to Command.OnChunk.
	Streamed int64
	// Canceled reports that the context ended the process, which is not a failure.
	Canceled bool
}

// Runner is the command-execution seam.
type Runner interface {
	Run(ctx context.Context, cmd Command) (Outcome, error)
}

// Provider is the boxlite implementation of sandbox.Provider.
type Provider struct {
	cfg    Config
	runner Runner

	// cliMu serialises CLI invocations. The engine takes an exclusive lock on
	// BOXLITE_HOME ("Another BoxliteRuntime is already using directory"), so two
	// overlapping invocations do not queue -- one of them fails. Measured.
	cliMu   sync.Mutex
	mu      sync.Mutex
	created map[string]struct{}
	// skippedReason explains why Prune refuses, so a zero count is never mistaken for
	// "there was nothing to delete".
	skippedReason string
}

// New builds a provider from engine configuration directly.
func New(cfg Config) (*Provider, error) {
	if strings.TrimSpace(cfg.Bin) == "" {
		cfg.Bin = defaultBin
	}
	if len(cfg.Registry) > 0 {
		for i, r := range cfg.Registry {
			if strings.TrimSpace(r) == "" {
				return nil, fmt.Errorf("boxlite: registry entry %d is empty", i)
			}
		}
	}
	if cfg.StartupTimeout == 0 {
		cfg.StartupTimeout = DefaultStartupTimeout
	}
	p := &Provider{cfg: cfg, runner: cfg.Runner, created: map[string]struct{}{}}
	if p.runner == nil {
		p.runner = &execRunner{bin: cfg.Bin, home: cfg.Home}
	}
	return p, nil
}

// knownAttrs lists the engine attributes this provider reads. A key outside this set
// is refused rather than ignored: Raw is the passthrough map, so a typo would
// otherwise read as configuration that took effect while the provider used a default.
var knownAttrs = []string{"bin", "home", "registry"}

// NewProvider adapts the provider to sandbox.ProviderConfig so it can be registered
// as a factory.
func NewProvider(cfg sandbox.ProviderConfig) (sandbox.Provider, error) {
	c := Config{
		Image:          cfg.Image,
		Bin:            defaultBin,
		StartupTimeout: time.Duration(cfg.Timeout),
	}
	if v, ok := cfg.Raw["bin"]; ok {
		s, isStr := v.(string)
		if !isStr || strings.TrimSpace(s) == "" {
			return nil, fmt.Errorf("boxlite: attribute \"bin\" must be a non-empty string")
		}
		c.Bin = s
	}
	if v, ok := cfg.Raw["home"]; ok {
		s, isStr := v.(string)
		if !isStr {
			return nil, fmt.Errorf("boxlite: attribute \"home\" must be a string")
		}
		c.Home = s
	}
	if v, ok := cfg.Raw["registry"]; ok {
		list, err := stringList("registry", v)
		if err != nil {
			return nil, err
		}
		c.Registry = list
	}
	var unknown []string
	for k := range cfg.Raw {
		known := false
		for _, a := range knownAttrs {
			if k == a {
				known = true
				break
			}
		}
		if !known {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return nil, fmt.Errorf("boxlite: unrecognised engine attributes %v (known: %s)", unknown, strings.Join(knownAttrs, ", "))
	}
	return New(c)
}

func stringList(key string, v any) ([]string, error) {
	switch t := v.(type) {
	case []string:
		return t, nil
	case string:
		if strings.TrimSpace(t) == "" {
			return nil, nil
		}
		return []string{t}, nil
	default:
		return nil, fmt.Errorf("boxlite: attribute %q must be a string or a list of strings", key)
	}
}

// Register installs the provider in the default registry. Import it explicitly;
// nothing runs from an init() that scans PATH.
func Register() error {
	if sandbox.HasProvider(ProviderName) {
		return nil
	}
	return sandbox.RegisterProvider(ProviderName, NewProvider)
}

// Name implements sandbox.Provider.
func (p *Provider) Name() string { return ProviderName }

// Isolation implements sandbox.Provider. Each box runs its own kernel, measured rather
// than assumed; the core reconciles this claim against a boundary measurement when a
// caller supplies one (D-39), so an engine that stopped doing it is caught.
func (p *Provider) Isolation() sandbox.Isolation { return sandbox.IsolationMicroVM }

// Capabilities declares only what the CLI surface backs: a session by create/start,
// streaming by exec's foreground stream, egress by `network outbound --allow-net`, port
// forwarding by `--publish`. Snapshot, clone and rebuild are absent because the engine
// documents no such verb; file bridge is absent until a FileService is written over
// `cp`.
func (p *Provider) Capabilities() sandbox.CapabilitySet {
	return sandbox.Capabilities(
		sandbox.CapSession,
		sandbox.CapStreaming,
		sandbox.CapEgressAllowlist,
		sandbox.CapPortForward,
	)
}

// Probe asks the engine two questions a caller cannot answer from a name: is there an
// engine here, and can it virtualise.
//
// It does not pull an image and does not create a box, so it is cheap enough to run on
// every Open. Image presence has its own check (D-36).
func (p *Provider) Probe(ctx context.Context) error {
	res, err := p.run(ctx, Command{Args: []string{"info"}})
	if err != nil {
		return p.unavailable("boxlite CLI could not be run", err,
			"install boxlite (the release installer lands it in $HOME/.local/bin), or point WithAttrs{\"bin\": ...} at the executable")
	}
	if res.Exit != 0 {
		return p.unavailable("boxlite info exited nonzero",
			fmt.Errorf("exit %d: %s", res.Exit, firstLine(res.Stderr)),
			"run `boxlite info` by hand to see what the engine reports")
	}
	available, known := parseVirtualization(res.Stdout)
	switch {
	case !known:
		// Unknown is not a pass: an engine that does not answer cannot be assumed to
		// have hardware virtualisation, and this provider's whole claim is VMs.
		return p.unavailable("boxlite did not report whether virtualisation is available",
			fmt.Errorf("no virtualization line in `boxlite info`: %s", firstLine(res.Stdout)),
			"check `boxlite --version`; this provider expects a CLI that reports virtualization status")
	case !available:
		return p.unavailable("boxlite reports that hardware virtualisation is not available on this host",
			errors.New("virtualization: unavailable"),
			"run on Linux with KVM access, or inside a Linux host that has it; BoxLite needs hardware virtualisation for the micro-VM tier")
	}
	return nil
}

// parseVirtualization reads the `virtualization:` line from `boxlite info`. The third
// return distinguishes "answered no" from "did not answer", because collapsing those
// two turns a silent engine into a passing host.
func parseVirtualization(out string) (available, known bool) {
	for _, ln := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(ln), ":")
		if !ok || !strings.EqualFold(strings.TrimSpace(k), "virtualization") {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "available", "true", "yes":
			return true, true
		default:
			return false, true
		}
	}
	return false, false
}

// ImageAvailable implements sandbox.ImageQueryable in the engine's own vocabulary:
// `boxlite images` prints a table, and a tag that is not in it cannot start.
func (p *Provider) ImageAvailable(ctx context.Context, image string) (bool, error) {
	if strings.TrimSpace(image) == "" {
		return false, fmt.Errorf("boxlite: no image given, so presence cannot be answered")
	}
	res, err := p.run(ctx, Command{Args: []string{"images"}})
	if err != nil {
		return false, fmt.Errorf("boxlite: could not list images: %w", err)
	}
	if res.Exit != 0 {
		return false, fmt.Errorf("boxlite: `images` exited %d: %s", res.Exit, firstLine(res.Stderr))
	}
	return imageListed(res.Stdout, image), nil
}

// imageListed matches "repo tag" pairs in the table, accepting the ways an image
// reference can be written (`alpine`, `alpine:latest`, `library/alpine:latest`).
func imageListed(out, image string) bool {
	wantRepo, wantTag := splitImage(image)
	for _, ln := range strings.Split(out, "\n") {
		if !strings.ContainsAny(ln, "│|") {
			continue // header rule or banner
		}
		fields := strings.Fields(strings.NewReplacer("│", " ", "|", " ").Replace(ln))
		if len(fields) < 2 || strings.EqualFold(fields[0], "REPOSITORY") || strings.EqualFold(fields[0], "NAME") {
			continue
		}
		if sameRepo(fields[0], wantRepo) && fields[1] == wantTag {
			return true
		}
	}
	return false
}

func splitImage(image string) (repo, tag string) {
	repo, tag = image, "latest"
	if i := strings.LastIndex(image, ":"); i > strings.LastIndex(image, "/") {
		repo, tag = image[:i], image[i+1:]
	}
	return strings.TrimPrefix(repo, "docker.io/"), tag
}

func sameRepo(a, b string) bool {
	return stripRegistryHost(a) == stripRegistryHost(b)
}

// stripRegistryHost drops a leading registry hostname and the docker.io/library aliases,
// so "alpine", "library/alpine" and "docker.m.daocloud.io/library/alpine" all compare
// equal. The engine stores whichever name the pull used, and a mirror-qualified name is
// the normal case once a registry is configured.
func stripRegistryHost(s string) string {
	s = strings.TrimPrefix(s, "docker.io/")
	if i := strings.IndexByte(s, '/'); i > 0 {
		host := s[:i]
		if strings.ContainsAny(host, ".:") || host == "localhost" {
			s = s[i+1:]
		}
	}
	return strings.TrimPrefix(s, "library/")
}

// qualifyImage prefixes the configured registry when the reference does not already name
// one, so the mirror survives from create to the pull that happens at start.
func (p *Provider) qualifyImage(image string) string {
	if len(p.cfg.Registry) == 0 || imageHasRegistry(image) {
		return image
	}
	return strings.TrimSuffix(p.cfg.Registry[0], "/") + "/" + image
}

func imageHasRegistry(image string) bool {
	i := strings.IndexByte(image, '/')
	if i < 0 {
		return false
	}
	host := image[:i]
	return strings.ContainsAny(host, ".:") || host == "localhost"
}

// NewEnvironment creates and starts one box.
//
// Create and start are separate calls with separate deadlines for the same reason the
// container provider splits them: a failure between the two still has an id to clean
// up, and a half-created box is exactly the leak reclamation exists to prevent.
func (p *Provider) NewEnvironment(ctx context.Context, spec sandbox.EnvSpec) (sandbox.Environment, error) {
	image := spec.Image
	if image == "" {
		image = p.cfg.Image
	}
	if strings.TrimSpace(image) == "" {
		return nil, fmt.Errorf("boxlite: no image: bind WithImage or set the profile image before creating a box")
	}
	// The image reference must carry the registry. `--registry` is a pull-time flag and
	// does not persist into the box record: measured, a box created with
	// `--registry mirror create alpine:latest` records plain `alpine:latest`, and `start`
	// then tries the default registry and fails. Qualifying the reference is what makes
	// the mirror survive to the pull that actually happens at start.
	image = p.qualifyImage(image)
	args := []string{}
	for _, reg := range p.cfg.Registry {
		args = append(args, "--registry", reg)
	}
	args = append(args, "create")
	if spec.SessionID != "" {
		// A name makes the box recognisable in `boxlite list`, which is the only
		// ownership hint the engine offers. It is a label for humans, not proof.
		args = append(args, "--name", boxName(spec.SessionID))
	}
	for _, kv := range spec.Spec.Env {
		args = append(args, "--env", kv)
	}
	for _, pm := range spec.Expose {
		args = append(args, "--publish", fmt.Sprintf("%d:%d", pm.HostPort, pm.GuestPort))
	}
	if spec.Limits.CPUCount > 0 {
		args = append(args, "--cpus", strconv.Itoa(spec.Limits.CPUCount))
	}
	if spec.Limits.MemoryMB > 0 {
		args = append(args, "--memory", fmt.Sprintf("%dM", spec.Limits.MemoryMB))
	}
	// The box must outlive the create call, so its entrypoint is an idle sleep rather
	// than the caller's command: Exec supplies that per run. `create` has no --detach
	// (it does not start anything — measured: `boxlite create --help` lists --name,
	// --cpus, --memory, --publish, --env, --volume, --rootfs and no detach flag), so
	// passing one would be rejected rather than ignored.
	args = append(args, image, "sleep", "infinity")

	createCtx, cancel := p.bounded(ctx)
	res, err := p.run(createCtx, Command{Args: args})
	createErr := createCtx.Err()
	cancel()
	if err != nil {
		return nil, p.unavailable("boxlite create could not be dispatched", err,
			"check that the engine is responsive: `boxlite info`")
	}
	if res.Exit != 0 {
		return nil, p.unavailable("boxlite create exited nonzero", engineFailure(res, createErr),
			fmt.Sprintf("image %q may be missing or its registry unreachable: pull it explicitly, or set WithAttrs{\"registry\": ...}", image))
	}
	id := strings.TrimSpace(lastNonEmptyLine(res.Stdout))
	if id == "" {
		return nil, fmt.Errorf("boxlite: create returned no box id")
	}

	startCtx, cancelStart := p.bounded(ctx)
	sres, serr := p.run(startCtx, Command{Args: []string{"start", id}})
	startErr := startCtx.Err()
	cancelStart()
	if serr != nil || sres.Exit != 0 {
		// Cleanup must not inherit a deadline that has already passed, or the box we
		// refuse to adopt stays behind as a leak.
		_ = p.remove(context.WithoutCancel(ctx), id)
		if serr != nil {
			return nil, p.unavailable("boxlite start could not be dispatched", serr,
				"run `boxlite list --all` to see what the engine thinks exists")
		}
		return nil, p.unavailable("boxlite start exited nonzero", engineFailure(sres, startErr),
			fmt.Sprintf("box %s was created but would not start; `boxlite logs %s` shows why", id, id))
	}

	p.track(id)
	return &Environment{provider: p, id: id, session: spec.SessionID}, nil
}

// boxName derives a human-readable engine name from the session id. The engine
// validates names, so anything outside its alphabet becomes a hyphen.
func boxName(session string) string {
	var b strings.Builder
	for _, r := range session {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	name := strings.Trim(b.String(), "-.")
	if name == "" {
		name = "hno"
	}
	if len(name) > 48 {
		name = strings.Trim(name[:48], "-.")
	}
	// Sessions the runtime itself names already carry the prefix; measuring produced
	// boxes called hno-hno-boundary-1234-0 until this check existed.
	if strings.HasPrefix(name, "hno-") {
		return name
	}
	return "hno-" + name
}

// applyEgress replaces the box's outbound policy with the given allowlist. It is
// applied per run because the policy is per request, and a failure is fatal to the run:
// continuing would execute under the engine's default egress while the caller believes
// it is restricted.
func (p *Provider) applyEgress(ctx context.Context, id string, allow []string) error {
	args := []string{"network", "outbound", id, "enabled"}
	for _, host := range allow {
		args = append(args, "--allow-net", host)
	}
	res, err := p.run(ctx, Command{Args: args})
	if err != nil {
		return p.unavailable("boxlite network outbound could not be dispatched", err,
			"the box's egress policy is unknown, so the run was not started")
	}
	if res.Exit != 0 {
		return p.unavailable("boxlite network outbound exited nonzero",
			fmt.Errorf("exit %d: %s", res.Exit, firstLine(res.Stderr)),
			"the egress allowlist was not applied, so the run was not started rather than run unrestricted")
	}
	return nil
}

// Close releases provider-level state. Boxes are owned by the caller and closed
// through Environment.Close.
func (p *Provider) Close(ctx context.Context) error {
	p.mu.Lock()
	p.created = map[string]struct{}{}
	p.mu.Unlock()
	return nil
}

func (p *Provider) track(id string) {
	p.mu.Lock()
	p.created[id] = struct{}{}
	p.mu.Unlock()
}

func (p *Provider) untrack(id string) {
	p.mu.Lock()
	delete(p.created, id)
	p.mu.Unlock()
}

// remove deletes a box.
func (p *Provider) remove(ctx context.Context, id string) error {
	// --force is required: `boxlite rm` refuses a running box and this provider deletes
	// before stopping. Measured: without it `rm` reports success-shaped output and the box
	// stays Running, so the leak is silent.
	res, err := p.run(ctx, Command{Args: []string{"rm", "--force", id}})
	if err != nil {
		return p.unavailable("boxlite rm could not be dispatched", err,
			fmt.Sprintf("remove box %s by hand: boxlite rm --force %s", id, id))
	}
	if res.Exit != 0 {
		return p.unavailable("boxlite rm exited nonzero",
			fmt.Errorf("exit %d: %s", res.Exit, firstLine(res.Stderr)),
			fmt.Sprintf("remove box %s by hand: boxlite rm --force %s", id, id))
	}
	p.untrack(id)
	return nil
}

// Sweep reports boxes this process knows it created and that the engine still lists.
// The local map is what this process believes; `boxlite list --all` is what exists.
func (p *Provider) Sweep(ctx context.Context) ([]string, error) {
	live, err := p.listedIDs(ctx)
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	known := make([]string, 0, len(p.created))
	for id := range p.created {
		known = append(known, id)
	}
	p.mu.Unlock()
	out := make([]string, 0, len(known))
	for _, id := range known {
		if _, ok := live[id]; ok {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out, nil
}

// Prune refuses.
//
// The container provider deletes confidently because every box it creates carries a
// label naming this process and the engine answers whether that owner is alive. boxlite
// has no label concept at all — the CLI reference does not mention one — so a box found
// on this host cannot be attributed to us, to another process, or to another tool.
// Deleting on that guess is how a reclaim tool destroys somebody else's work, and "not
// in my process's map" cannot establish abandonment.
//
// The refusal is the answer, it is reported rather than silent, and it is answerable
// through SkippedPrune.
func (p *Provider) Prune(ctx context.Context) ([]string, error) {
	p.mu.Lock()
	p.skippedReason = "boxlite exposes no ownership label, so this provider cannot prove which boxes belong to this process; nothing was deleted"
	p.mu.Unlock()
	return nil, nil
}

// SkippedPrune returns why Prune declined, so a zero count is never mistaken for
// "there was nothing to delete".
func (p *Provider) SkippedPrune() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.skippedReason == "" {
		return nil
	}
	return []string{p.skippedReason}
}

// boxState maps a box id to its engine-reported status from `boxlite list --all`.
func (p *Provider) boxState(ctx context.Context) (map[string]string, error) {
	res, err := p.run(ctx, Command{Args: []string{"list", "--all"}})
	if err != nil {
		return nil, p.unavailable("boxlite list could not be dispatched", err,
			"run `boxlite list --all` by hand to see what exists")
	}
	if res.Exit != 0 {
		return nil, p.unavailable("boxlite list exited nonzero",
			fmt.Errorf("exit %d: %s", res.Exit, firstLine(res.Stderr)),
			"run `boxlite list --all` by hand")
	}
	return parseListStates(res.Stdout), nil
}

func (p *Provider) listedIDs(ctx context.Context) (map[string]string, error) {
	return p.boxState(ctx)
}

// parseListStates reads id→status from the table. It tolerates the box-drawing table,
// a plain one, and an empty table (a host with no boxes is not an error).
func parseListStates(out string) map[string]string {
	states := map[string]string{}
	for _, ln := range strings.Split(out, "\n") {
		if !strings.ContainsAny(ln, "│|") {
			continue
		}
		fields := strings.Fields(strings.NewReplacer("│", " ", "|", " ").Replace(ln))
		if len(fields) == 0 {
			continue
		}
		first := fields[0]
		if strings.EqualFold(first, "ID") || strings.HasPrefix(first, "─") || strings.HasPrefix(first, "-") {
			continue
		}
		status := ""
		if len(fields) > 2 {
			status = fields[2]
		}
		states[first] = status
	}
	return states
}

// run is the only place a CLI invocation is dispatched, so the engine's exclusive
// home-directory lock is respected by construction rather than by every caller
// remembering to.
func (p *Provider) run(ctx context.Context, cmd Command) (Outcome, error) {
	p.cliMu.Lock()
	defer p.cliMu.Unlock()
	return p.runner.Run(ctx, cmd)
}

func (p *Provider) bounded(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, p.cfg.StartupTimeout)
}

// unavailable builds the typed error with remediation.
func (p *Provider) unavailable(detail string, cause error, fixes ...string) error {
	if cause != nil {
		fixes = append(fixes, "engine said: "+firstLine(cause.Error()))
	}
	return &sandbox.UnavailableError{
		Tried:  []sandbox.Attempt{{Provider: ProviderName, Isolation: p.Isolation(), Reason: detail, Fixes: fixes}},
		Want:   sandbox.IsolationMicroVM,
		Reason: detail,
		Fixes:  fixes,
	}
}

func engineFailure(res Outcome, ctxErr error) error {
	if ctxErr != nil {
		return fmt.Errorf("deadline exceeded while the engine worked: %w", ctxErr)
	}
	return fmt.Errorf("exit %d: %s", res.Exit, firstLine(res.Stderr))
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func lastNonEmptyLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if t := strings.TrimSpace(lines[i]); t != "" {
			return t
		}
	}
	return ""
}

// execRunner is the production Runner: it runs the real CLI, streams output when
// asked, and never accumulates more than maxCapture per stream.
type execRunner struct {
	bin  string
	home string
}

// maxCapture bounds one stream held in host memory. The payload's stdout passes
// through this buffer, so an unbounded reader turns "the payload printed 2 GB" into
// "the host allocated 2 GB" — and the configured output ceiling would be reporting a
// limit nothing enforced.
const maxCapture = 8 << 20

func (r *execRunner) Run(ctx context.Context, cmd Command) (Outcome, error) {
	c := exec.CommandContext(ctx, r.bin, cmd.Args...)
	if r.home != "" {
		c.Env = append(os.Environ(), "BOXLITE_HOME="+r.home)
	}
	if cmd.Stdin != "" {
		c.Stdin = strings.NewReader(cmd.Stdin)
	}
	stdoutBuf := &cappedBuffer{limit: maxCapture}
	stderrBuf := &cappedBuffer{limit: maxCapture}
	stdout := &streamWriter{buf: stdoutBuf, stream: sandbox.StreamStdout, onChunk: cmd.OnChunk}
	c.Stdout = stdout
	c.Stderr = stderrBuf
	err := c.Run()
	out := Outcome{
		Stdout:   stdoutBuf.String(),
		Stderr:   stderrBuf.String(),
		Streamed: stdout.Streamed(),
	}
	if ctx.Err() != nil {
		out.Canceled = true
		return out, ctx.Err()
	}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			out.Exit = ee.ExitCode()
			return out, nil
		}
		return out, err
	}
	return out, nil
}
