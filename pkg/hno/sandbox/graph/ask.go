package graph

import (
	"context"
	"errors"
	"fmt"

	coregraph "github.com/rexleimo/agno-go/pkg/hno/graph"
	"github.com/rexleimo/agno-go/pkg/hno/sandbox"
)

// FromCheckpoint recovers the question a waiting node asked from a graph checkpoint entry.
//
// The graph records an EntryInterrupt holding the full Interrupt, and the Interrupt's payload
// is the Ask this package wrote. A process that reads a checkpoint stream can therefore find
// the approvals nobody has answered yet, which is the difference between a pipeline that can
// be finished after a restart and one whose suspended work is lost with the process.
//
// It reports false for completion entries and for entries whose payload came from something
// other than this package, so a mixed graph does not produce phantom approvals.
func FromCheckpoint(cp coregraph.Checkpoint) (*Ask, bool) {
	if cp.Kind != coregraph.EntryInterrupt || cp.Interrupt == nil {
		return nil, false
	}
	ask, ok := cp.Interrupt.Payload.(Ask)
	if !ok || ask.Approval == "" {
		return nil, false
	}
	if ask.Node != "" && cp.Node != "" && ask.Node != cp.Node {
		// A payload naming a different node than the entry does is not this node's
		// question; refusing beats attributing an answer to the wrong work.
		return nil, false
	}
	return &ask, true
}

// Outstanding lists the unanswered approvals in a checkpoint stream, in entry order.
func Outstanding(entries []coregraph.Checkpoint) []Ask {
	var out []Ask
	for _, cp := range entries {
		if ask, ok := FromCheckpoint(cp); ok {
			out = append(out, *ask)
		}
	}
	return out
}

// ErrNoSession is returned when a recorded session cannot be re-acquired. It is reserved for
// a genuine lookup failure: an approval that has already been answered reports ErrNoApproval,
// because nothing here proves the session is gone.
var ErrNoSession = errors.New("sandboxgraph: session is not available")

// ErrNoApproval is returned when the session is reachable but holds no such approval: the
// question was already answered, expired, or belongs to another handle.
var ErrNoApproval = errors.New("sandboxgraph: approval is not outstanding")

// Answer spends a verdict recorded against this Ask on the session it belongs to.
//
// It is the cross-process half of the loop: the approval id, the session name, and the
// environment the human was shown are all in the record, so a different process can finish
// the work without holding the graph that suspended. The session is re-acquired without
// creating it, because creating a fresh session here would hand back an empty box and let
// the approval be spent on a different environment than the one that was asked about.
//
// The sandbox's own invariant still applies underneath: if the environment changed between
// the question and this answer, Resume refuses and the approval is not consumed.
func (a Ask) Answer(ctx context.Context, sb *sandbox.Sandbox, approved bool, who string) (*Result, error) {
	if a.Approval == "" {
		return nil, errors.New("sandboxgraph: answer needs an approval id")
	}
	if sb == nil {
		return nil, errors.New("sandboxgraph: answer needs a sandbox handle")
	}
	if a.Session == "" {
		return nil, errors.New("sandboxgraph: answer needs a session name")
	}
	// Cheap existence guard before touching the session machinery. It is deliberately not
	// presented as proof of this particular approval: the handle reports a count, so a
	// zero count rules this handle out, while a nonzero count still needs the precise check
	// below. The reason to bother is cost, not correctness: Session() with
	// WithCreateIfMissing(false) does not refuse an unknown id on a store-less handle, so
	// answering a stale record would boot a fresh environment for a box nobody asked for.
	// The error names what the guard actually established: this approval is not outstanding
	// here. It says nothing about whether the session is alive, and claiming otherwise would
	// send a caller off to look for a missing box when the question was already answered.
	if diag, err := sb.Diagnose(ctx); err == nil && !holdsAnyApproval(diag) {
		return nil, fmt.Errorf("%w %q: this handle has no outstanding approvals at all", ErrNoApproval, a.Approval)
	}
	sess, err := sb.Session(ctx, a.Session, sandbox.WithCreateIfMissing(false))
	if err != nil {
		return nil, fmt.Errorf("sandboxgraph: re-acquire session %q: %w", a.Session, err)
	}
	// The approval book is the authority on whether this question is still open here.
	if !openApproval(sess, a.Approval) {
		return nil, fmt.Errorf("%w %q for session %q: it is not in Session.Awaiting()", ErrNoApproval, a.Approval, a.Session)
	}
	if !approved {
		reason := who
		if reason == "" {
			reason = "refused out of band"
		}
		if err := sess.Deny(ctx, a.Approval, reason); err != nil {
			return nil, fmt.Errorf("sandboxgraph: deny %q: %w", a.Approval, err)
		}
		return &Result{State: StateRefused, Refused: true, Reason: reason, Ask: a}, nil
	}

	res, err := askAncestor(ctx, sess, a, who)
	if err != nil {
		return nil, err
	}
	return res, nil
}

// askAncestor spends an approval and resumes the recorded request.
func askAncestor(ctx context.Context, sess *sandbox.Session, a Ask, who string) (*Result, error) {
	if err := sess.Approve(ctx, a.Approval, who); err != nil {
		var unknown *sandbox.UnknownApprovalError
		if errors.As(err, &unknown) {
			return nil, fmt.Errorf("%w %q: %w", ErrNoApproval, a.Approval, err)
		}
		return nil, fmt.Errorf("sandboxgraph: approve %q: %w", a.Approval, err)
	}
	rep, err := sess.Resume(ctx, a.Approval)
	if err != nil {
		return nil, fmt.Errorf("sandboxgraph: resume %q: %w", a.Approval, err)
	}
	return &Result{Run: rep, State: StateRan, Ask: a}, nil
}

// holdsAnyApproval reads the handle's approval count out of a diagnostic report.
func holdsAnyApproval(diag *sandbox.DiagReport) bool {
	if diag == nil {
		return true
	}
	for _, c := range diag.Checks {
		if c.Env == "approvals" {
			return !c.OK
		}
	}
	return false
}

// openApproval checks the approval book directly, which is the only precise answer to
// "is this question still open here".
func openApproval(sess *sandbox.Session, id string) bool {
	for _, p := range sess.Awaiting() {
		if p.ID == id {
			return true
		}
	}
	return false
}
