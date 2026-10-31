// Package graph runs sandbox sessions as nodes of a control-flow graph.
//
// It is the embedding layer between two packages that do not know about each other:
// pkg/hno/sandbox, which isolates execution and demands human approval for work a policy
// refuses to run unattended, and pkg/hno/graph, which schedules nodes and suspends a run
// awaiting human input. The sandbox already asks; the graph already waits. What neither
// could do alone is carry the question across the boundary, so that a human answer to the
// graph's interrupt authorises the sandbox's suspended request and the work then runs in
// the environment the human was shown.
//
// The mapping is deliberately narrow. This package adds no scheduler, no second HITL
// mechanism, and no redefinition of pkg/hno/graph semantics: it translates
// *sandbox.ApprovalPendingError into coregraph.Interrupt and turns the human's response
// back into Session.Approve or Session.Deny, then Session.Resume.
//
// # Run identity
//
// A node pins Request.ID. That is not cosmetic. The sandbox mints a fresh run id for every
// request that arrives without one, and an approval asked without an explicit id is named
// after that run. A graph that re-enters a waiting node re-runs its body with an equivalent
// but new request, so an unpinned request would be asked a second question under a second
// id: the human's answer would refer to an identifier nobody is holding, and the sandbox
// would treat the resubmission as unapproved work. Pinning makes the identifier stable,
// which is what lets the answer discharge the obligation instead of restarting it.
//
// The node still reads the approval id from the session rather than deriving it, because a
// policy may name its own. Matching the suspended request by identity is exact; predicting
// an identifier is not.
//
// # What survives a restart
//
// pkg/hno/graph resumes a suspension in the process that created it. This package therefore
// promises the sandbox half of cross-process continuity, and says so: Ask records the
// session, the approval, and the environment the approval was bound to, and Ask.Answer
// re-acquires that session by name and spends the verdict on it. A new process can finish
// work a previous process suspended. Continuing the graph itself in a new process is the
// graph engine's concern and is out of scope here.
package graph

import (
	"context"
	"errors"
	"fmt"

	coregraph "github.com/rexleimo/agno-go/pkg/hno/graph"
	"github.com/rexleimo/agno-go/pkg/hno/sandbox"
)

// DefaultField is the response field a node reads when the caller does not rename it.
const DefaultField = "approved"

// Outcomes a node can report.
const (
	// StateRan means the work executed and Run carries the result.
	StateRan = "ran"
	// StateRefused means a human denied the work, so it never ran.
	StateRefused = "refused"
)

// Result is the value a sandbox node publishes to the graph.
//
// Downstream nodes branch on it. A completed run reports through the sandbox run contract,
// a refused one is a decision rather than a fault, and both are readable without holding a
// sandbox handle.
type Result struct {
	// Run is the sandbox report for work that ran. It is nil when the request never
	// executed: refused, or denied under a node configured to fail.
	Run *sandbox.Report
	// Refused marks a human denial. It is not an error: an approval that can be denied
	// is a decision point, and a graph that turns every denial into a failed node loses
	// the branch that was built to handle it.
	Refused bool
	// Reason carries the policy's objection or the human's verdict, whichever is the
	// last word on this node.
	Reason string
	// Ask identifies the question this node asked, so a checkpoint stream can be read
	// later and answered by another process.
	Ask Ask
	// State is one of StateRan or StateRefused.
	State string
}

// Ran reports whether the request executed.
func (r *Result) Ran() bool { return r.State == StateRan && r.Run != nil }

// ExitCode returns the process exit code, or -1 when nothing ran. Callers branching on a
// failed command should read this instead of dereferencing Run.
func (r *Result) ExitCode() int {
	if r.Run == nil {
		return -1
	}
	return r.Run.ExitCode
}

// Artifacts returns the declared outputs captured for this run, empty if none.
func (r *Result) Artifacts() []sandbox.Artifact {
	if r.Run == nil {
		return nil
	}
	return r.Run.Artifacts
}

// Ask names one sandbox approval, in terms a checkpoint can hold.
type Ask struct {
	// Node is the graph node that asked.
	Node string
	// Session is the sandbox session the work belongs to.
	Session string
	// Approval is the approval id a verdict must cite.
	Approval string
	// Lineage is the environment the human was shown when asked, empty if the request
	// suspended before anything was provisioned (the normal ordering, since a denied
	// request should cost no boot).
	Lineage string
	// Question is the policy's reason, kept so an answer recorded later can be read in
	// context by whoever finds the checkpoint.
	Question string
}

// nodeConfig holds the per-node knobs.
type nodeConfig struct {
	field    string
	message  string
	approver string
	reason   string
	fails    bool
	schema   map[string]any
}

