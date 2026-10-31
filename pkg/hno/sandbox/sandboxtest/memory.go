// Package sandboxtest provides an in-memory sandbox provider that implements
// the full optional-capability surface of pkg/hno/sandbox. It exists so the
// contract can be tested without a hypervisor, a container runtime, or network
// egress — and so downstream code can be tested against realistic lifecycle
// behaviour instead of a stub that ignores it.
package sandboxtest

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/rexleimo/agno-go/pkg/hno/sandbox"
)

// Provider is the in-memory reference provider.
type Provider struct {
	mu       sync.Mutex
	cfg      sandbox.ProviderConfig
	envs     map[string]*Env
	closed   bool
	created  int
	Swept    []string
	ProbeErr error
	// SlowBoot simulates boot latency so timeout behaviour is testable.
	SlowBoot time.Duration
}

// ProbedProvider wraps Provider with in-guest readiness answers.
//
// It is a separate type on purpose. If the default double implemented
// ReadinessProber, no offline test could reach the "this provider cannot look inside
// the guest" path, which is the refusal that keeps an unevaluable probe from reading as
// a passed one — the exact lie this whole area exists to prevent.
type ProbedProvider struct {
	*Provider
	// Ready answers the probe when set.
	Ready func(ctx context.Context, env sandbox.Environment, probe sandbox.ReadyProbe) (bool, string, error)
	// ReadyPorts and ReadyURLs are the declarative form for tests that only want the
	// service to be up.
	ReadyPorts map[int]bool
	ReadyURLs  map[string]bool
}

// NewProvider returns a fresh in-memory provider.
func NewProvider(cfg sandbox.ProviderConfig) (sandbox.Provider, error) {
	return &Provider{cfg: cfg, envs: map[string]*Env{}}, nil
}

// Register installs the in-memory provider under "memory". It is called from
// init, so importing this package for its provider is enough; calling it again
// is a no-op rather than an error, because tests and examples both import it.
func Register() error {
	if sandbox.HasProvider("memory") {
		return nil
	}
	return sandbox.RegisterProvider("memory", NewProvider)
}

// Name implements sandbox.Provider.
func (p *Provider) Name() string { return "memory" }

// Isolation reports syscall-filtered isolation: the highest level an in-memory
// provider can honestly claim (D-02 asks for the real level, not a hopeful one).
func (p *Provider) Isolation() sandbox.Isolation { return sandbox.IsolationContainer }

// Capabilities declares everything the contract exercises.
func (p *Provider) Capabilities() sandbox.CapabilitySet {
	return sandbox.Capabilities(
		sandbox.CapSession, sandbox.CapFileBridge, sandbox.CapStreaming, sandbox.CapBackground,
		sandbox.CapQueryStatus, sandbox.CapSnapshot, sandbox.CapClone, sandbox.CapRebuild,
		sandbox.CapEgressAllowlist, sandbox.CapAudit, sandbox.CapIdleSuspend,
	)
}

// Probe implements sandbox.Provider.
func (p *Provider) Probe(ctx context.Context) error {
	if p.ProbeErr != nil {
		return p.ProbeErr
	}
	return nil
}

