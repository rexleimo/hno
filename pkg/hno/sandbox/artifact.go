package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"
	"time"
)

// Key is the identity of a workspace source for cache and journal purposes:
// identical declarations produce identical keys regardless of slice ordering,
// which is what makes a step cache useful (§17.2).
func (s *WorkspaceSource) Key() string {
	if s == nil {
		return "none"
	}
	return shaHex(fmt.Sprintf("%d|%s|%s|%s|%s|%s|%s", s.Kind, s.Dir, s.Git.URL+"@"+s.Git.Ref, s.Image, s.SubPath, s.Git.TokenRef, joinSorted(s.Ignore)))
}

// KeyForSource is the exported form providers use when building cache keys.
func KeyForSource(src *WorkspaceSource) string { return src.Key() }

// cloneSource deep-copies a workspace source. A stream-backed source shares its
// reader on purpose: re-reading a one-shot stream is the caller's problem and
// silently copying it would hide that.
func cloneSource(src *WorkspaceSource) *WorkspaceSource {
	if src == nil {
		return nil
	}
	out := *src
	out.Ignore = append([]string(nil), src.Ignore...)
	if src.Git.Subdirs != nil {
		out.Git.Subdirs = append([]string(nil), src.Git.Subdirs...)
	}
	if src.Artifacts != nil {
		out.Artifacts = append(ArtifactSet(nil), src.Artifacts...)
	}
	return &out
}

// joinSorted sorts before joining so ordering never changes a digest.
func joinSorted(items []string) string {
	cp := append([]string(nil), items...)
	sort.Strings(cp)
	var b strings.Builder
	for _, v := range cp {
		b.WriteString(v)
		b.WriteByte(0)
	}
	return b.String()
}

// shaHex renders a short sha256 digest for step and payload identities.
func shaHex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:16]
}

// workspaceLabel describes a workspace source without absolute host paths,
// which is what makes an audit record safe to ship (§15.3).
func workspaceLabel(src *WorkspaceSource) string {
	switch {
	case src == nil:
		return ""
	case src.Kind == SourceGit:
		return src.Git.URL + "@" + src.Git.Ref
	case src.Kind == SourceImage:
		return src.Image
	default:
		return fmt.Sprintf("%d:%s", int(src.Kind), src.SubPath)
	}
}

// collect gathers declared artefacts into the report. Required artefacts that
// are missing or oversized fail the run; optional ones are delivered truncated
// and flagged, because a silent half-result is worse than a visible one (§13.3,
// D-17).
func (sb *Sandbox) collect(ctx context.Context, rc *RunContext, env Environment, rep *Report) error {
	if len(rc.Request.Outputs) == 0 || rep == nil {
		return nil
	}
	fs, err := sb.filesOf(ctx, env)
	if err != nil {
		return err
	}
	sb.emit(ctx, Event{Kind: EventCollecting, RunID: rc.RunID, SessionID: rc.SessionID})
	c := newCollector(fs, rc.Limits)
	total := int64(0)
	// Two specs may legitimately cover the same file ("dist" as a dir plus
	// "dist/*.js" as a glob). Counting it twice would double-bill the total byte
	// budget and hand the caller the same artefact under two names.
	seen := map[string]bool{}
	for _, spec := range rc.Request.Outputs {
		found, size, err := c.collectOne(ctx, spec, func(a Artifact) {
			key := a.Path
			if key == "" {
				key = a.Name
			}
			if seen[key] {
				return
			}
			seen[key] = true
			rep.Artifacts = append(rep.Artifacts, a)
		})
		if err != nil {
			return err
		}
		total += size
		if !found && spec.Required {
			return &ArtifactError{Spec: spec, Reason: "missing"}
		}
	}
	if limit := rc.Limits.ArtifactTotalMaxBytes; limit > 0 && total > limit {
		return &ArtifactError{Spec: ArtifactSpec{Path: "*", MaxBytes: limit}, Reason: "too-large", Found: total, Limit: limit}
	}
	if len(rep.Artifacts) > 0 {
		sb.emit(ctx, Event{Kind: EventArtifactsCollected, RunID: rc.RunID, SessionID: rc.SessionID, Detail: map[string]any{"count": len(rep.Artifacts), "names": rep.Artifacts.Names()}})
	}
	if len(c.dropped) > 0 {
		sb.emit(ctx, Event{Kind: EventArtifactsCollected, RunID: rc.RunID, Detail: map[string]any{"dropped": c.dropped}})
	}
	return nil
}

