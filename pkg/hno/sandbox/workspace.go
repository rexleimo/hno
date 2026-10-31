package sandbox

import (
	"fmt"
	"io"
	"time"
)

// MountMode decides where the working tree physically lives. CopySnapshot is
// the default: letting an agent write into a host directory by default is not
// convenience, it is an incident (D-12).
type MountMode uint8

const (
	// MountUnknown is the zero value and fails validation.
	MountUnknown MountMode = iota
	// CopySnapshot materialises the declared inputs into the sandbox disk.
	// The host is untouched and the run is reproducible.
	CopySnapshot
	// MountShare exposes a host directory to the guest. Anything the payload
	// writes is written to the host, so it requires an explicit policy opt-in.
	MountShare
	// MountHybrid mounts large read-only datasets and snapshots the writable
	// working tree.
	MountHybrid
)

// String names the mode for profiles and audit records.
func (m MountMode) String() string {
	switch m {
	case CopySnapshot:
		return "copy-snapshot"
	case MountShare:
		return "mount-share"
	case MountHybrid:
		return "hybrid"
	default:
		return "unknown"
	}
}

// WorkspaceSource declares where a working tree comes from. Every variant is
// replayable, because RebuildFromSpec (D-11) can only re-render what was
// declared; a host path that silently disappeared is a permanent failure, not
// a retry.
type WorkspaceSource struct {
	// Kind selects which field below is authoritative.
	Kind SourceKind
	// Dir is a host directory for SourceDir.
	Dir string
	// Archive streams a tar/tar.gz/zip for SourceArchive.
	Archive io.Reader
	// Git describes a repository checkout for SourceGit. Credentials are
	// injected by a Policy and never enter the sandbox environment (§13.1).
	Git GitSource
	// Artifacts references an upstream Report's artefact set for
	// SourceArtifacts: pipeline hand-off without touching the host disk.
	Artifacts ArtifactSet
	// Image names a container image whose layers already contain the project.
	Image string
	// SubPath selects a directory inside the source (git sparse checkout,
	// archive prefix stripping, image path).
	SubPath string
	// Ignore lists patterns excluded while materialising.
	Ignore []string
	// MaxBytes bounds materialisation; 0 uses Limits.WorkspaceMaxBytes.
	MaxBytes int64
}

// SourceKind enumerates the replayable input origins of a workspace.
type SourceKind uint8

const (
	// SourceUnknown is the zero value and fails validation.
	SourceUnknown SourceKind = iota
	SourceDir
	SourceArchive
	SourceGit
	SourceArtifacts
	SourceImage
)

// String names the source kind for reports, events and provider errors. The
// kind appears in every "why did my project not materialise" message, so an
// unnamed enum there reads as a number.
func (k SourceKind) String() string {
	switch k {
	case SourceDir:
		return "dir"
	case SourceArchive:
		return "archive"
	case SourceGit:
		return "git"
	case SourceArtifacts:
		return "artifacts"
	case SourceImage:
		return "image"
	default:
		return "unknown"
	}
}

// GitSource is a declarative checkout request.
type GitSource struct {
	URL      string
	Ref      string // branch, tag or commit; commit is the reproducible choice
	Subdirs  []string
	TokenRef string // policy-resolved reference, never a literal secret
}

// FromDir declares a host directory as the working tree.
func FromDir(dir string, ignore ...string) *WorkspaceSource {
	return &WorkspaceSource{Kind: SourceDir, Dir: dir, Ignore: ignore}
}

// FromArchive declares a tar/tar.gz/zip stream. It never needs a host path,
// which is what makes "the model produced a project" work (§13.1).
func FromArchive(r io.Reader) *WorkspaceSource {
	return &WorkspaceSource{Kind: SourceArchive, Archive: r}
}

// FromGit declares a repository checkout.
func FromGit(url, ref string, subdirs ...string) *WorkspaceSource {
	return &WorkspaceSource{Kind: SourceGit, Git: GitSource{URL: url, Ref: ref, Subdirs: subdirs}}
}

// FromArtifacts continues a pipeline: an upstream artefact directory becomes
// this stage's working tree without a host round-trip.
func FromArtifacts(set ArtifactSet, subPath string) *WorkspaceSource {
	return &WorkspaceSource{Kind: SourceArtifacts, Artifacts: set, SubPath: subPath}
}

// FromImage declares that the project already lives in the image layers.
func FromImage(image string) *WorkspaceSource {
	return &WorkspaceSource{Kind: SourceImage, Image: image}
}

// Ignore is a pattern list helper so call sites read as declarations.
func Ignore(patterns ...string) []string { return patterns }

// Validate rejects sources that cannot be replayed.
func (s *WorkspaceSource) Validate() error {
	if s == nil {
		return nil // no workspace is legal
	}
	switch s.Kind {
	case SourceDir:
		if s.Dir == "" {
			return fmt.Errorf("dir source requires a host path")
		}
	case SourceArchive:
		if s.Archive == nil {
			return fmt.Errorf("archive source requires a reader")
		}
	case SourceGit:
		if s.Git.URL == "" {
			return fmt.Errorf("git source requires a repository url")
		}
		if s.Git.Ref == "" {
			return fmt.Errorf("git source requires a ref; an implicit default branch is not reproducible")
		}
	case SourceArtifacts:
		if len(s.Artifacts) == 0 {
			return fmt.Errorf("artifact source requires a non-empty upstream artefact set")
		}
	case SourceImage:
		if s.Image == "" {
			return fmt.Errorf("image source requires an image reference")
		}
	case SourceUnknown:
		return fmt.Errorf("workspace source kind is unset")
	default:
		return fmt.Errorf("unsupported workspace source kind %d", s.Kind)
	}
	if s.MaxBytes < 0 {
		return fmt.Errorf("workspace max bytes must not be negative")
	}
	return nil
}