// NewEnvironment implements sandbox.Provider.
func (p *Provider) NewEnvironment(ctx context.Context, spec sandbox.EnvSpec) (sandbox.Environment, error) {
	if p.SlowBoot > 0 {
		select {
		case <-time.After(p.SlowBoot):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, errors.New("sandboxtest: provider closed")
	}
	id := spec.SessionID
	if id == "" {
		p.created++
		id = fmt.Sprintf("env-%d", p.created)
	}
	if existing, ok := p.envs[id]; ok {
		return existing, nil
	}
	env := &Env{
		id:        id,
		image:     spec.Image,
		provider:  p,
		files:     map[string][]byte{},
		history:   []string{},
		installed: map[string]string{},
	}
	if spec.Workspace != nil && spec.Workspace.Kind == sandbox.SourceArtifacts {
		for _, a := range spec.Workspace.Artifacts {
			env.files[path.Join("/", spec.Workspace.SubPath, a.Name)] = []byte(a.Name + ":" + a.Path)
		}
	}
	p.envs[id] = env
	return env, nil
}

// Close implements sandbox.Provider.
func (p *Provider) Close(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	return nil
}

// LoadSession implements sandbox.SessionStore.
func (p *Provider) LoadSession(ctx context.Context, id string) (sandbox.SessionRecord, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	env, ok := p.envs[id]
	if !ok {
		return sandbox.SessionRecord{}, false, nil
	}
	return sandbox.SessionRecord{ID: id, Provider: "memory", Image: env.image, Isolation: p.Isolation(), EnvID: id, Spec: env.Spec(), CreatedAt: sandbox.UnixTime{Seconds: time.Now().Unix()}, LastActiveAt: sandbox.UnixTime{Seconds: time.Now().Unix()}}, true, nil
}

// SaveSession implements sandbox.SessionStore.
func (p *Provider) SaveSession(ctx context.Context, rec sandbox.SessionRecord) error { return nil }

// DropSession implements sandbox.SessionStore.
func (p *Provider) DeleteSession(ctx context.Context, id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.envs, id)
	return nil
}

// CreateSnapshot implements sandbox.Snapshotter.
func (p *Provider) Snapshot(ctx context.Context, env sandbox.Environment, name string) (sandbox.SnapshotHandle, error) {
	e, ok := env.(*Env)
	if !ok {
		return sandbox.SnapshotHandle{}, fmt.Errorf("sandboxtest: foreign environment %q", env.ID())
	}
	return e.snapshot(name), nil
}

// RestoreSnapshot implements sandbox.Snapshotter.
func (p *Provider) Restore(ctx context.Context, env sandbox.Environment, snap sandbox.SnapshotHandle) error {
	e, ok := env.(*Env)
	if !ok {
		return fmt.Errorf("sandboxtest: foreign environment %q", env.ID())
	}
	return e.restore(snap.Name)
}

// DeleteSnapshot implements sandbox.Snapshotter.
func (p *Provider) DeleteSnapshot(ctx context.Context, snap sandbox.SnapshotHandle) error { return nil }

// Clone implements sandbox.Cloner.
func (p *Provider) Clone(ctx context.Context, env sandbox.Environment, id string) (sandbox.Environment, error) {
	e, ok := env.(*Env)
	if !ok {
		return nil, fmt.Errorf("sandboxtest: foreign environment %q", env.ID())
	}
	clone := e.clone(id)
	p.mu.Lock()
	p.envs[id] = clone
	p.mu.Unlock()
	return clone, nil
}

// Attach implements sandbox.Attacher.
func (p *Provider) Attach(ctx context.Context, ref sandbox.RunRef, sink sandbox.ChunkReporter) error {
	e, ok := p.env(ref.SessionID)
	if !ok {
		return &sandbox.StaleSessionError{SessionID: ref.SessionID, Provider: "memory"}
	}
	return e.reattach(ref.ID, sink)
}

// Sweep implements sandbox.LeaseSweeper.
func (p *Provider) Sweep(ctx context.Context) ([]string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var orphans []string
	for id, e := range p.envs {
		// Anything still in the live map that got closed without being dropped
		// is exactly the residue this call exists to find.
		e.mu.Lock()
		dead := e.destroyed
		e.mu.Unlock()
		if dead {
			orphans = append(orphans, id)
		}
	}
	p.Swept = append(p.Swept, orphans...)
	return orphans, nil
}

// env returns a live environment by id.
func (p *Provider) env(id string) (*Env, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.envs[id]
	return e, ok
}

// Env is an in-memory execution environment.
type Env struct {
	mu        sync.Mutex
	id        string
	image     string
	provider  *Provider
	files     map[string][]byte
	history   []string
	installed map[string]string
	spec      sandbox.SessionSpec
	snapshots map[string]*snapshot
	nextSnap  int
	destroyed bool
	suspended bool
	runs      map[string]*runRecord
	seq       int
}

// snapshot captures a workspace state.
type snapshot struct {
	files map[string][]byte
	spec  sandbox.SessionSpec
}