// envFileSource is the narrow file surface collection needs. Providers either
// satisfy it through FileBridge or get a typed capability error.
type envFileSource interface {
	Stat(ctx context.Context, p string) (FileInfo, error)
	List(ctx context.Context, dir string) ([]FileInfo, error)
	Read(ctx context.Context, p string) ([]byte, error)
}

// collector turns declared specs into readable handles within a byte budget.
type collector struct {
	fs      envFileSource
	limits  Limits
	dropped []string
}

// newCollector builds a collector for one run.
func newCollector(fs envFileSource, limits Limits) *collector {
	return &collector{fs: fs, limits: limits}
}

// collectOne gathers one spec and reports whether anything was found plus the
// bytes it accounted for.
func (c *collector) collectOne(ctx context.Context, spec ArtifactSpec, add func(Artifact)) (bool, int64, error) {
	kind := inferKind(spec)
	limit := spec.MaxBytes
	if limit == 0 {
		limit = c.limits.ArtifactMaxBytes
	}
	switch kind {
	case KindDir:
		return c.collectTree(ctx, spec, limit, add)
	case KindGlob:
		return c.collectGlob(ctx, spec, limit, add)
	default:
		return c.collectFile(ctx, spec, limit, add)
	}
}

// collectFile gathers a single file artefact.
func (c *collector) collectFile(ctx context.Context, spec ArtifactSpec, limit int64, add func(Artifact)) (bool, int64, error) {
	info, err := c.fs.Stat(ctx, spec.Path)
	if err != nil {
		if errors.Is(err, ErrFileNotFound) {
			return false, 0, nil
		}
		return false, 0, err
	}
	if info.Dir {
		return false, 0, fmt.Errorf("artefact %q is a directory; declare KindDir", spec.Path)
	}
	truncated := limit > 0 && info.Size > limit
	if truncated {
		if spec.Required {
			return true, info.Size, &ArtifactError{Spec: spec, Reason: "too-large", Found: info.Size, Limit: limit}
		}
		c.dropped = append(c.dropped, spec.Path)
	}
	collected, err := artifactFrom(ctx, spec, limit, truncated, info.Size, c.fs)
	if err != nil {
		return false, 0, err
	}
	add(collected)
	return true, minInt64(info.Size, maxInt64(limit, 1)), nil
}

// collectTree gathers a directory one level deep as separate file artefacts.
func (c *collector) collectTree(ctx context.Context, spec ArtifactSpec, limit int64, add func(Artifact)) (bool, int64, error) {
	entries, err := c.fs.List(ctx, spec.Path)
	if err != nil {
		if errors.Is(err, ErrFileNotFound) {
			return false, 0, nil
		}
		return false, 0, err
	}
	var found bool
	var total int64
	for _, e := range entries {
		if e.Dir {
			continue
		}
		sub := ArtifactSpec{Path: e.Path, Kind: KindFile, As: spec.As, MaxBytes: limit, Required: false}
		ok, size, err := c.collectFile(ctx, sub, limit, add)
		if err != nil {
			return ok, total, err
		}
		if ok {
			found = true
			total += size
		}
	}
	return found, total, nil
}

// collectGlob expands a pattern into file artefacts.
func (c *collector) collectGlob(ctx context.Context, spec ArtifactSpec, limit int64, add func(Artifact)) (bool, int64, error) {
	dir, pattern := splitPattern(spec.Path)
	entries, err := c.fs.List(ctx, dir)
	if err != nil {
		if errors.Is(err, ErrFileNotFound) {
			return false, 0, nil
		}
		return false, 0, err
	}
	var found bool
	var total int64
	for _, e := range entries {
		if e.Dir || !matchGlob(pattern, baseName(e.Path)) {
			continue
		}
		sub := ArtifactSpec{Path: e.Path, Kind: KindFile, As: spec.As, MaxBytes: limit, Required: spec.Required}
		ok, size, err := c.collectFile(ctx, sub, limit, add)
		if err != nil {
			return ok, total, err
		}
		if ok {
			found = true
			total += size
		}
	}
	return found, total, nil
}

// artifactFrom collects one artefact's content within the byte budget.
//
// The read happens now, during collection, while the environment is certainly
// alive. Returning a lazy handle into the environment would break two ways: a
// one-shot run closes its environment before the caller receives the report, and a
// session that expires later would invalidate artefacts a workflow already consumed.
// The report is the deliverable, so the bytes travel inside it, bounded by
// Limits.ArtifactMaxBytes and the set total (§13.4).
func artifactFrom(ctx context.Context, spec ArtifactSpec, limit int64, truncated bool, size int64, fs envFileSource) (Artifact, error) {
	name := spec.As
	if name == "" {
		name = spec.Path
	}
	data, err := fs.Read(ctx, spec.Path)
	if err != nil {
		return Artifact{}, fmt.Errorf("sandbox: read artefact %q: %w", spec.Path, err)
	}
	// The budget is applied here rather than trusted from the engine, because a
	// provider that ignores MaxBytes must not be able to hand the caller an
	// over-limit blob.
	if limit > 0 && int64(len(data)) > limit {
		data = data[:limit]
		truncated = true
	}
	sum := sha256.Sum256(data)
	return Artifact{
		Name:      name,
		Path:      spec.Path,
		Kind:      inferKind(spec),
		Size:      int64(len(data)),
		Truncated: truncated,
		Digest:    "sha256:" + hex.EncodeToString(sum[:]),
		data:      data,
	}, nil
}

