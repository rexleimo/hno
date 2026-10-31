package sandbox

import (
	"context"
	"time"
)

// Action is the vocabulary a Policy may return. Three states, no "undecided":
// a hook that wants to abstain returns Decision{Action: ActAllow}.
type DecisionAction uint8

const (
	// ActAllow lets the request through.
	ActAllow DecisionAction = iota
	// ActDeny vetoes it.
	ActDeny
	// ActRequireApproval suspends execution until a human decides.
	ActRequireApproval
)

// String names the action.
func (a DecisionAction) String() string {
	switch a {
	case ActAllow:
		return "allow"
	case ActDeny:
		return "deny"
	case ActRequireApproval:
		return "require_approval"
	default:
		return "unset"
	}
}

// Decision is a Policy's answer.
type Decision struct {
	Action DecisionAction
	Reason string
	// Rule names the check that fired, so a trail is searchable instead of
	// merely long.
	Rule     string
	Policy   string // filled by the core when empty, for DeniedError
	Approval *Approval
	// Tighten mutates the request within the allowed envelope. It can only
	// reduce privilege; the core rejects loosening (§7).
	Tighten func(*Request)
}

// Approval is the human-decision envelope handed to whatever owns approvals
// (an agent HITL interrupt, a ticket, a queue).
type Approval struct {
	ID       string
	Approver string
	Reason   string
	Expires  time.Duration
	Attrs    map[string]any
}

// RunContext is the immutable per-execution snapshot the extension points
// share: who asked, what for, which budget, and what the caller wants
// correlated across hooks.
type RunContext struct {
	RunID     string
	SessionID string
	Caller    CallerInfo
	Kind      RunKind
	Request   *Request
	Project   *Project
	Want      Isolation
	Provider  string
	Budget    Budget
	Limits    Limits
	Attrs     map[string]any
	// Sinks are the per-run output bindings, merged with the handle-level ones
	// at fan-out. They live here because a Sink is an extension point: policy
	// must be able to see who is receiving output, and a per-run sink that never
	// reached the fan-out would be a silent no-op (§7).
	Sinks []SinkBinding
}

// RunKind distinguishes the three execution shapes (§6.4).
type RunKind uint8

const (
	// RunKindUnknown is the zero value.
	RunKindUnknown RunKind = iota
	// RunKindOnce is a one-shot execution on a fresh environment.
	RunKindOnce
	// RunKindPhase is one phase inside a session or project.
	RunKindPhase
	// RunKindProject is a multi-phase project run.
	RunKindProject
	// RunKindBackground is a detached run with a reattachable handle.
	RunKindBackground
)

// String names the kind.
func (k RunKind) String() string {
	switch k {
	case RunKindOnce:
		return "once"
	case RunKindPhase:
		return "phase"
	case RunKindProject:
		return "project"
	case RunKindBackground:
		return "background"
	default:
		return "unset"
	}
}

// CallerInfo identifies who is asking, for policy and audit.
type CallerInfo struct {
	Agent   string
	Tenant  string
	User    string
	TraceID string
}

// Policy decides whether an execution may happen. It is the only extension
// point allowed to say no, and a broken Policy is fatal: fail closed (§7).
type Policy interface {
	Decide(ctx context.Context, rc *RunContext) (Decision, error)
}

// PolicyFunc adapts a function to Policy.
type PolicyFunc func(ctx context.Context, rc *RunContext) (Decision, error)

// Decide implements Policy.
func (f PolicyFunc) Decide(ctx context.Context, rc *RunContext) (Decision, error) { return f(ctx, rc) }

// Name is the optional identifier extension points can implement so denied
// errors and hook faults can name the culprit instead of "policy #2".
type Namer interface{ Name() string }
