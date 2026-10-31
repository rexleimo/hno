package sandbox

import (
	"fmt"
	"strings"
	"time"
)

// Phase is one ordered step of a project-level run. Phases share one workspace
// in one session, which is exactly what a one-shot executor cannot do: pay
// "npm ci" once and reuse it for build, test and lint (§13.2).
type Phase struct {
	// Name identifies the phase in PhaseReport and for resumption.
	Name string
	// Run is the request executed for this phase.
	Run Request
	// Required marks a phase whose failure aborts the project. At least one
	// phase must be required, or the project reports success on empty work.
	Required bool
	// FailOpen lets the phase fail without aborting the rest. Use it for
	// lint/style gates whose result is business data, not a build failure.
	FailOpen bool
	// Timeout overrides the phase budget. Zero uses the project budget.
	Timeout time.Duration
}

// Project is a multi-phase, workspace-scale run. It is a Request in the same
// vocabulary as a single run: declared inputs, declared artefacts, one report.
type Project struct {
	// ID optionally pins the project run for journaling and reattachment.
	ID string
	// From declares the working tree. Nil means an empty workspace.
	From *WorkspaceSource
	// Mode selects the workspace materialisation. Zero resolves to
	// Limits.DefaultMountMode, which is CopySnapshot (D-12).
	Mode MountMode
	// Runtime names the image or interpreter environment for the project.
	Runtime string
	// Phases run in order and share the workspace.
	Phases []Phase
	// Artifacts are collected after the phases finish, from the same
	// workspace the phases mutated.
	Artifacts []ArtifactSpec
	// Budget bounds the whole project; each phase inherits ExecTimeout.
	Budget Budget
	// Env applies to every phase.
	Env []string
	// Egress applies to every phase.
	Egress Egress
	// Lock decides how this project coexists with other work in the session.
	Lock LockMode
	// ResumeFrom restarts a previously failed project at the named phase
	// instead of re-running required setup (§13.2).
	ResumeFrom string
	// Attrs carries caller metadata for Policy and Observer.
	Attrs map[string]any
}

// ProjectResult reports one project run.
type ProjectResult struct {
	// RunID is the project-level identifier; phases have their own.
	RunID string
	// SessionID is the session the project ran in.
	SessionID string
	// Phases reports each executed phase in order.
	Phases []PhaseReport
	// Artifacts holds everything declared and collectable.
	Artifacts ArtifactSet
	// ExitCode is the first failing required phase's code, else the last
	// phase's code.
	ExitCode int
	// Status classifies the overall outcome.
	Status ProjectStatus
	// Duration is wall-clock across executed phases plus collection.
	Duration time.Duration
	// FailedPhase is the phase that aborted the project, if any.
	FailedPhase string
	// NextAction tells the caller how to continue, so "retry" is never guess
	// work: resume the failed phase, rebuild, or stop (§16.6).
	NextAction Action
	// Failure carries the aborting error's classification when Status is
	// ProjectFailed.
	Failure *Failure
}

// ProjectStatus is the project-level outcome.
type ProjectStatus uint8

const (
	// ProjectUnset is the zero value.
	ProjectUnset ProjectStatus = iota
	// ProjectSucceeded means every required phase passed and required
	// artefacts were collected.
	ProjectSucceeded
	// ProjectPartial means a FailOpen phase failed but required work
	// completed.
	ProjectPartial
	// ProjectFailed means a required phase failed and execution stopped.
	ProjectFailed
	// ProjectAborted means policy, cancellation or a lost environment stopped
	// execution before the failing point.
	ProjectAborted
)

// String names the status.
func (s ProjectStatus) String() string {
	switch s {
	case ProjectSucceeded:
		return "succeeded"
	case ProjectPartial:
		return "partial"
	case ProjectFailed:
		return "failed"
	case ProjectAborted:
		return "aborted"
	default:
		return "unset"
	}
}

// PhaseReport is the outcome of one phase inside a project or session.
type PhaseReport struct {
	Name     string
	RunID    string
	ExitCode int
	Status   ProjectStatus
	Duration time.Duration
	// Truncated marks output that exceeded the output limit.
	Truncated bool
	// Err holds the phase failure when Status is not succeeded. It is the
	// typed failure, so classification survives the project wrapper.
	Err error
}

// Validate rejects projects that cannot produce a meaningful result.
func (p Project) Validate() error {
	if len(p.Phases) == 0 {
		return fmt.Errorf("project needs at least one phase")
	}
	seen := map[string]bool{}
	required := 0
	for i, ph := range p.Phases {
		if strings.TrimSpace(ph.Name) == "" {
			return fmt.Errorf("phase %d needs a name", i)
		}
		if seen[ph.Name] {
			return fmt.Errorf("duplicate phase name %q", ph.Name)
		}
		seen[ph.Name] = true
		if ph.Required {
			required++
		}
		if ph.Timeout < 0 {
			return fmt.Errorf("phase %q timeout must not be negative", ph.Name)
		}
		if err := ph.Run.Validate(); err != nil {
			return fmt.Errorf("phase %q: %w", ph.Name, err)
		}
	}
	if required == 0 {
		return fmt.Errorf("project needs at least one required phase, otherwise it reports success on nothing")
	}
	if p.ResumeFrom != "" && !seen[p.ResumeFrom] {
		return fmt.Errorf("resume phase %q is not part of this project", p.ResumeFrom)
	}
	if err := p.From.Validate(); err != nil {
		return err
	}
	for _, spec := range p.Artifacts {
		if spec.Path == "" {
			return fmt.Errorf("artefact spec needs a path")
		}
	}
	for _, entry := range p.Env {
		if !validEnvEntry(entry) {
			return fmt.Errorf("project env entry %q must be KEY=VALUE with a non-empty key", entry)
		}
	}
	return nil
}

// phaseNames lists phases for error messages and journal keys.
func (p Project) phaseNames() []string {
	out := make([]string, 0, len(p.Phases))
	for _, ph := range p.Phases {
		out = append(out, ph.Name)
	}
	return out
}