func (c *nodeConfig) normalise(name string) {
	if c.field == "" {
		c.field = DefaultField
	}
	if c.message == "" {
		c.message = fmt.Sprintf("sandbox node %q needs approval", name)
	}
	if c.approver == "" {
		c.approver = "graph"
	}
	if c.schema == nil {
		// The decision is required; a reason is optional. readResponse already accepts a
		// reason string when present and uses it as the recorded deny reason, so the schema
		// now says what the node actually reads instead of under-declaring it.
		//
		// This was not possible before: pkg/hno/graph validated every declared property
		// against the response map, so a key nobody sent read back as nil and failed its
		// declared type -- declaring "reason" would have forced every answer to include it.
		// That defect is fixed (D-32), and the fix is only trusted because this is a real
		// consumer of it rather than a test kept honest by hand.
		c.schema = map[string]any{
			"type":     "object",
			"required": []string{c.field},
			"properties": map[string]any{
				c.field:  map[string]any{"type": "boolean"},
				"reason": map[string]any{"type": "string"},
			},
		}
	}
}

// NodeOption configures a sandbox node.
type NodeOption func(*nodeConfig)

// WithResponseField renames the boolean field the answer is read from (default "approved").
func WithResponseField(field string) NodeOption {
	return func(c *nodeConfig) { c.field = field }
}

// WithMessage sets the text a human sees at the interrupt.
func WithMessage(msg string) NodeOption {
	return func(c *nodeConfig) { c.message = msg }
}

// WithApprover names the identity recorded as answering on the graph's behalf, so an audit
// can tell "rex approved in the UI" from "the pipeline auto-answered".
func WithApprover(name string) NodeOption {
	return func(c *nodeConfig) { c.approver = name }
}

// WithDenyReason sets the reason recorded when a response denies the work.
func WithDenyReason(reason string) NodeOption {
	return func(c *nodeConfig) { c.reason = reason }
}

// WithRefusalFails turns a denial into a node error instead of a Refused result. It exists
// for graphs with no refusal branch: there, letting a denial pass as an ordinary output is
// how a pipeline reports success on work that was never done.
func WithRefusalFails() NodeOption {
	return func(c *nodeConfig) { c.fails = true }
}

// WithResponseSchema replaces the schema the graph validates answers against. The subset
// pkg/hno/graph interprets is an object with required names and property types.
func WithResponseSchema(schema map[string]any) NodeOption {
	return func(c *nodeConfig) { c.schema = schema }
}

// ErrRefused is the error a node returns when it was denied and configured to fail.
var ErrRefused = errors.New("sandboxgraph: work was refused")

// Node runs req on sess as a graph node and publishes a *Result.
//
// The node owns one run: it pins the request id, so re-entry after an answer is the same
// run rather than a fresh question. A request that already carries an ID keeps it.
func Node(name string, sess *sandbox.Session, req sandbox.Request, opts ...NodeOption) coregraph.Node {
	cfg := &nodeConfig{}
	for _, o := range opts {
		o(cfg)
	}
	cfg.normalise(name)
	if req.ID == "" {
		// Stable identity for this node's run, derived from the node name rather than a
		// counter so a resumed graph asks about the same approval even when the body runs
		// again in a different order.
		req.ID = "sandboxgraph:" + name
	}
	return &node{name: name, sess: sess, req: req, cfg: cfg}
}

type node struct {
	name string
	sess *sandbox.Session
	req  sandbox.Request
	cfg  *nodeConfig
}

// Name implements coregraph.Node.
func (n *node) Name() string { return n.name }

// Run implements coregraph.Node.
func (n *node) Run(ctx context.Context, in any) (any, error) {
	// Re-entry first. The human's answer arrives through the graph context, and reading
	// it before touching the engine is what keeps a denied request from ever running.
	if held := n.awaiting(); held != nil {
		if resp, ok := coregraph.InterruptResponse(ctx, held.ID); ok {
			res, err := n.answered(ctx, held, resp)
			if err != nil {
				return nil, err
			}
			return res, nil
		}
		// An approval that is still outstanding with no answer attached means this run
		// is being re-entered by something other than a Resume; suspending again is the
		// only honest move.
		return nil, n.suspend(held)
	}

	rep, err := n.sess.Exec(ctx, n.req)
	if err != nil {
		var pending *sandbox.ApprovalPendingError
		if errors.As(err, &pending) {
			// The session recorded the question when it refused the run, so the record is
			// the authority on its id; re-reading it also picks up the lineage the error
			// does not carry.
			if held := n.awaiting(); held != nil {
				return nil, n.suspend(held)
			}
			return nil, n.suspend(&sandbox.PendingApproval{
				ID: pending.ApprovalID, SessionID: n.sess.ID(), Request: n.req,
				Lineage: "", Reason: pending.Reason,
			})
		}
		// Anything else is a real failure: a lost environment, an unmet capability, a
		// timeout. Presenting those as an approval request would ask a human to authorise
		// work the engine could not run even with a yes.
		return nil, err
	}
	return &Result{
		Run:   rep,
		State: StateRan,
		Ask:   Ask{Node: n.name, Session: n.sess.ID()},
	}, nil
}