func (sb *Sandbox) filesOf(ctx context.Context, env Environment) (envFileSource, error) {
	if !sb.caps.Has(CapFileBridge) {
		return nil, &CapabilityError{Provider: sb.name, Missing: CapFileBridge, Wanted: sb.caps}
	}
	bridge, ok := sb.provider.(FileBridge)
	if !ok {
		return nil, &CapabilityError{Provider: sb.name, Missing: CapFileBridge, Wanted: sb.caps}
	}
	fs, err := bridge.Files(ctx, rawEnvironment(env))
	if err != nil {
		return nil, fmt.Errorf("sandbox: file bridge: %w", err)
	}
	return fs, nil
}

// rawEnvironment unwraps core decorations so a provider is handed an environment
// object it created itself.
//
// The core wraps a one-shot environment in closerOnce to guarantee cleanup. Passing
// that wrapper to FileBridge.Files hands the provider a type it never built, and a
// provider that checks (as it should) rejects it as foreign — which surfaced as
// "file bridge: foreign environment" on any single Run that declared outputs.
func rawEnvironment(env Environment) Environment {
	for {
		switch w := env.(type) {
		case closerOnce:
			env = w.Environment
		case interface{ Unwrap() Environment }:
			if inner := w.Unwrap(); inner != nil && inner != env {
				env = inner
				continue
			}
			return env
		default:
			return env
		}
	}
}

// --- path helpers ----------------------------------------------------------

// baseName returns the last element of a path.
func baseName(p string) string { return path.Base(p) }

// splitPattern separates a glob into directory and pattern halves.
func splitPattern(p string) (dir, pattern string) { return path.Dir(p), path.Base(p) }

// matchGlob reports whether a name matches a glob subset: '*', '?', classes.
func matchGlob(pattern, name string) bool {
	if pattern == "" {
		return false
	}
	if strings.HasPrefix(pattern, "**") {
		return true // `reports/**` means any depth, which is the common case
	}
	return globMatch(pattern, name)
}

// globMatch is the recursive matcher.
func globMatch(pattern, name string) bool {
	for len(pattern) > 0 {
		switch pattern[0] {
		case '*':
			pattern = pattern[1:]
			if pattern == "" {
				return true
			}
			for i := 0; i <= len(name); i++ {
				if globMatch(pattern, name[i:]) {
					return true
				}
			}
			return false
		case '?':
			if name == "" {
				return false
			}
			pattern, name = pattern[1:], name[1:]
		case '[':
			end := strings.IndexByte(pattern[1:], ']')
			if end < 0 || name == "" {
				return false
			}
			if !inClass(pattern[1:1+end], name[0]) {
				return false
			}
			pattern, name = pattern[end+2:], name[1:]
		default:
			if name == "" || name[0] != pattern[0] {
				return false
			}
			pattern, name = pattern[1:], name[1:]
		}
	}
	return name == ""
}

// inClass reports membership in a bracket expression.
func inClass(set string, b byte) bool {
	negate := strings.HasPrefix(set, "!") || strings.HasPrefix(set, "^")
	if negate {
		set = set[1:]
	}
	found := false
	for i := 0; i < len(set); i++ {
		if i+2 < len(set) && set[i+1] == '-' {
			if b >= set[i] && b <= set[i+2] {
				found = true
			}
			i += 2
			continue
		}
		if set[i] == b {
			found = true
		}
	}
	return found != negate
}

// minInt64 returns the smaller value.
func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// maxInt64 returns the larger value.
func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// --- per-run output fan-out ------------------------------------------------

// fanout dispatches chunks to every sink bound to the handle, each under its
// own overflow contract (§7).
type fanout struct {
	sb     *Sandbox
	runID  string
	mu     sync.Mutex
	seq    int64
	sinks  []SinkBinding
	closed bool
	// tail holds the most recent output of the run, independent of whether any Sink
	// is configured. A ReadyProbe that only worked when someone attached a sink would
	// be the same class of no-op as the probe itself: a contract the caller cannot
	// exercise without extra setup.
	tail []byte
	// delivered counts what the provider actually pushed while the run was alive,
	// per stream. It exists so "this provider streams" is a measured fact rather
	// than a capability bit the provider asserted about itself.
	delivered map[Stream]int64
}

