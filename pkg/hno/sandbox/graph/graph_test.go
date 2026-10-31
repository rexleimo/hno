package graph_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	coregraph "github.com/rexleimo/agno-go/pkg/hno/graph"
	"github.com/rexleimo/agno-go/pkg/hno/sandbox"
	sgraph "github.com/rexleimo/agno-go/pkg/hno/sandbox/graph"
	"github.com/rexleimo/agno-go/pkg/hno/sandbox/sandboxtest"
)

// policyOn demands approval for any request whose code mentions trigger.
func policyOn(trigger string) sandbox.Policy {
	return sandbox.PolicyFunc(func(_ context.Context, rc *sandbox.RunContext) (sandbox.Decision, error) {
		if strings.Contains(rc.Request.Code, trigger) {
			return sandbox.Decision{
				Action: sandbox.ActRequireApproval,
				Reason: "node work touching " + trigger,
				Policy: "guard-" + trigger,
			}, nil
		}
		return sandbox.Decision{Action: sandbox.ActAllow}, nil
	})
}

func openSandbox(t *testing.T, opts ...sandbox.Option) *sandbox.Sandbox {
	t.Helper()
	if err := sandboxtest.Register(); err != nil {
		t.Fatalf("register provider: %v", err)
	}
	base := []sandbox.Option{
		sandbox.WithProvider("memory"),
		sandbox.WithAllowedProviders("memory"),
		sandbox.WithImage("test:latest"),
	}
	sb, err := sandbox.Open(context.Background(), append(base, opts...)...)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = sb.Close() })
	return sb
}

func session(t *testing.T, sb *sandbox.Sandbox, id string) *sandbox.Session {
	t.Helper()
	sess, err := sb.Session(context.Background(), id, sandbox.WithCreateIfMissing(true))
	if err != nil {
		t.Fatalf("Session %q: %v", id, err)
	}
	return sess
}

// build wires entry -> work -> done so the graph has somewhere to go after the answer.
func build(work coregraph.Node) *coregraph.Graph {
	g := coregraph.New()
	g.AddNode(coregraph.NodeFunc("entry", func(_ context.Context, in any) (any, error) { return "go", nil }))
	g.AddNode(work)
	g.AddNode(coregraph.NodeFunc("done", func(_ context.Context, in any) (any, error) {
		res, ok := in.(*sgraph.Result)
		if !ok {
			return nil, errors.New("done: downstream lost the sandbox result")
		}
		return res, nil
	}))
	g.AddEdge("entry", "work")
	g.AddEdge("work", "done")
	g.SetEntry("entry")
	g.SetOutput("done")
	if err := g.Validate(); err != nil {
		panic(err)
	}
	return g
}

// TestNodeSuspendsAsAGraphInterrupt is the completion gate's first half: the sandbox's
// question must become the graph's waiting state, not a failed node.
func TestNodeSuspendsAsAGraphInterrupt(t *testing.T) {
	ctx := context.Background()
	sb := openSandbox(t, sandbox.WithPolicy(policyOn("deploy")))
	sess := session(t, sb, "g1")

	g := build(sgraph.Node("work", sess, sandbox.Shell("export deploy=1")))
	_, err := g.Run(ctx, nil)
	if !errors.Is(err, coregraph.ErrSuspended) {
		t.Fatalf("an approval request must suspend the graph, got %v", err)
	}
	var susp *coregraph.Suspension
	if !errors.As(err, &susp) {
		t.Fatalf("want *graph.Suspension, got %T", err)
	}
	if len(susp.Interrupts) != 1 {
		t.Fatalf("want one waiting node, got %d", len(susp.Interrupts))
	}
	ask, ok := susp.Interrupts[0].Interrupt.Payload.(sgraph.Ask)
	if !ok {
		t.Fatalf("the interrupt payload must be a sandboxgraph.Ask, got %T", susp.Interrupts[0].Interrupt.Payload)
	}
	if ask.Session != "g1" || ask.Node != "work" || ask.Approval == "" {
		t.Fatalf("the record needs session, node and approval to be answerable later, got %+v", ask)
	}
	if !strings.Contains(ask.Question, "deploy") {
		t.Fatalf("the policy's reason should travel with the question, got %q", ask.Question)
	}
}

