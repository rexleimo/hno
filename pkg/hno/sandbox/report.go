package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

// Report is the result of one execution. It is returned even when the run
// failed or was denied: the caller still needs RunID, isolation actually
// achieved, and durations for audit. Only a panic inside the core leaves it
// nil (§5 Run contract).
type Report struct {
	// RunID identifies this execution for logs, reattach and the journal.
	RunID string
	// SessionID is empty for one-shot runs.
	SessionID string
	// ExitCode is the payload's exit status. -1 means "never started".
	ExitCode int
	Stdout   string
	Stderr   string
	// OutputSent is the number of output bytes the provider delivered *while the
	// run was still executing*, counted per chunk at the sink boundary. It is zero
	// for a provider that only hands over a finished buffer.
	//
	// This used to be a documented field nothing ever wrote, which made "I received
	// the output" and "the sink was configured but got nothing" indistinguishable to
	// the caller — the same silent-no-op class as an unset artefact size (§13.4, D-05).
	OutputSent int64
	// OutputSkipped counts bytes withheld from sinks by a limit or an overflow
	// policy, so a quiet sink is explainable as deliberate rather than broken.
	OutputSkipped int64
	// Truncated says output hit the limit and the tail may be missing, so an
	// empty stderr does not prove the code was quiet.
	Truncated bool
	// TimedOut distinguishes "the code was slow" from "the code failed".
	TimedOut bool
	// Lost says the environment died mid-run (§15).
	Lost bool
	// Canceled says the caller's context ended the run.
	Canceled bool
	// Duration covers boot plus execution; SetupDuration splits the boot cost
	// so a timeout can be attributed honestly (§16.2).
	Duration      time.Duration
	SetupDuration time.Duration
	// Isolation is the level actually achieved, which may exceed the ask but
	// never fall below it.
	Isolation Isolation
	Provider  string
	// Artifacts holds the declared outputs that were collected.
	Artifacts ArtifactSet
	// Phases reports phase-level outcomes for project runs.
	Phases []PhaseReport
	// RunRef enables detach, status query and reattachment.
	RunRef *RunRef
	// BlockedHosts lists egress destinations denied mid-run (§15).
	BlockedHosts []string
	// Failure classifies the outcome so retry decisions are mechanical
	// (§16.1). Nil means the run succeeded.
	Failure *Failure
	// Idempotent replays of a journalled run report Republished instead of
	// executing twice (§16.3).
	Republished bool
	// RepublishedRunID points at the original run when Republished is true.
	RepublishedRunID string
}

// Failed reports whether the run did not complete successfully. It covers
// policy denial and environment loss, not only a non-zero exit code.
func (r *Report) Failed() bool {
	return r != nil && (r.ExitCode != 0 || r.TimedOut || r.Lost || r.Canceled || r.Failure != nil)
}

// Retryable reports whether the core would consider this outcome worth another
// attempt at all. Callers still own the decision; this keeps "did the sandbox
// break" out of hand-written exit-code arithmetic.
func (r *Report) Retryable() bool { return r != nil && r.Failure != nil && r.Failure.Retryable }

// ArtifactSet is the collected output of one run, keyed by the declared name
// so downstream stages select by stable identity instead of by path guessing.
type ArtifactSet []Artifact

// Get finds an artefact by its declared As name or, failing that, by path.
func (s ArtifactSet) Get(name string) (Artifact, bool) {
	for _, a := range s {
		if a.Name == name || a.Path == name {
			return a, true
		}
	}
	return Artifact{}, false
}

// Names lists artefact identifiers for reports and logs.
func (s ArtifactSet) Names() []string {
	out := make([]string, 0, len(s))
	for _, a := range s {
		out = append(out, a.Name)
	}
	return out
}

// Total is the summed declared size, used by transfer budget checks.
func (s ArtifactSet) Total() int64 {
	var n int64
	for _, a := range s {
		n += a.Size
	}
	return n
}

// Artifact is one collected output. Content is fetched lazily through the
// provider so a large report never sits in memory just because a run
// mentioned it (§14.3).
type Artifact struct {
	// Name is the declared As key, defaulting to Path.
	Name string
	// Path is where the artefact lived inside the sandbox.
	Path string
	Kind ArtifactKind
	// Size is the collected byte count, which may be smaller than on-disk size
	// when Truncated is set.
	Size      int64
	Truncated bool
	// Digest is a content hash when the content could be read during collection.
	Digest string
	// data holds the collected bytes.
	//
	// Collection reads eagerly rather than handing back a live pointer into the
	// environment. A one-shot run closes its environment before the caller gets the
	// report, so a lazy opener produced a handle that could only ever error — and a
	// session that expires later would invalidate artefacts the workflow already
	// relied on. The report is the deliverable, so the bytes travel with it, bounded
	// by Limits.ArtifactMaxBytes and the set total (§13.4).
	data []byte
}

// ErrArtifactUnreadable is returned when an artefact handle exists but its
// content is gone (session destroyed, retention expired, limit dropped it).
var ErrArtifactUnreadable = errors.New("sandbox: artefact content is no longer readable")

// Content returns the collected bytes.
func (a Artifact) Content() []byte { return a.data }

// StreamingContractError reports that a provider claimed incremental output
// delivery and then delivered none.
type StreamingContractError struct {
	Provider string
	// Streams names the streams that produced output but received no chunk.
	Streams []string
	// DeclaredBytes is the output the payload produced.
	DeclaredBytes int64
}