// runRecord backs detached runs and reattach.
type runRecord struct {
	mu     sync.Mutex
	chunks []sandbox.OutputChunk
	done   chan struct{}
}

// recorded returns a copy of the chunks the background run pushed.
func (r *runRecord) recorded() []sandbox.OutputChunk {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]sandbox.OutputChunk, len(r.chunks))
	copy(out, r.chunks)
	return out
}

// ID implements sandbox.Environment.
func (e *Env) ID() string { return e.id }

// Spec returns the replayable spec recorded so far.
func (e *Env) Spec() sandbox.SessionSpec {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.spec.Clone()
}

// Exec implements sandbox.Environment.
func (e *Env) Exec(ctx context.Context, spec sandbox.RunSpec, sink sandbox.ChunkReporter) (*sandbox.Report, error) {
	e.mu.Lock()
	if e.destroyed {
		e.mu.Unlock()
		return nil, &sandbox.LostError{RunID: spec.RunID, SessionID: e.id, Provider: "memory"}
	}
	e.seq++
	e.mu.Unlock()

	req := spec.Request
	started := time.Now()
	rep := &sandbox.Report{RunID: spec.RunID, SessionID: e.id, Provider: "memory", Isolation: sandbox.IsolationContainer}

	// Enforce egress before anything else: a denied host must look like a
	// refusal, not a network error (§6.3).
	if hosts := unauthorizedHosts(req, e.provider.cfg); len(hosts) > 0 {
		rep.ExitCode = -1
		rep.BlockedHosts = hosts
		rep.Duration = time.Since(started)
		err := &sandbox.DeniedError{Policy: "sandboxtest/egress", Reason: "host not in allowlist", Rule: strings.Join(hosts, ",")}
		rep.Failure = sandbox.AsFailure(err)
		return rep, err
	}

	if req.Background {
		rec := &runRecord{done: make(chan struct{})}
		e.mu.Lock()
		if e.runs == nil {
			e.runs = map[string]*runRecord{}
		}
		e.runs[spec.RunID] = rec
		e.mu.Unlock()
		go func() {
			defer close(rec.done)
			for i := 0; i < 3; i++ {
				ch := sandbox.OutputChunk{RunID: spec.RunID, Stream: sandbox.StreamStdout, Bytes: []byte(fmt.Sprintf("bg %d\n", i)), At: time.Now().UTC()}
				// The chunk goes through the sink like a real provider would, so a
				// ReadyProbe matched on run output sees it. Recording only into the
				// provider's own buffer makes this double lie in exactly the direction
				// a dishonest provider lies, and no offline test could notice.
				rec.mu.Lock()
				rec.chunks = append(rec.chunks, ch)
				rec.mu.Unlock()
				if sink != nil {
					_ = sink.Report(ch)
				}
			}
		}()
		rep.RunRef = &sandbox.RunRef{ID: spec.RunID, SessionID: e.id}
		rep.Duration = time.Since(started)
		return rep, nil
	}

	out, stderr, code, err := e.interpret(ctx, spec, sink)
	rep.Stdout = out
	rep.Stderr = stderr
	rep.ExitCode = code
	rep.Duration = time.Since(started)
	if err != nil {
		rep.Failure = sandbox.AsFailure(err)
		return rep, err
	}
	if code != 0 {
		rep.Failure = &sandbox.Failure{Class: sandbox.ClassUserCode, NextAction: sandbox.ActionModelFix, Cause: fmt.Errorf("exit code %d", code)}
	}
	return rep, nil
}

// Close implements sandbox.Environment. A closed environment leaves the
// provider's live set: that is what lets Sweep answer "is anything still
// running?" without a second source of truth (§15).
func (e *Env) Close(ctx context.Context) error {
	e.mu.Lock()
	e.destroyed = true
	e.mu.Unlock()
	e.provider.mu.Lock()
	defer e.provider.mu.Unlock()
	if e.provider.envs[e.id] == e {
		delete(e.provider.envs, e.id)
	}
	return nil
}