// TestApprovedRunCompletesInTheSameBox is the second half: the answer authorises the work,
// and it lands in the environment the human was shown.
func TestApprovedRunCompletesInTheSameBox(t *testing.T) {
	ctx := context.Background()
	sb := openSandbox(t, sandbox.WithPolicy(policyOn("deploy")))
	sess := session(t, sb, "g2")
	// Warm the box so the approval is bound to a real environment.
	if _, err := sess.Exec(ctx, sandbox.Shell("export warm=1")); err != nil {
		t.Fatalf("warm: %v", err)
	}

	g := build(sgraph.Node("work", sess, sandbox.Shell("export deploy=1")))
	_, err := g.Run(ctx, nil)
	var susp *coregraph.Suspension
	if !errors.As(err, &susp) {
		t.Fatalf("want suspension, got %v", err)
	}
	id := susp.Interrupts[0].Interrupt.InterruptID

	res, err := g.Resume(ctx, map[string]any{id: map[string]any{"approved": true}})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	out, ok := res.Output().(*sgraph.Result)
	if !ok {
		t.Fatalf("output must be *sandboxgraph.Result, got %T", res.Output())
	}
	if !out.Ran() || out.ExitCode() != 0 {
		t.Fatalf("approved work must run and report, got %+v", out)
	}
	// Nothing is left parked: the graph and the sandbox agree the question is closed.
	if left := sess.Awaiting(); len(left) != 0 {
		t.Fatalf("a spent approval must not remain outstanding, got %d", len(left))
	}
}

// TestDeniedWorkNeverRuns checks a denial is a branch, not a fault, and that the request
// never reached the engine.
func TestDeniedWorkNeverRuns(t *testing.T) {
	ctx := context.Background()
	sb := openSandbox(t, sandbox.WithPolicy(policyOn("deploy")))
	sess := session(t, sb, "g3")

	g := build(sgraph.Node("work", sess, sandbox.Shell("export deploy=1")))
	_, err := g.Run(ctx, nil)
	var susp *coregraph.Suspension
	if !errors.As(err, &susp) {
		t.Fatalf("want suspension, got %v", err)
	}
	id := susp.Interrupts[0].Interrupt.InterruptID
	res, err := g.Resume(ctx, map[string]any{id: map[string]any{"approved": false, "reason": "no window"}})
	if err != nil {
		t.Fatalf("a denial must not fail the graph: %v", err)
	}
	out := res.Output().(*sgraph.Result)
	if !out.Refused || out.Ran() {
		t.Fatalf("want a refusal with nothing run, got %+v", out)
	}
	if !strings.Contains(out.Reason, "no window") {
		t.Fatalf("the human's reason should reach downstream nodes, got %q", out.Reason)
	}
	// And the sandbox agrees: the approval is answered, not pending.
	if left := sess.Awaiting(); len(left) != 0 {
		t.Fatalf("denied approval still outstanding: %d", len(left))
	}
}

// TestRefusalFailsOption covers graphs with no refusal branch.
func TestRefusalFailsOption(t *testing.T) {
	ctx := context.Background()
	sb := openSandbox(t, sandbox.WithPolicy(policyOn("deploy")))
	sess := session(t, sb, "g4")

	g := build(sgraph.Node("work", sess, sandbox.Shell("export deploy=1"), sgraph.WithRefusalFails()))
	_, err := g.Run(ctx, nil)
	var susp *coregraph.Suspension
	if !errors.As(err, &susp) {
		t.Fatalf("want suspension, got %v", err)
	}
	id := susp.Interrupts[0].Interrupt.InterruptID
	_, err = g.Resume(ctx, map[string]any{id: map[string]any{"approved": false}})
	if !errors.Is(err, sgraph.ErrRefused) {
		t.Fatalf("WithRefusalFails must surface a refusal as an error, got %v", err)
	}
}

// TestRunExecutesExactlyOnce is the re-entry trap: a waiting node that re-runs its body
// after an answer must not run the request twice.
func TestRunExecutesExactlyOnce(t *testing.T) {
	ctx := context.Background()
	var runs int
	events := sandbox.ObserverFunc(func(_ context.Context, ev sandbox.Event) error {
		if ev.Kind == sandbox.EventPhaseStarted {
			runs++
		}
		return nil
	})
	sb := openSandbox(t, sandbox.WithPolicy(policyOn("deploy")), sandbox.WithObserver(events))
	sess := session(t, sb, "g5")

	g := build(sgraph.Node("work", sess, sandbox.Shell("export deploy=1")))
	_, err := g.Run(ctx, nil)
	if !errors.Is(err, coregraph.ErrSuspended) {
		t.Fatalf("want suspension, got %v", err)
	}
	if runs != 0 {
		t.Fatalf("a suspended request must not have reached the engine, saw %d runs", runs)
	}
	var susp *coregraph.Suspension
	if !errors.As(err, &susp) {
		t.Fatalf("want suspension product, got %T", err)
	}
	id := susp.Interrupts[0].Interrupt.InterruptID
	if _, err := g.Resume(ctx, map[string]any{id: map[string]any{"approved": true}}); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if runs != 1 {
		t.Fatalf("the approved run must execute once, got %d", runs)
	}
}