func (e *StreamingContractError) Error() string {
	return fmt.Sprintf("sandbox: provider %q declares streaming but delivered no chunks for %s (%d bytes of output arrived only at exit)",
		e.Provider, strings.Join(e.Streams, ", "), e.DeclaredBytes)
}

// StreamingContract reports whether streaming actually happened.
//
// The question is mechanical: if this run produced output and zero bytes reached a
// sink while the run was alive, whatever the provider advertised, delivery was at
// exit. Callers that render progress for a human should ask this instead of trusting
// a CapabilitySet, because a sink that is configured and silent is otherwise
// indistinguishable from a payload that printed nothing.
//
// It returns nil when the run asked for nothing, produced nothing, or genuinely
// streamed.
func (r *Report) StreamingContract() error {
	if r == nil || r.OutputSent > 0 {
		return nil
	}
	var silent []string
	if len(r.Stdout) > 0 {
		silent = append(silent, StreamStdout.String())
	}
	if len(r.Stderr) > 0 {
		silent = append(silent, StreamStderr.String())
	}
	if len(silent) == 0 {
		return nil
	}
	return &StreamingContractError{
		Provider:      r.Provider,
		Streams:       silent,
		DeclaredBytes: int64(len(r.Stdout) + len(r.Stderr)),
	}
}

// Remediation explains what to do about a false capability claim.
func (e *StreamingContractError) Remediation() []string {
	return []string{
		"treat the output as post-exact: read Report.Stdout/Stderr instead of a Sink",
		"or use a provider that streams genuinely (see CapabilitySet.Has(CapStreaming))",
	}
}

// Reader streams the artefact. The caller must close it.
func (a Artifact) Reader(_ context.Context) (io.ReadCloser, error) {
	if a.data == nil {
		return nil, fmt.Errorf("%w: %s", ErrArtifactUnreadable, a.Name)
	}
	return io.NopCloser(bytes.NewReader(a.data)), nil
}

// Bytes loads the artefact into memory.
func (a Artifact) Bytes(ctx context.Context) ([]byte, error) {
	rc, err := a.Reader(ctx)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}

// JSON decodes a JSON artefact into v.
func (a Artifact) JSON(ctx context.Context, v any) error {
	b, err := a.Bytes(ctx)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("sandbox: artefact %q is not valid JSON: %w", a.Name, err)
	}
	return nil
}

// Lines splits a text artefact into lines, for line-oriented downstream work.
func (a Artifact) Lines(ctx context.Context) ([]string, error) {
	b, err := a.Bytes(ctx)
	if err != nil {
		return nil, err
	}
	text := strings.TrimSuffix(string(b), "\n")
	if text == "" {
		return nil, nil
	}
	return strings.Split(text, "\n"), nil
}

// WriteTo streams the artefact into w (a host file, a tar writer, another
// sandbox). It returns the bytes written.
func (a Artifact) WriteTo(ctx context.Context, w io.Writer) (int64, error) {
	rc, err := a.Reader(ctx)
	if err != nil {
		return 0, err
	}
	defer rc.Close()
	n, err := io.Copy(w, rc)
	if err != nil {
		return n, fmt.Errorf("sandbox: artefact %q transfer failed: %w", a.Name, err)
	}
	return n, nil
}

// Store persists the artefact into a blob store so a later workflow step,
// issue comment or audit trail can read it after the sandbox is gone.
func (a Artifact) Store(ctx context.Context, store BlobStore, key string) error {
	if store == nil {
		return fmt.Errorf("sandbox: artefact %q needs a blob store to persist", a.Name)
	}
	rc, err := a.Reader(ctx)
	if err != nil {
		return err
	}
	defer rc.Close()
	if err := store.Put(ctx, key, rc, a.Size); err != nil {
		return fmt.Errorf("sandbox: persisting artefact %q under %q: %w", a.Name, key, err)
	}
	return nil
}

// BlobStore is the minimal persistence contract artefacts need. It is declared
// here so the core does not import a storage package (dependency rule);
// pkg/hno/store satisfies it via a small adapter owned by the caller.
type BlobStore interface {
	Put(ctx context.Context, key string, r io.Reader, sizeHint int64) error
}

// RunRef is the handle for a submitted run: detach, query and reattach
// (§15). It is a value, safe to persist in a workflow state.
type RunRef struct {
	ID        string
	SessionID string
	Provider  string
	// Reattachable says the engine can resend output after a disconnect.
	Reattachable bool
	// StatusQueryable says terminal state can be confirmed, which is what
	// turns "uncertain" into a safe retry (§16.3).
	StatusQueryable bool
}

// Status describes a live sandbox handle: what it actually provides, not what
// was asked for. CI asserts on this instead of on a version string (§5).
type Status struct {
	Provider     string
	Isolation    Isolation
	Capabilities CapabilitySet
	// Image is the resolved environment reference.
	Image string
	// Formation is how the engine is attached: inproc, remote or sidecar.
	Formation Formation
	// Sessions counts live sessions owned by this handle.
	Sessions int
	// StartedAt is when the handle was opened.
	StartedAt time.Time
	// Limits in force after profile resolution.
	Limits Limits
}

// Meets reports whether the handle satisfies a capability and isolation ask.
func (s Status) Meets(want Capability, min Isolation) error {
	if !s.Capabilities.Has(want) {
		return &CapabilityError{Provider: s.Provider, Missing: want, Wanted: s.Capabilities}
	}
	if min != IsolationUnknown && !s.Isolation.AtLeast(min) {
		return &IsolationError{Provider: s.Provider, Want: min, Got: s.Isolation}
	}
	return nil
}