// interpret executes the tiny "language" this provider understands. Each shape
// maps onto a real provider concern so contract tests mean something.
func (e *Env) interpret(ctx context.Context, spec sandbox.RunSpec, sink sandbox.ChunkReporter) (string, string, int, error) {
	req := spec.Request
	e.mu.Lock()
	defer e.mu.Unlock()

	// inline file inputs materialise first
	for p, f := range req.Files {
		e.files["/"+strings.TrimPrefix(p, "/")] = append([]byte(nil), f.Content...)
	}
	if req.Workspace != nil && req.Workspace.Kind == sandbox.SourceArtifacts {
		for _, a := range req.Workspace.Artifacts {
			e.files["/"+strings.TrimPrefix(req.Workspace.SubPath+"/"+a.Name, "/")] = []byte(a.Name + ":" + a.Path)
		}
	}

	code := strings.TrimSpace(req.Code)
	switch {
	case code == "":
		// argv-style run: installs are recorded into the replayable spec so
		// rebuild can reproduce them (§17.1).
		if len(req.Argv) > 0 {
			e.recordInstallLocked(req.Argv)
		}
		return "", "", 0, nil

	case strings.HasPrefix(code, "#!echo "):
		line := strings.TrimPrefix(code, "#!echo ")
		if sink != nil && spec.Streaming {
			for _, part := range strings.Split(line, "\n") {
				if err := sink.Report(sandbox.OutputChunk{Stream: sandbox.StreamStdout, Bytes: []byte(part + "\n"), At: time.Now().UTC()}); err != nil {
					return "", "", -1, err
				}
			}
		}
		return line + "\n", "", 0, nil

	case strings.HasPrefix(code, "#!fail "):
		// `#!fail <exit> <message>` writes message to stderr and exits with the
		// given code. It exists because #!exit produces a fixed message, which is
		// no use for asserting that a phase keeps the *payload's own* diagnostic —
		// the thing a model has to read in order to fix its code (§16.4).
		rest := strings.TrimPrefix(code, "#!fail ")
		raw, msg, found := strings.Cut(rest, " ")
		if !found {
			return "", "no message given", 1, nil
		}
		var n int
		fmt.Sscanf(raw, "%d", &n)
		if n == 0 {
			n = 1
		}
		return "", msg + "\n", n, nil

	case strings.HasPrefix(code, "#!exit "):
		var n int
		fmt.Sscanf(strings.TrimPrefix(code, "#!exit "), "%d", &n)
		return "", "exited on request\n", n, nil

	case strings.HasPrefix(code, "#!timeout"):
		return "", "", -1, context.DeadlineExceeded

	case strings.HasPrefix(code, "#!lost"):
		return "", "", -1, &sandbox.LostError{RunID: spec.RunID, SessionID: e.id, Provider: "memory"}

	case strings.HasPrefix(code, "#!write "):
		rest := strings.TrimPrefix(code, "#!write ")
		p, body, _ := strings.Cut(rest, " ")
		e.files["/"+strings.TrimPrefix(p, "/")] = []byte(body)
		return "", "", 0, nil

	case strings.HasPrefix(code, "#!panic"):
		panic("sandboxtest: deliberate provider panic")

	case strings.HasPrefix(code, "#!cat "):
		p := strings.TrimPrefix(code, "#!cat ")
		data, ok := e.files["/"+strings.TrimPrefix(p, "/")]
		if !ok {
			return "", "no such file\n", 1, nil
		}
		return string(data), "", 0, nil
	}

	// Anything else is treated as user code that fails, which mirrors what a
	// real runtime reports and keeps ClassUserCode honest.
	return "", "syntax error near unexpected token\n", 2, nil
}

// recordInstallLocked records an argv invocation as a replayable step.
func (e *Env) recordInstallLocked(argv []string) {
	if len(argv) == 0 {
		return
	}
	step := sandbox.Step{Kind: sandbox.StepInstall, Tool: argv[0], Args: append([]string(nil), argv[1:]...)}
	step.Sequence = len(e.spec.Steps) + 1
	step.Digest = digestOf(argv)
	e.spec.Steps = append(e.spec.Steps, step)
	e.history = append(e.history, strings.Join(argv, " "))
	e.installed[strings.Join(argv, " ")] = step.Digest
}