// TestChangedBoxRefusesTheOldGrant inherits the approval invariant: an answer given about
// one environment cannot be spent on another.
func TestChangedBoxRefusesTheOldGrant(t *testing.T) {
	ctx := context.Background()
	sb := openSandbox(t,
		sandbox.WithPolicy(policyOn("deploy")),
		sandbox.WithRetryMode(sandbox.RebuildFromSpec),
	)
	sess := session(t, sb, "g6")
	if _, err := sess.Exec(ctx, sandbox.Shell("export warm=1")); err != nil {
		t.Fatalf("warm: %v", err)
	}

	g := build(sgraph.Node("work", sess, sandbox.Shell("export deploy=1")))
	_, err := g.Run(ctx, nil)
	var susp *coregraph.Suspension
	if !errors.As(err, &susp) {
		t.Fatalf("want suspension, got %v", err)
	}
	id := susp.Interrupts[0].Interrupt.InterruptID
	ask := susp.Interrupts[0].Interrupt.Payload.(sgraph.Ask)
	if !ask.Bound() {
		t.Fatal("an approval taken while a box was live must record that box")
	}

	// The environment dies and is replaced before the answer is spent. Marking the loss is what
	// makes it real for the session: dropping only the provider's copy leaves the session holding
	// the same handle, and then there is no second box for the invariant to refuse.
	sandboxtest.DropEnvironment(t, "g6")
	sess.MarkLost(errors.New("gate: environment dropped"))
	if _, err := sess.Exec(ctx, sandbox.Shell("export other=1")); err != nil {
		t.Fatalf("re-provision: %v", err)
	}

	_, err = g.Resume(ctx, map[string]any{id: map[string]any{"approved": true}})
	if err == nil {
		t.Fatal("approved work must not run in a box the approval never saw")
	}
	var changed *sandbox.ApprovalEnvironmentChangedError
	if !errors.As(err, &changed) {
		t.Fatalf("want *sandbox.ApprovalEnvironmentChangedError, got %T: %v", err, err)
	}
}

// TestErrorIsNotAnApproval pins two shapes a naive node would merge. A command that exits
// non-zero is a result the graph should read, while an engine that cannot run at all is an
// error: presenting the second as an approval request would ask a human to authorise work the
// engine could not perform even with a yes.
func TestEngineFailureIsNotAnApproval(t *testing.T) {
	ctx := context.Background()
	sb := openSandbox(t)
	sess := session(t, sb, "g7")

	// A failing command is data under the run contract.
	g := build(sgraph.Node("work", sess, sandbox.Shell("#!fail boom")))
	res, err := g.Run(ctx, nil)
	if errors.Is(err, coregraph.ErrSuspended) {
		t.Fatal("a failed run is not a request for permission")
	}
	if err != nil {
		t.Fatalf("a non-zero exit is data under the run contract, got error %v", err)
	}
	if out := res.Output().(*sgraph.Result); out.ExitCode() == 0 {
		t.Fatalf("the failing exit code must reach the graph, got %+v", out)
	}

	// An unusable environment is an error, and must not be queued for a human.
	broken := session(t, sb, "g7b")
	if err := broken.Release(ctx); err != nil {
		t.Fatalf("Release: %v", err)
	}
	g2 := coregraph.New()
	g2.AddNode(sgraph.Node("work", broken, sandbox.Shell("export any=1")))
	g2.SetEntry("work")
	g2.SetOutput("work")
	if err := g2.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	_, err = g2.Run(ctx, nil)
	if errors.Is(err, coregraph.ErrSuspended) {
		t.Fatal("an unusable environment must not reach a human as an approval")
	}
	if err == nil {
		t.Fatal("want the engine failure to reach the graph")
	}
}

