package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// StepKind names one recorded environment change.
type StepKind uint8

const (
	// StepUnknown is the zero value and never valid.
	StepUnknown StepKind = iota
	// StepMaterialize places declared input content into the workspace.
	StepMaterialize
	// StepInstall changes the toolchain or package set through the sanctioned
	// API. Replaying it is what keeps "pip install pandas" alive after a
	// rebuild (§17.1).
	StepInstall
	// StepSetup runs a declared setup script.
	StepSetup
	// StepEnv records an environment variable assignment.
	StepEnv
	// StepWorkspaceDelta is a promoted accidental change: files the payload
	// created that CollectThenRebuild captured so they survive a rebuild
	// (§17.4).
	StepWorkspaceDelta
)

// String names the step kind.
func (k StepKind) String() string {
	switch k {
	case StepMaterialize:
		return "materialize"
	case StepInstall:
		return "install"
	case StepSetup:
		return "setup"
	case StepEnv:
		return "env"
	case StepWorkspaceDelta:
		return "workspace-delta"
	default:
		return "unknown"
	}
}

// Step is one replayable environment change. Everything that claims to modify
// the environment goes through the recorded spec, because the default retry
// mode is RebuildFromSpec: what is not declared does not come back (D-11).
type Step struct {
	Kind StepKind
	Tool string   // pip, apt, npm, go, or "" for non-install steps
	Args []string // ordered arguments
	// Argv is the exact command line the core ran. Rebuild prefers it over
	// reconstructing flags from a tool name, because a reconstructed command is
	// how a rebuilt environment silently differs from the original (§17.1).
	Argv     []string
	Script   string // body for StepSetup
	Path     string // workspace-relative target for materialize
	Env      string // KEY for StepEnv
	Source   *WorkspaceSource
	Digest   string // content hash for cache identity
	Sequence int    // declaration order; the key ignores it on purpose
}

// Key is the stable cache identity of a step: base image digest plus the
// normalised content of the change. Declaration order is excluded so inserting
// a step earlier does not invalidate the cache of unrelated steps (§17.2).
func (s Step) Key() string {
	h := sha256.New()
	fmt.Fprintf(h, "kind=%s\n", s.Kind)
	fmt.Fprintf(h, "tool=%s\n", s.Tool)
	for _, a := range s.Args {
		fmt.Fprintf(h, "arg=%s\n", a)
	}
	for _, a := range s.Argv {
		fmt.Fprintf(h, "argv=%s\n", a)
	}
	fmt.Fprintf(h, "script=%s\n", strings.TrimSpace(s.Script))
	fmt.Fprintf(h, "path=%s\n", s.Path)
	fmt.Fprintf(h, "env=%s\n", s.Env)
	fmt.Fprintf(h, "digest=%s\n", s.Digest)
	if s.Source != nil {
		fmt.Fprintf(h, "source=%d|%s|%s|%s|%s\n", s.Source.Kind, s.Source.Dir, s.Source.Git.URL+"@"+s.Source.Git.Ref, s.Source.Image, s.Source.SubPath)
		ignore := append([]string(nil), s.Source.Ignore...)
		sort.Strings(ignore)
		fmt.Fprintf(h, "ignore=%s\n", strings.Join(ignore, ","))
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// Validate rejects steps that cannot be replayed.
func (s Step) Validate() error {
	switch s.Kind {
	case StepInstall:
		if s.Tool == "" {
			return fmt.Errorf("install step needs a tool")
		}
		if len(s.Args) == 0 && len(s.Argv) == 0 {
			return fmt.Errorf("install step %q needs arguments", s.Tool)
		}
	case StepSetup:
		if strings.TrimSpace(s.Script) == "" {
			return fmt.Errorf("setup step needs a script body")
		}
	case StepMaterialize, StepWorkspaceDelta:
		if err := checkRelPath("step path", s.Path); err != nil {
			return err
		}
		if s.Kind == StepWorkspaceDelta && s.Source == nil {
			return fmt.Errorf("workspace delta step needs an archive source")
		}
	case StepEnv:
		if s.Env == "" {
			return fmt.Errorf("env step needs a key")
		}
	case StepUnknown:
		return fmt.Errorf("step kind is unset")
	default:
		return fmt.Errorf("unsupported step kind %d", s.Kind)
	}
	return nil
}

// EnvFingerprint folds the environment the steps run under into the cache key:
// a step replayed with a different registry mirror or proxy is not the same
// step. Values are hashed, never stored in plaintext.
func EnvFingerprint(env []string) string {
	h := sha256.New()
	keys := make([]string, 0, len(env))
	for _, entry := range env {
		name, _, ok := strings.Cut(entry, "=")
		if !ok {
			name = entry
		}
		keys = append(keys, name)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(h, "%s\x00", k)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// SessionSpec is the replayable definition of an environment. It is ordered,
// validated, and the only description of the environment that survives a
// rebuild.
type SessionSpec struct {
	Base      string // image or runtime reference
	Env       []string
	Steps     []Step
	MountMode MountMode
	// Expose publishes guest ports when the environment is created. It lives here
	// rather than only on Request because port mapping is a create-time property of the
	// substrate: a run that asked for a mapping after the container existed could not
	// have it applied, so the field would have been a promise no provider could keep.
	Expose []PortMap
	// Recorded is set once the spec has been persisted so callers can tell a
	// fresh spec from a resumed one.
	Recorded bool
}

// Clone returns a deep copy so a session never shares slice backing arrays with
// its caller.
func (sp SessionSpec) Clone() SessionSpec {
	out := sp
	out.Env = append([]string(nil), sp.Env...)
	if sp.Expose != nil {
		out.Expose = append([]PortMap(nil), sp.Expose...)
	}
	if sp.Steps != nil {
		out.Steps = make([]Step, len(sp.Steps))
		copy(out.Steps, sp.Steps)
	}
	return out
}

// Add appends a validated step and returns the session-scoped index.
func (sp *SessionSpec) Add(step Step) error {
	if err := step.Validate(); err != nil {
		return err
	}
	step.Sequence = len(sp.Steps)
	sp.Steps = append(sp.Steps, step)
	return nil
}

// Fingerprint identifies the whole spec, used by the journal and by drift
// reporting.
func (sp SessionSpec) Fingerprint() string {
	h := sha256.New()
	fmt.Fprintf(h, "base=%s\nmode=%s\n", sp.Base, sp.MountMode)
	fmt.Fprintf(h, "envfp=%s\n", EnvFingerprint(sp.Env))
	for _, s := range sp.Steps {
		fmt.Fprintf(h, "step=%s\n", s.Key())
	}
	return hex.EncodeToString(h.Sum(nil))[:32]
}
