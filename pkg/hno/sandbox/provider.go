package sandbox

import (
	"context"
	"fmt"
	"sort"
	"sync"
)

// Provider is the one interface an engine implements. The core never imports an
// engine SDK, and an engine never sees a profile: it receives an already
// validated, already defaulted RunSpec (§3).
type Provider interface {
	// Name is the stable provider id used in Status, events and errors.
	Name() string
	// Isolation is the strength this provider claims. Providers must not
	// over-claim: the core records this verbatim in every report.
	Isolation() Isolation
	// Capabilities is the feature list. Optional interfaces may be implemented
	// too, but only what is listed here is reachable.
	Capabilities() CapabilitySet
	// Probe reports whether the provider can run at all, with a
	// *UnavailableError-shaped reason when it cannot.
	Probe(ctx context.Context) error
	// NewEnvironment creates or resumes an environment for a session. One-shot
	// runs pass SessionID == "".
	NewEnvironment(ctx context.Context, spec EnvSpec) (Environment, error)
	// Close releases provider-level resources owned by this process.
	Close(ctx context.Context) error
}

// EnvSpec is the resolved, validated description of an environment. It carries
// no caller-supplied privilege: the core applied policy and limits already.
type EnvSpec struct {
	SessionID string
	Image     string
	// Workspace is the declared working tree to materialise at creation.
	Workspace   *WorkspaceSource
	Isolation   Isolation
	Spec        SessionSpec
	MountMode   MountMode
	Budget      Budget
	Limits      Limits
	ReplayFrom  []Step // steps that still need replaying after cache lookup
	CachedSteps []string
	Labels      map[string]string
	// Expose lists the guest ports the caller asked to publish. It arrives here rather
	// than only in RunSpec.Request because a provider has to act on it at create:
	// anything else would silently discard the request.
	Expose []PortMap
}

// Environment is a live sandbox the provider manages: one micro-VM, one gVisor
// container, one hosted sandbox. It is the execution substrate of both Run and
// Session.
type Environment interface {
	// ID is the engine-side identifier, empty for ephemeral one-shot
	// environments.
	ID() string
	// Exec runs one request inside this environment and returns a populated
	// Report. It must fill Stdout/Stderr/ExitCode/Duration and, when the
	// provider declares CapStreaming, push chunks to the supplied sink.
	Exec(ctx context.Context, req RunSpec, sink ChunkReporter) (*Report, error)
	// Close stops the environment. Idempotent.
	Close(ctx context.Context) error
}

// RunSpec is the frozen, post-policy description of one execution handed to a
// provider. It exists so providers never see a Policy, a caller identity, or an
// extension point.
type RunSpec struct {
	RunID      string
	SessionID  string
	Request    Request
	Workspace  *WorkspaceSource
	MountMode  MountMode
	Budget     Budget
	Limits     Limits
	Streaming  bool
	Background bool
}

// ChunkReporter is how a provider pushes output into the core's sink fan-out.
// It returns a *HookFaultError when a sink configured OverflowFault.
type ChunkReporter interface {
	Report(chunk OutputChunk) error
}

// SessionStore is the optional persistence contract: the core records session
// state, idempotency entries and checkpoints through it (implemented over the
// existing store/session persistence in pkg/hno by an adapter).
type SessionStore interface {
	SaveSession(ctx context.Context, rec SessionRecord) error
	LoadSession(ctx context.Context, id string) (SessionRecord, bool, error)
	DeleteSession(ctx context.Context, id string) error
}

// SessionRecord is what survives a host restart (§15).
type SessionRecord struct {
	ID           string
	Provider     string
	Image        string
	Isolation    Isolation
	EnvID        string
	Spec         SessionSpec
	CreatedAt    UnixTime
	LastActiveAt UnixTime
	TTLSeconds   int64
}

// UnixTime keeps the record serialisable without importing time in storage
// adapters; the core converts on both ends.
type UnixTime struct{ Seconds int64 }

// Now returns the wall clock this record was touched at.
func (t UnixTime) Now() int64 { return t.Seconds }

// Snapshotter is implemented by providers declaring CapSnapshot (§17, §15).
type Snapshotter interface {
	Snapshot(ctx context.Context, env Environment, name string) (SnapshotHandle, error)
	Restore(ctx context.Context, env Environment, handle SnapshotHandle) error
	DeleteSnapshot(ctx context.Context, handle SnapshotHandle) error
}