// AddSpec records a step the caller declared through the session API.
func (e *Env) AddSpec(step sandbox.Step) {
	e.mu.Lock()
	defer e.mu.Unlock()
	step.Sequence = len(e.spec.Steps) + 1
	e.spec.Steps = append(e.spec.Steps, step)
}

// File returns a workspace file's content.
func (e *Env) File(p string) ([]byte, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	data, ok := e.files["/"+strings.TrimPrefix(p, "/")]
	return data, ok
}

// snapshot takes a workspace snapshot.
func (e *Env) snapshot(name string) sandbox.SnapshotHandle {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.snapshots == nil {
		e.snapshots = map[string]*snapshot{}
	}
	if name == "" {
		e.nextSnap++
		name = fmt.Sprintf("snap-%d", e.nextSnap)
	}
	cp := map[string][]byte{}
	for k, v := range e.files {
		cp[k] = append([]byte(nil), v...)
	}
	e.snapshots[name] = &snapshot{files: cp, spec: e.spec.Clone()}
	return sandbox.SnapshotHandle{ID: e.id + "/" + name, SessionID: e.id, Name: name, Bytes: int64(len(cp))}
}

// restore rolls the workspace back to a snapshot.
func (e *Env) restore(name string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	snap, ok := e.snapshots[name]
	if !ok {
		return fmt.Errorf("sandboxtest: unknown snapshot %q", name)
	}
	e.files = map[string][]byte{}
	for k, v := range snap.files {
		e.files[k] = append([]byte(nil), v...)
	}
	e.spec = snap.spec.Clone()
	return nil
}

// clone produces an independent environment with the same state.
func (e *Env) clone(id string) *Env {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := &Env{
		id:        id,
		image:     e.image,
		provider:  e.provider,
		files:     map[string][]byte{},
		installed: map[string]string{},
		spec:      e.spec.Clone(),
	}
	for k, v := range e.files {
		out.files[k] = append([]byte(nil), v...)
	}
	return out
}

// reattach replays recorded output for a detached run (§5.4).
func (e *Env) reattach(runID string, sink sandbox.ChunkReporter) error {
	e.mu.Lock()
	rec, ok := e.runs[runID]
	e.mu.Unlock()
	if !ok {
		return fmt.Errorf("sandboxtest: no detached run %q", runID)
	}
	<-rec.done
	for _, c := range rec.recorded() {
		if err := sink.Report(c); err != nil {
			return err
		}
	}
	return nil
}

// unauthorizedHosts checks egress against the request's allowlist.
func unauthorizedHosts(req sandbox.Request, cfg sandbox.ProviderConfig) []string {
	if req.Egress.Mode != sandbox.EgressAllowlist {
		return nil
	}
	allowed := map[string]bool{}
	for _, h := range req.Egress.Allow {
		allowed[strings.ToLower(h)] = true
	}
	var blocked []string
	for _, h := range req.Egress.Requested {
		if !allowed[strings.ToLower(h)] {
			blocked = append(blocked, h)
		}
	}
	sort.Strings(blocked)
	return blocked
}

// digestOf hashes argv for step identity.
func digestOf(argv []string) string {
	sum := sha256.Sum256([]byte(strings.Join(argv, "\x00")))
	return hex.EncodeToString(sum[:])[:16]
}

// Files returns the workspace view for this environment.
func (p *Provider) Files(ctx context.Context, env sandbox.Environment) (sandbox.FileService, error) {
	e, ok := env.(*Env)
	if !ok {
		return nil, fmt.Errorf("sandboxtest: foreign environment %q", env.ID())
	}
	return &fileService{env: e}, nil
}

// fileService is the in-memory workspace view.
type fileService struct{ env *Env }

// Write implements sandbox.FileService.
func (f *fileService) Write(ctx context.Context, p string, content []byte, mode uint32) error {
	f.env.mu.Lock()
	defer f.env.mu.Unlock()
	f.env.files["/"+strings.TrimPrefix(p, "/")] = append([]byte(nil), content...)
	return nil
}