// TestAnswerFromCheckpoint is the cross-process half: a fresh handle, only the recorded
// question, and the work still finishes in the session it started in.
func TestAnswerFromCheckpoint(t *testing.T) {
	ctx := context.Background()
	sb := openSandbox(t, sandbox.WithPolicy(policyOn("deploy")))
	sess := session(t, sb, "g8")
	if _, err := sess.Exec(ctx, sandbox.Shell("export warm=1")); err != nil {
		t.Fatalf("warm: %v", err)
	}
	g := build(sgraph.Node("work", sess, sandbox.Shell("export deploy=1")))
	_, err := g.Run(ctx, nil)
	var susp *coregraph.Suspension
	if !errors.As(err, &susp) {
		t.Fatalf("want suspension, got %v", err)
	}

	// Persist the interrupt entry the way a Checkpointer would see it.
	entries := []coregraph.Checkpoint{{
		Seq: 1, Node: "work", Kind: coregraph.EntryInterrupt,
		Interrupt: &susp.Interrupts[0].Interrupt,
	}}
	asked := sgraph.Outstanding(entries)
	if len(asked) != 1 {
		t.Fatalf("the checkpoint stream should show one unanswered approval, got %d", len(asked))
	}

	// A second handle stands in for a new process. It must not create the session: an
	// empty box would let the answer be spent on the wrong environment.
	fresh := openSandbox(t, sandbox.WithPolicy(policyOn("deploy")))
	if _, err := asked[0].Answer(ctx, fresh, true, "second-process"); err == nil {
		t.Fatal("a handle that does not hold the session must say so rather than answer blindly")
	} else if !errors.Is(err, sgraph.ErrNoSession) {
		t.Fatalf("want ErrNoSession, got %v", err)
	} else if !strings.Contains(err.Error(), "cannot be verified as a resumption") {
		t.Fatalf("the refusal should come from the core's own check, not from this package's guess, got %q", err)
	}

	res, err := asked[0].Answer(ctx, sb, true, "second-process")
	if err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if !res.Ran() || res.ExitCode() != 0 {
		t.Fatalf("the recorded work must complete, got %+v", res)
	}
	if left := sess.Awaiting(); len(left) != 0 {
		t.Fatalf("the answered approval must be closed, got %d left", len(left))
	}
}

// TestAnswerRefusesToInventASession checks the guard that keeps a re-acquire from silently
// creating an empty box.
func TestAnswerRefusesToInventASession(t *testing.T) {
	ctx := context.Background()
	sb := openSandbox(t)
	ask := sgraph.Ask{Node: "work", Session: "never-created", Approval: "ap-1"}
	_, err := ask.Answer(ctx, sb, true, "x")
	if !errors.Is(err, sgraph.ErrNoSession) {
		t.Fatalf("a session nobody can verify must not be answered, got %v", err)
	}
	// Asserted against the core directly too, so this package's guarantee does not rest only
	// on the adapter's mapping: the same id asked of the handle is refused as unverifiable.
	// This is the defect that used to force a pre-check here; it is now a tested contract.
	if _, err := sb.Session(ctx, "never-created", sandbox.WithCreateIfMissing(false)); err == nil {
		t.Fatal("core: WithCreateIfMissing(false) must refuse an unknown id even with no session store")
	} else {
		var stale *sandbox.StaleSessionError
		if !errors.As(err, &stale) {
			t.Fatalf("core: want *StaleSessionError, got %T: %v", err, err)
		}
	}
}

// TestPinnedRunIdentityIsStable proves the reason the node owns a run id: the same request
// re-entered is the same approval. Without the pin, the id is minted fresh per call, so a
// graph that re-enters a waiting node would be holding an answer to a question nobody asked.
func TestPinnedRunIdentityIsStable(t *testing.T) {
	ctx := context.Background()
	sb := openSandbox(t, sandbox.WithPolicy(policyOn("deploy")))
	sess := session(t, sb, "g9")

	// Control: an unpinned request asked twice produces two different questions. This is
	// what the node must avoid, and asserting it here is what makes the second half of the
	// test mean something rather than pass by construction.
	ids := map[string]bool{}
	unpinned := sandbox.Shell("export deploy=1")
	for i := 0; i < 2; i++ {
		_, err := sess.Exec(ctx, unpinned)
		var pending *sandbox.ApprovalPendingError
		if !errors.As(err, &pending) {
			t.Fatalf("run %d: want pending, got %v", i, err)
		}
		ids[pending.ApprovalID] = true
	}
	if len(ids) != 2 {
		t.Fatalf("control failed: an unpinned request should mint a fresh approval id per run, got %v", ids)
	}

	// Through the node, two independent graph runs must ask the same question.
	seen := []string{}
	for i := 0; i < 2; i++ {
		g := build(sgraph.Node("work", sess, sandbox.Shell("export deploy=1")))
		_, err := g.Run(ctx, nil)
		var susp *coregraph.Suspension
		if !errors.As(err, &susp) {
			t.Fatalf("graph run %d: want suspension, got %v", i, err)
		}
		seen = append(seen, susp.Interrupts[0].Interrupt.InterruptID)
	}
	if seen[0] != seen[1] {
		t.Fatalf("a node must ask the same question on re-entry, got %q then %q", seen[0], seen[1])
	}
}

