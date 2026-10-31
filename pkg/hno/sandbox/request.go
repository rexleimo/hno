package sandbox

import (
	"fmt"
	"io"
	"regexp"
	"time"
)

// Request describes one execution. It replaces the old one-shot Spec, whose
// only file channel was a map of inline bytes: real work needs declared
// inputs, a declared workspace and declared artefacts (§13).
type Request struct {
	// ID optionally pins a run identifier so it can be reattached or queried
	// after a disconnect (requires CapQueryStatus/CapBackground).
	ID string
	// Runtime selects an interpreter template: "python", "node", "shell", or
	// an engine-native one. Empty with Argv set means run Argv directly.
	Runtime string
	// Code is the payload fed to the runtime on stdin, so payload bytes never
	// appear in the process argument list.
	Code string
	// Argv is an explicit command line, used instead of Runtime/Code.
	Argv []string
	// Cwd is the working directory inside the sandbox.
	Cwd string
	// Env entries are KEY=VALUE. Secrets do not belong here: they are resolved
	// by a Policy or substituted by the host proxy (Egress.SecretRefs).
	Env []string
	// Files inlines small payloads (≤ Limits.InlineFileMaxBytes). Project
	// scale data belongs in Inputs or Workspace, not here (v0.2 defect).
	Files map[string]FileContent
	// Inputs streams larger or host-resident files in without buffering them
	// in a map.
	Inputs []Input
	// Workspace declares the working tree for project-level work.
	Workspace *WorkspaceSource
	// Mode selects how the workspace is materialised. Zero resolves to the
	// profile default, and a per-run option can never widen it (D-12).
	Mode MountMode
	// Outputs declares which artefacts to collect. Undeclared paths are
	// dropped and counted (§13.3).
	Outputs []ArtifactSpec
	// Egress is the outbound network intent.
	Egress Egress
	// Timeout bounds execution only; boot is charged to the session budget.
	Timeout time.Duration
	// Background starts the run detached and returns as soon as it is
	// submitted (requires CapBackground).
	Background bool
	// ReadyProbe turns Background into "start a service and wait until it is
	// ready" instead of "hang forever" (§15).
	ReadyProbe *ReadyProbe
	// Expose forwards guest ports to host ports (requires CapPortForward).
	Expose []PortMap
	// Effect declares what the run leaves behind and therefore whether it may
	// be retried automatically. Zero is EffectExternal.
	Effect EffectClass
	// IdempotencyKey plus Delivery enable the run journal (§16.3).
	IdempotencyKey string
	Delivery       Delivery
	// Budget overrides the resolved profile budget for this run.
	Budget Budget
	// Attrs carries caller metadata for Policy and Observer consumption. It is
	// never sent to the engine.
	Attrs map[string]any
}

// FileContent is an inline file payload with its mode.
type FileContent struct {
	Content []byte
	Mode    uint32
}

// Input is one streamed or host-resident input file. Exactly one content
// source may be set.
type Input struct {
	// Path is the in-sandbox destination path, relative to the workspace root.
	Path string
	// Mode is the POSIX file mode applied inside the sandbox.
	Mode uint32
	// Bytes, HostPath and Reader are alternatives; at most one is set.
	Bytes    []byte
	HostPath string
	Reader   io.Reader
	// Executable marks a script input so the mode gains the execute bits even
	// when Mode is zero.
	Executable bool
}

// ArtifactSpec declares one collected artefact (§13.3). Artefacts are
// declared, never guessed: guessing produces either a megabyte of cache or
// nothing at all.
type ArtifactSpec struct {
	// Path is a directory, a file or a glob pattern.
	Path string
	// Kind disambiguates so a literal "*" is not mistaken for a directory.
	Kind ArtifactKind
	// Required artefacts that are missing or oversized fail the run; optional
	// ones are delivered truncated with Truncated set (D-17).
	Required bool
	// MaxBytes bounds this artefact. Zero uses Limits.ArtifactMaxBytes.
	MaxBytes int64
	// As names the artefact in Report.Artifacts so downstream stages select by
	// a stable key instead of a path.
	As string
}

// ArtifactKind says how to interpret ArtifactSpec.Path.
type ArtifactKind uint8

const (
	// KindUnset resolves to KindFile for a plain path and KindGlob for a
	// pattern.
	KindUnset ArtifactKind = iota
	// KindFile is a single file.
	KindFile
	// KindDir is a directory tree.
	KindDir
	// KindGlob is a shell pattern.
	KindGlob
)

// ReadyProbe waits for a service to be usable instead of trusting sleep (§15).
type ReadyProbe struct {
	// URL is polled until it returns 2xx (checked inside the sandbox).
	URL string
	// Port is considered ready when it accepts a connection.
	Port int
	// Pattern matches a line on stdout/stderr.
	Pattern string
	// Deadline bounds the probe. Zero uses Budget.SetupTimeout.
	Deadline time.Duration
}

// PortMap forwards one guest port to the host.
type PortMap struct {
	HostPort  int
	GuestPort int
}

// Python, Node and Shell build the three templates the executor has always
// supported, keeping payload off the argument list.
func Python(code string) Request { return Request{Runtime: "python", Code: code} }
func Node(code string) Request   { return Request{Runtime: "node", Code: code} }
func Shell(script string) Request {
	return Request{Runtime: "shell", Code: script}
}

// Command runs an explicit argv with no interpreter template.
func Command(argv ...string) Request { return Request{Argv: argv} }