// awaiting finds the approval this node asked, by matching the suspended request rather
// than predicting an identifier. A policy may name its own approval id, and an id minted
// from a run id the node cannot recompute is not guessable; the session already holds the
// answer.
func (n *node) awaiting() *sandbox.PendingApproval {
	for _, p := range n.sess.Awaiting() {
		if p.Request.ID == n.req.ID || p.RunID == n.req.ID {
			out := p
			return &out
		}
	}
	return nil
}

// suspend turns a sandbox approval into a graph interrupt.
func (n *node) suspend(p *sandbox.PendingApproval) error {
	ask := Ask{
		Node:     n.name,
		Session:  n.sess.ID(),
		Approval: p.ID,
		Lineage:  p.Lineage,
		Question: p.Reason,
	}
	return coregraph.RequestInterrupt(coregraph.Interrupt{
		InterruptID:    p.ID,
		Message:        n.cfg.message,
		ResponseSchema: n.cfg.schema,
		Payload:        ask,
		Mode:           coregraph.ResumeRerun,
	})
}

// answered spends the human's verdict on the approval and runs, or does not, accordingly.
func (n *node) answered(ctx context.Context, p *sandbox.PendingApproval, resp any) (*Result, error) {
	approved, reason, err := readResponse(resp, n.cfg.field)
	if err != nil {
		return nil, fmt.Errorf("sandboxgraph: node %q: %w", n.name, err)
	}
	if !approved {
		deny := reason
		if deny == "" {
			deny = n.cfg.reason
		}
		if deny == "" {
			deny = "refused at the graph node"
		}
		if err := n.sess.Deny(ctx, p.ID, deny); err != nil {
			return nil, fmt.Errorf("sandboxgraph: node %q deny: %w", n.name, err)
		}
		if n.cfg.fails {
			return nil, fmt.Errorf("sandboxgraph: node %q: %w: %s", n.name, ErrRefused, deny)
		}
		return &Result{
			State:   StateRefused,
			Refused: true,
			Reason:  deny,
			Ask:     n.askFor(p),
		}, nil
	}
	if err := n.sess.Approve(ctx, p.ID, n.cfg.approver); err != nil {
		return nil, fmt.Errorf("sandboxgraph: node %q approve: %w", n.name, err)
	}
	rep, err := n.sess.Resume(ctx, p.ID)
	if err != nil {
		// A refusal to spend the approval is reported as itself. Swallowing it into a
		// Refused result would tell the graph a human said no when what happened is that
		// the box the human said yes about is gone.
		return nil, fmt.Errorf("sandboxgraph: node %q resume: %w", n.name, err)
	}
	return &Result{
		Run:    rep,
		State:  StateRan,
		Reason: reason,
		Ask:    n.askFor(p),
	}, nil
}

func (n *node) askFor(p *sandbox.PendingApproval) Ask {
	return Ask{
		Node:     n.name,
		Session:  n.sess.ID(),
		Approval: p.ID,
		Lineage:  p.Lineage,
		Question: p.Reason,
	}
}

// readResponse accepts the shapes a graph response can take. A bare bool is the answer
// itself; an object carries the configured field, and an optional reason string.
func readResponse(resp any, field string) (approved bool, reason string, err error) {
	switch v := resp.(type) {
	case bool:
		return v, "", nil
	case map[string]any:
		raw, ok := v[field]
		if !ok {
			return false, "", fmt.Errorf("response has no %q field", field)
		}
		switch b := raw.(type) {
		case bool:
			approved = b
		case string:
			// JSON round-tripping can deliver a string; accepting "true"/"false" keeps a
			// checkpoint answered from another process working, while anything else is an
			// error rather than a silent no.
			switch b {
			case "true":
				approved = true
			case "false":
				approved = false
			default:
				return false, "", fmt.Errorf("field %q is %q, not a yes or no", field, b)
			}
		default:
			return false, "", fmt.Errorf("field %q is %T, not a yes or no", field, raw)
		}
		if r, ok := v["reason"].(string); ok {
			reason = r
		}
		return approved, reason, nil
	default:
		return false, "", fmt.Errorf("response is %T, want a boolean or an object", resp)
	}
}

// Bound reports whether this question was asked about a specific environment.
//
// A request suspended before provisioning has no box to return to, which is the normal
// ordering: a denied request should cost no boot. Recording the distinction lets a reader
// of a checkpoint know whether answering means "run in that box" or only "run this work".
func (a Ask) Bound() bool { return a.Lineage != "" }