// SnapshotHandle identifies one environment snapshot.
type SnapshotHandle struct {
	ID        string
	SessionID string
	Name      string
	Bytes     int64
}

// Cloner is implemented by providers declaring CapClone (§15).
type Cloner interface {
	Clone(ctx context.Context, env Environment, as string) (Environment, error)
}

// Attacher is implemented by providers declaring CapQueryStatus /
// CapBackground (§15).
type Attacher interface {
	Status(ctx context.Context, ref RunRef) (RunState, *Report, error)
	Attach(ctx context.Context, ref RunRef, sink ChunkReporter) error
	Suspend(ctx context.Context, env Environment) error
	Resume(ctx context.Context, id string) (Environment, error)
	Leases(ctx context.Context) ([]string, error)
}

// FileBridge is implemented by providers declaring CapFileBridge (§5, D-05).
type FileBridge interface {
	Files(ctx context.Context, env Environment) (FileService, error)
}

// providerRegistry is the process-wide table of engine factories keyed by name,
// so providers are wired by explicit imports (D-03) and never discovered by
// scanning PATH.
type providerRegistry struct {
	mu        sync.RWMutex
	factories map[string]ProviderFactory
}

// ProviderFactory builds one provider instance from a resolved config.
type ProviderFactory func(cfg ProviderConfig) (Provider, error)

// ProviderConfig is the engine-facing subset of the resolved sandbox config:
// enough to connect, nothing else.
type ProviderConfig struct {
	Name      string
	Image     string
	Formation Formation
	Endpoint  string
	Timeout   Duration
	Labels    map[string]string
	Raw       map[string]any // engine-specific passthrough, opaque to core
}

// Duration is an int64 nanosecond span kept serialisable for adapters.
type Duration int64

// NewProviderRegistry creates an empty registry.
func NewProviderRegistry() *providerRegistry {
	return &providerRegistry{factories: map[string]ProviderFactory{}}
}

// Register adds a factory under a unique name.
func (r *providerRegistry) Register(name string, f ProviderFactory) error {
	if name == "" {
		return fmt.Errorf("sandbox: provider factory name must not be empty")
	}
	if f == nil {
		return fmt.Errorf("sandbox: provider factory must not be nil")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.factories[name]; exists {
		return fmt.Errorf("sandbox: provider factory %q already registered", name)
	}
	r.factories[name] = f
	return nil
}

// has reports whether a name is registered.
func (r *providerRegistry) has(name string) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.factories[name]
	return ok
}

// remove drops a registration (tests only).
func (r *providerRegistry) remove(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.factories, name)
}

// Names lists registered factories, sorted for deterministic selection
// diagnostics.
func (r *providerRegistry) Names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.factories))
	for name := range r.factories {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// build instantiates a provider by name.
func (r *providerRegistry) build(cfg ProviderConfig) (Provider, error) {
	r.mu.RLock()
	f, ok := r.factories[cfg.Name]
	r.mu.RUnlock()
	if !ok {
		return nil, &UnavailableError{Reason: "no provider factory named " + cfg.Name, Tried: []Attempt{{Provider: cfg.Name, Reason: "not registered", Fix: "import a provider package, e.g. pkg/hno/sandbox/provider/boxlite"}}}
	}
	return f(cfg)
}

// DefaultRegistry is the process-wide registry Open consults.
var DefaultRegistry = NewProviderRegistry()

// RegisterProvider adds an engine factory to the default registry. Engines call
// this from an explicit constructor package, never from an init() that scans
// PATH (D-03).
func RegisterProvider(name string, f ProviderFactory) error { return DefaultRegistry.Register(name, f) }

// HasProvider reports whether a name is registered. It lets an initialization
// path be idempotent instead of pretending a repeat registration failed.
func HasProvider(name string) bool { return DefaultRegistry.has(name) }

// UnregisterProvider removes a registration. Tests only: swapping engines
// mid-process is how the wrong box gets used.
func UnregisterProvider(name string) { DefaultRegistry.remove(name) }

// Providers lists registered provider names in a stable order.
func Providers() []string { return DefaultRegistry.Names() }