// EgressMode is the network posture of a run. The old executor only had
// NetworkNone; a run that needs a allowlist could not say so (§10 出网白名单).
type EgressMode uint8

const (
	// EgressUnset leaves the profile decision in force.
	EgressUnset EgressMode = iota
	// EgressNone denies all network access. Local default (D-08).
	EgressNone
	// EgressAllowlist permits only the listed hosts.
	EgressAllowlist
	// EgressOpen permits outbound traffic. Only valid for a development
	// profile; a production profile that reaches this fails validation.
	EgressOpen
)

// String names the posture for audit records.
func (m EgressMode) String() string {
	switch m {
	case EgressNone:
		return "none"
	case EgressAllowlist:
		return "allowlist"
	case EgressOpen:
		return "open"
	default:
		return "unset"
	}
}

// Egress declares outbound network intent.
type Egress struct {
	Mode  EgressMode
	Allow []string // hosts or CIDRs, valid only with EgressAllowlist
	// Requested lists hosts the code attempted to reach. Providers that can
	// observe connections fill it in; the core uses it to report refusals
	// instead of a generic network error (§16.6).
	Requested []string
	// SecretRefs name credentials the host-side proxy substitutes so the
	// secret never appears in the guest environment (requires
	// CapSecretSubstitution).
	SecretRefs []string
}

// EffectClass declares what a run may leave behind outside the sandbox. It
// gates retry, not permissions (§16.3): an external effect is never retried
// automatically.
type EffectClass uint8

const (
	// EffectUnset is treated as EffectExternal, the safe assumption.
	EffectUnset EffectClass = iota
	// EffectPure computes without changing anything observable.
	EffectPure
	// EffectWorkspace writes inside the sandbox only; rollback or rebuild
	// makes it retryable.
	EffectWorkspace
	// EffectExternal produces an externally visible action: a host write, a
	// git push, an HTTP call, a database row.
	EffectExternal
)

// String names the effect class.
func (e EffectClass) String() string {
	switch e {
	case EffectPure:
		return "pure"
	case EffectWorkspace:
		return "workspace"
	case EffectExternal:
		return "external"
	default:
		return "external"
	}
}

// Delivery is the at-most/at-least/exactly-once promise for a run with an
// idempotency key.
type Delivery uint8

const (
	// DeliveryUnset means: no journal, plain execution.
	DeliveryUnset Delivery = iota
	// DeliveryAtMostOnce never runs twice; an uncertain outcome is an error.
	DeliveryAtMostOnce
	// DeliveryAtLeastOnce may run again; the receiver must de-duplicate.
	DeliveryAtLeastOnce
	// DeliveryExactlyOnce is best effort: journal plus idempotency key.
	DeliveryExactlyOnce
)

// RetryMode decides what state survives before an attempt is repeated
// (§16.4). RebuildFromSpec is the default (D-11).
type RetryMode uint8

const (
	// RetryUnset resolves to RebuildFromSpec.
	RetryUnset RetryMode = iota
	// RetryBlind repeats in place. Legal only for EffectPure: the previous
	// attempt's debris is still on disk.
	RetryBlind
	// RebuildFromSpec discards the environment and re-renders the recorded
	// spec before retrying.
	RebuildFromSpec
	// RollbackToPhase restores the last checkpoint. Requires an explicit
	// checkpoint because auto-checkpointing is off by default (D-14).
	RollbackToPhase
	// ForwardsOnly repeats nothing: continue or re-collect artefacts.
	ForwardsOnly
)

// String names the retry mode.
func (m RetryMode) String() string {
	switch m {
	case RetryBlind:
		return "blind"
	case RebuildFromSpec:
		return "rebuild-from-spec"
	case RollbackToPhase:
		return "rollback-to-phase"
	case ForwardsOnly:
		return "forwards-only"
	default:
		return "rebuild-from-spec"
	}
}

// LockMode decides how concurrent Exec calls on one session are serialised
// (D-06).
type LockMode uint8

const (
	// LockUnset resolves to LockSerial.
	LockUnset LockMode = iota
	// LockSerial queues Exec on this session.
	LockSerial
	// LockParallel allows overlapping runs; the caller owns state pollution.
	LockParallel
)

// String names the lock mode.
func (m LockMode) String() string {
	if m == LockParallel {
		return "parallel"
	}
	return "serial"
}

// Budget bounds one run. SetupTimeout is separate from ExecTimeout because a
// micro-VM boot costs 1-2s: charging boot against the execution deadline makes
// every retry time out spuriously (§16.2).
type Budget struct {
	SetupTimeout   time.Duration
	ExecTimeout    time.Duration
	OutputLimit    int64
	WorkspaceLimit int64
	CostLimit      float64 // provider billing, 0 = profile default
}