// TestResponseShapeTolerance covers a checkpoint answered out of band, where the verdict may
// arrive as a string after a JSON round trip.
func TestResponseShapeTolerance(t *testing.T) {
	ctx := context.Background()
	sb := openSandbox(t, sandbox.WithPolicy(policyOn("deploy")))
	sess := session(t, sb, "g10")
	g := build(sgraph.Node("work", sess, sandbox.Shell("export deploy=1")))
	_, err := g.Run(ctx, nil)
	var susp *coregraph.Suspension
	if !errors.As(err, &susp) {
		t.Fatalf("want suspension, got %v", err)
	}
	id := susp.Interrupts[0].Interrupt.InterruptID
	// The graph validates types before the node sees the answer, so a wrong shape is
	// refused there rather than silently read as a no.
	if _, err := g.Resume(ctx, map[string]any{id: map[string]any{"approved": "maybe"}}); err == nil {
		t.Fatal("a non-boolean answer must not be accepted")
	} else {
		time.Sleep(0)
	}
}

// TestMalformedReasonIsNowRejected records the side of D-32 that tightens. Before, "reason"
// was not declared at all, so any value under that key was accepted unchecked and silently
// dropped by readResponse when it was not a string. Declaring it means the graph validates it:
// a non-string reason is refused instead of quietly disappearing, and the question stays open.
func TestMalformedReasonIsNowRejected(t *testing.T) {
	ctx := context.Background()
	sb := openSandbox(t, sandbox.WithPolicy(policyOn("deploy")))
	sess := session(t, sb, "g-reason-type")

	g := build(sgraph.Node("work", sess, sandbox.Shell("export deploy=1")))
	_, err := g.Run(ctx, nil)
	var susp *coregraph.Suspension
	if !errors.As(err, &susp) {
		t.Fatalf("want suspension, got %v", err)
	}
	id := susp.Interrupts[0].Interrupt.InterruptID

	if _, err := g.Resume(ctx, map[string]any{id: map[string]any{
		"approved": true, "reason": 42,
	}}); !errors.Is(err, coregraph.ErrInvalidResponse) {
		t.Fatalf("a non-string reason must now be refused, got %v", err)
	}
	// The bad answer did not spend the approval: it can still be answered correctly.
	if left := sess.Awaiting(); len(left) != 1 {
		t.Fatalf("a rejected answer must leave the question open, got %d left", len(left))
	}
	res, err := g.Resume(ctx, map[string]any{id: map[string]any{
		"approved": true, "reason": "window opened",
	}})
	if err != nil {
		t.Fatalf("corrected answer: %v", err)
	}
	if out := res.Output().(*sgraph.Result); !out.Ran() {
		t.Fatalf("the corrected answer must run the work, got %+v", out)
	}
}

// TestAnswerWithoutReasonIsAccepted is the other half of the same fix: an optional reason may
// be left out entirely, which is what the node promises a caller who only wants yes or no.
func TestAnswerWithoutReasonIsAccepted(t *testing.T) {
	ctx := context.Background()
	sb := openSandbox(t, sandbox.WithPolicy(policyOn("deploy")))
	sess := session(t, sb, "g-no-reason")

	g := build(sgraph.Node("work", sess, sandbox.Shell("export deploy=1")))
	_, err := g.Run(ctx, nil)
	var susp *coregraph.Suspension
	if !errors.As(err, &susp) {
		t.Fatalf("want suspension, got %v", err)
	}
	id := susp.Interrupts[0].Interrupt.InterruptID
	res, err := g.Resume(ctx, map[string]any{id: map[string]any{"approved": true}})
	if err != nil {
		t.Fatalf("an answer with only the decision must be accepted: %v", err)
	}
	if out := res.Output().(*sgraph.Result); !out.Ran() {
		t.Fatalf("want the work to run, got %+v", out)
	}
}