// defaultSinkBuffer is the per-sink queue depth when a binding does not state
// one. It is shared with config resolution so handle-level and per-run sinks
// cannot end up with different defaults.
const defaultSinkBuffer = 256

// newFanout prepares output delivery for one run. Per-run bindings are appended
// to the handle's, because WithRunSink exists for the caller who wants output for
// exactly this run: dropping it silently would be the failure mode D-05 forbids.
func (sb *Sandbox) newFanout(runID string, extra ...SinkBinding) *fanout {
	sinks := append(append([]SinkBinding(nil), sb.cfg.Sinks...), extra...)
	for i := range sinks {
		if sinks[i].Buffered <= 0 {
			sinks[i].Buffered = defaultSinkBuffer
		}
		if sinks[i].OnOverflow == OverflowUnset {
			sinks[i].OnOverflow = sb.cfg.Limits.Overflow
		}
	}
	return &fanout{sb: sb, runID: runID, sinks: sinks, delivered: map[Stream]int64{}}
}

// enabled reports whether anybody is listening.
func (f *fanout) enabled() bool { return f != nil && len(f.sinks) > 0 }

// close marks the run finished; late chunks are counted, not delivered (§7).
func (f *fanout) close() {
	if f == nil {
		return
	}
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
}

// Report implements ChunkReporter.
func (f *fanout) Report(chunk OutputChunk) error {
	if f == nil {
		return nil
	}
	f.mu.Lock()
	f.seq++
	seq := f.seq
	late := f.closed
	f.mu.Unlock()
	chunk.RunID = f.runID
	chunk.Sequence = seq
	f.mu.Lock()
	if f.delivered == nil {
		f.delivered = map[Stream]int64{}
	}
	f.delivered[chunk.Stream] += int64(len(chunk.Bytes))
	f.retain(chunk.Bytes)
	f.mu.Unlock()
	if chunk.At.IsZero() {
		chunk.At = time.Now().UTC()
	}
	if late {
		f.sb.reportFault(context.Background(), "sink", "fanout", fmt.Errorf("chunk %d arrived after the run ended", seq))
		return nil
	}
	f.sb.emit(context.Background(), Event{Kind: EventOutput, RunID: f.runID, Detail: map[string]any{"stream": chunk.Stream.String(), "bytes": len(chunk.Bytes), "seq": seq}})
	for _, b := range f.sinks {
		if b.Sink == nil {
			continue
		}
		err := callSink(b.Sink, chunk)
		if err == nil {
			continue
		}
		switch b.OnOverflow {
		case OverflowFault:
			return &HookFaultError{Kind: "sink", Name: b.Name, Detail: err.Error()}
		case OverflowBlock:
			f.sb.reportFault(context.Background(), "sink", b.Name, err)
			return err
		default:
			f.sb.reportFault(context.Background(), "sink", b.Name, err)
		}
	}
	return nil
}

// callSink invokes one sink with panic recovery: a broken consumer must not
// take the run down with it (§7).
func callSink(s Sink, chunk OutputChunk) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	return s.Emit(ctx, chunk)
}

// deliveredBytes reports what the provider pushed per stream, and the total.
func (f *fanout) deliveredBytes() (map[Stream]int64, int64) {
	if f == nil {
		return nil, 0
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[Stream]int64, len(f.delivered))
	var total int64
	for st, n := range f.delivered {
		out[st] = n
		total += n
	}
	return out, total
}

// readinessTailBytes bounds the retained output window. It is a fixed cost per
// background run, so it must stay small; it only has to be long enough to still
// contain the line a service prints when it comes up.
const readinessTailBytes = 64 << 10

// retain appends to the bounded tail. Callers hold f.mu.
func (f *fanout) retain(b []byte) {
	if len(b) == 0 {
		return
	}
	f.tail = append(f.tail, b...)
	if len(f.tail) > readinessTailBytes {
		// Keep the newest window: a readiness line is at the end, and an older
		// startup banner that scrolled past is not something the probe should fire on.
		f.tail = append(f.tail[:0], f.tail[len(f.tail)-readinessTailBytes:]...)
	}
}

// outputSnapshot returns the retained output window.
func (f *fanout) outputSnapshot() []byte {
	if f == nil {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.tail) == 0 {
		return nil
	}
	out := make([]byte, len(f.tail))
	copy(out, f.tail)
	return out
}