// clone returns a deep-enough copy for Policy to tighten without mutating the
// caller's value. Maps are rebuilt, slices are copied.
func (r Request) clone() Request {
	out := r
	out.Workspace = cloneSource(r.Workspace)
	if r.Files != nil {
		out.Files = make(map[string]FileContent, len(r.Files))
		for k, v := range r.Files {
			v.Content = append([]byte(nil), v.Content...)
			out.Files[k] = v
		}
	}
	out.Inputs = append([]Input(nil), r.Inputs...)
	out.Outputs = append([]ArtifactSpec(nil), r.Outputs...)
	out.Env = append([]string(nil), r.Env...)
	out.Egress.Allow = append([]string(nil), r.Egress.Allow...)
	out.Egress.SecretRefs = append([]string(nil), r.Egress.SecretRefs...)
	out.Argv = append([]string(nil), r.Argv...)
	if r.Expose != nil {
		out.Expose = append([]PortMap(nil), r.Expose...)
	}
	if r.ReadyProbe != nil {
		p := *r.ReadyProbe
		out.ReadyProbe = &p
	}
	if r.Budget != (Budget{}) {
		out.Budget = r.Budget
	}
	return out
}

// hasPayload reports whether the request actually asks for work. An empty
// request is a configuration error, never a no-op run.
//
// Declaring content without a command is work: placing files is what a recorded input
// does, and what rebuilding one has to do again. It used to be rejected unless a Runtime
// was also named, which made "write this file back" an illegal request and therefore made
// every rebuild of a session that had recorded an input fail before reaching a provider.
func (r Request) hasPayload() bool {
	if len(r.Argv) > 0 {
		return true
	}
	if r.Runtime != "" && (r.Code != "" || len(r.Files) > 0 || len(r.Inputs) > 0) {
		return true
	}
	return len(r.Inputs) > 0 || len(r.Files) > 0 || r.Workspace != nil
}

// Validate rejects requests that must never reach a provider.
func (r Request) Validate() error {
	if !r.hasPayload() {
		return fmt.Errorf("sandbox request has nothing to run: set Runtime+Code or Argv")
	}
	// Every path in a request is workspace-relative. An absolute path is not a
	// style preference: it is how a request escapes the sandbox root (§13.1).
	if r.Cwd != "" && !isRelPath(r.Cwd) {
		return fmt.Errorf("request cwd %q must be relative to the workspace root", r.Cwd)
	}
	for p := range r.Files {
		if !isRelPath(p) {
			return fmt.Errorf("inline file path %q must be relative to the workspace root", p)
		}
	}
	if len(r.Argv) > 0 && r.Runtime != "" && r.Code != "" {
		return fmt.Errorf("sandbox request cannot mix Argv with Runtime+Code")
	}
	if len(r.Argv) > 0 && r.Argv[0] == "" {
		return fmt.Errorf("sandbox request argv[0] must not be empty")
	}
	if r.Timeout < 0 {
		return fmt.Errorf("sandbox request timeout must not be negative")
	}
	if r.ReadyProbe != nil {
		// A probe needs a run that has already returned control, so only a detached
		// run can be probed: a foreground command blocks until exit and nothing can
		// observe it coming up. Rejecting the combination is the honest version of
		// accepting a field that could never be evaluated.
		if !r.Background {
			return fmt.Errorf("ready probe requires Background: a foreground run blocks until it exits, so there is no moment at which to check readiness")
		}
		if r.ReadyProbe.URL == "" && r.ReadyProbe.Port == 0 && r.ReadyProbe.Pattern == "" {
			return fmt.Errorf("ready probe has no condition to check")
		}
		if r.ReadyProbe.Deadline < 0 {
			return fmt.Errorf("ready probe deadline must not be negative")
		}
		if r.ReadyProbe.Pattern != "" {
			if _, err := regexp.Compile(r.ReadyProbe.Pattern); err != nil {
				// Not fatal at run time — the core falls back to substring matching —
				// but an uncompileable pattern is almost always a typo, and Validate is
				// where a caller can still hear about it cheaply.
				return fmt.Errorf("ready probe pattern is not a valid regexp (%v); it would be matched as a literal substring", err)
			}
		}
	}
	for path, file := range r.Files {
		if err := checkRelPath("files", path); err != nil {
			return err
		}
		_ = file
	}
	for _, in := range r.Inputs {
		if err := checkRelPath("inputs", in.Path); err != nil {
			return err
		}
		set := 0
		if in.Bytes != nil {
			set++
		}
		if in.HostPath != "" {
			set++
		}
		if in.Reader != nil {
			set++
		}
		if set != 1 {
			return fmt.Errorf("input %q needs exactly one of Bytes, HostPath or Reader", in.Path)
		}
	}
	if err := r.Workspace.Validate(); err != nil {
		return err
	}
	for _, spec := range r.Outputs {
		if spec.Path == "" {
			return fmt.Errorf("artefact spec needs a path")
		}
		if spec.MaxBytes < 0 {
			return fmt.Errorf("artefact %q max bytes must not be negative", spec.Path)
		}
	}
	switch r.Egress.Mode {
	case EgressAllowlist:
		if len(r.Egress.Allow) == 0 {
			return fmt.Errorf("allowlist egress requires at least one host")
		}
	case EgressOpen:
		if len(r.Egress.Allow) > 0 {
			return fmt.Errorf("open egress cannot also list allowed hosts")
		}
	}
	if r.Delivery != DeliveryUnset && r.IdempotencyKey == "" {
		return fmt.Errorf("delivery semantics require an idempotency key")
	}
	if r.IdempotencyKey != "" && r.Delivery == DeliveryUnset {
		return fmt.Errorf("idempotency key requires a delivery promise")
	}
	for _, entry := range r.Env {
		if !validEnvEntry(entry) {
			return fmt.Errorf("env entry %q must be KEY=VALUE with a non-empty key", entry)
		}
	}
	return nil
}