// Read implements sandbox.FileService.
func (f *fileService) Read(ctx context.Context, p string) ([]byte, error) {
	f.env.mu.Lock()
	defer f.env.mu.Unlock()
	data, ok := f.env.files["/"+strings.TrimPrefix(p, "/")]
	if !ok {
		return nil, sandbox.ErrFileNotFound
	}
	return append([]byte(nil), data...), nil
}

// Open implements sandbox.FileService.
func (f *fileService) Open(ctx context.Context, p string, opts sandbox.OpenOptions) (io.ReadWriteCloser, error) {
	data, err := f.Read(ctx, p)
	if err != nil {
		return nil, err
	}
	return &handle{data: data}, nil
}

// Stat implements sandbox.FileService.
func (f *fileService) Stat(ctx context.Context, p string) (sandbox.FileInfo, error) {
	f.env.mu.Lock()
	defer f.env.mu.Unlock()
	key := "/" + strings.TrimPrefix(p, "/")
	if data, ok := f.env.files[key]; ok {
		return sandbox.FileInfo{Path: strings.TrimPrefix(p, "/"), Size: int64(len(data)), ModTime: time.Now().UTC()}, nil
	}
	prefix := strings.TrimSuffix(key, "/") + "/"
	for k := range f.env.files {
		if strings.HasPrefix(k, prefix) {
			return sandbox.FileInfo{Path: strings.TrimSuffix(strings.TrimPrefix(p, "/"), "/"), Dir: true, ModTime: time.Now().UTC()}, nil
		}
	}
	return sandbox.FileInfo{}, sandbox.ErrFileNotFound
}

// List implements sandbox.FileService.
func (f *fileService) List(ctx context.Context, dir string) ([]sandbox.FileInfo, error) {
	f.env.mu.Lock()
	defer f.env.mu.Unlock()
	prefix := "/" + strings.TrimPrefix(dir, "/")
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	var out []sandbox.FileInfo
	seen := map[string]bool{}
	for k, v := range f.env.files {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		rel := strings.TrimPrefix(k, prefix)
		if rel == "" || strings.Contains(rel, "/") {
			continue
		}
		if seen[rel] {
			continue
		}
		seen[rel] = true
		out = append(out, sandbox.FileInfo{Path: strings.TrimSuffix(strings.TrimPrefix(dir, "/"), "/") + "/" + rel, Size: int64(len(v)), ModTime: time.Now().UTC()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	if len(out) == 0 {
		return nil, sandbox.ErrFileNotFound
	}
	return out, nil
}

// Remove implements sandbox.FileService.
func (f *fileService) Remove(ctx context.Context, p string) error {
	f.env.mu.Lock()
	defer f.env.mu.Unlock()
	delete(f.env.files, "/"+strings.TrimPrefix(p, "/"))
	return nil
}

// MkdirAll implements sandbox.FileService.
func (f *fileService) MkdirAll(ctx context.Context, dir string, mode uint32) error { return nil }

// CopyIn implements sandbox.FileService. Bulk project input belongs to
// Request.Workspace; this exists for ad-hoc pushes.
func (f *fileService) CopyIn(ctx context.Context, dir string, r io.Reader) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	key := "/" + strings.TrimPrefix(dir, "/")
	f.env.mu.Lock()
	defer f.env.mu.Unlock()
	f.env.files[key] = data
	return nil
}

// CopyOut implements sandbox.FileService by streaming the subtree as tar.
func (f *fileService) CopyOut(ctx context.Context, dir string, w io.Writer) error {
	entries, err := f.List(ctx, dir)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(w)
	defer tw.Close()
	for _, e := range entries {
		data, err := f.Read(ctx, e.Path)
		if err != nil {
			return err
		}
		hdr := &tar.Header{Name: path.Base(e.Path), Mode: 0o644, Size: int64(len(data))}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if _, err := tw.Write(data); err != nil {
			return err
		}
	}
	return nil
}

// Status implements sandbox.Attacher for detached runs (§5.4).
func (p *Provider) Status(ctx context.Context, ref sandbox.RunRef) (sandbox.RunState, *sandbox.Report, error) {
	e, ok := p.env(ref.SessionID)
	if !ok {
		return sandbox.RunStateUnknown, nil, &sandbox.StaleSessionError{SessionID: ref.SessionID, Provider: "memory"}
	}
	e.mu.Lock()
	rec, ok := e.runs[ref.ID]
	e.mu.Unlock()
	if !ok {
		return sandbox.RunStateUnknown, nil, fmt.Errorf("sandboxtest: unknown run %q", ref.ID)
	}
	select {
	case <-rec.done:
		return sandbox.RunStateDone, &sandbox.Report{RunID: ref.ID, SessionID: ref.SessionID, ExitCode: 0}, nil
	default:
		return sandbox.RunStateSubmitted, nil, nil
	}
}

// Suspend implements sandbox.Attacher.
func (p *Provider) Suspend(ctx context.Context, env sandbox.Environment) error {
	e, ok := env.(*Env)
	if !ok {
		return fmt.Errorf("sandboxtest: foreign environment %q", env.ID())
	}
	e.suspended = true
	return nil
}

// Resume implements sandbox.Attacher.
func (p *Provider) Resume(ctx context.Context, id string) (sandbox.Environment, error) {
	e, ok := p.env(id)
	if !ok {
		return nil, &sandbox.StaleSessionError{SessionID: id, Provider: "memory"}
	}
	e.suspended = false
	return e, nil
}

// Leases implements sandbox.Attacher.
func (p *Provider) Leases(ctx context.Context) ([]string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var ids []string
	for id := range p.envs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

// handle is a read handle over in-memory bytes.
type handle struct {
	data []byte
	pos  int
}

// Read implements io.Reader.
func (h *handle) Read(p []byte) (int, error) {
	if h.pos >= len(h.data) {
		return 0, io.EOF
	}
	n := copy(p, h.data[h.pos:])
	h.pos += n
	return n, nil
}

// Close implements io.ReadWriteCloser.
func (h *handle) Close() error { return nil }

// Write makes the handle writable so callers can stream into a workspace file.
func (h *handle) Write(p []byte) (int, error) {
	h.data = append(h.data[:h.pos], p...)
	h.pos += len(p)
	return len(p), nil
}

// Compile-time interface assertions keep the provider honest: if the contract
// changes, this file fails to build rather than silently under-implementing it.
var (
	_ sandbox.Provider     = (*Provider)(nil)
	_ sandbox.Environment  = (*Env)(nil)
	_ sandbox.SessionStore = (*Provider)(nil)
	_ sandbox.Snapshotter  = (*Provider)(nil)
	_ sandbox.Cloner       = (*Provider)(nil)
	_ sandbox.Attacher     = (*Provider)(nil)
	_ sandbox.LeaseSweeper = (*Provider)(nil)
	_ sandbox.FileBridge   = (*Provider)(nil)
	_ sandbox.FileService  = (*fileService)(nil)
	_ io.ReadWriteCloser   = (*handle)(nil)
)

// ProbeReady implements sandbox.ReadinessProber for the in-memory provider.
func (p *ProbedProvider) ProbeReady(ctx context.Context, env sandbox.Environment, probe sandbox.ReadyProbe) (bool, string, error) {
	if p.Ready != nil {
		return p.Ready(ctx, env, probe)
	}
	if probe.Port != 0 {
		if p.ReadyPorts[probe.Port] {
			return true, "", nil
		}
		return false, fmt.Sprintf("nothing listening on port %d", probe.Port), nil
	}
	if probe.URL != "" {
		if p.ReadyURLs[probe.URL] {
			return true, "", nil
		}
		return false, "GET " + probe.URL + " returned no response", nil
	}
	return false, "", fmt.Errorf("sandboxtest: no in-guest condition to check for %+v", probe)
}

var _ sandbox.ReadinessProber = (*ProbedProvider)(nil)
