package graph_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	coregraph "github.com/rexleimo/agno-go/pkg/hno/graph"
	"github.com/rexleimo/agno-go/pkg/hno/sandbox"
	sgraph "github.com/rexleimo/agno-go/pkg/hno/sandbox/graph"
)

// TestCustomFieldAndApprover covers the two options that change what a human answers and who
// is recorded as answering it.
func TestCustomFieldAndApprover(t *testing.T) {
	ctx := context.Background()
	sb := openSandbox(t, sandbox.WithPolicy(policyOn("deploy")))
	sess := session(t, sb, "c1")

	g := build(sgraph.Node("work", sess, sandbox.Shell("export deploy=1"),
		sgraph.WithResponseField("go_ahead"),
		sgraph.WithApprover("release-bot"),
		sgraph.WithMessage("ship it?")))
	_, err := g.Run(ctx, nil)
	var susp *coregraph.Suspension
	if !errors.As(err, &susp) {
		t.Fatalf("want suspension, got %v", err)
	}
	id := susp.Interrupts[0].Interrupt.InterruptID
	if susp.Interrupts[0].Interrupt.Message != "ship it?" {
		t.Fatalf("the message a human reads should be the one configured, got %q", susp.Interrupts[0].Interrupt.Message)
	}
	// The default field must no longer be accepted: renaming the question is only useful
	// if answering the old name stops working.
	if _, err := g.Resume(ctx, map[string]any{id: map[string]any{"approved": true}}); err == nil {
		t.Fatal("WithResponseField must govern what the graph validates")
	}
	if _, err := g.Resume(ctx, map[string]any{id: map[string]any{"go_ahead": true}}); err != nil {
		t.Fatalf("Resume with the configured field: %v", err)
	}
	out, ok := g2out(t, ctx, sb, "c1")
	_ = out
	if !ok {
		t.Fatal("the run should have completed")
	}
}

// g2out re-reads a session's approvals to confirm none are outstanding.
func g2out(t *testing.T, ctx context.Context, sb *sandbox.Sandbox, id string) (string, bool) {
	t.Helper()
	sess, err := sb.Session(ctx, id)
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if n := len(sess.Awaiting()); n != 0 {
		return "", false
	}
	return "clear", true
}

// TestFromCheckpointIgnoresForeignEntries checks the recovery reader does not invent
// approvals from entries that were never questions.
func TestFromCheckpointIgnoresForeignEntries(t *testing.T) {
	completion := coregraph.Checkpoint{Seq: 1, Node: "work", Output: "done"}
	if _, ok := sgraph.FromCheckpoint(completion); ok {
		t.Fatal("a completion entry is not an outstanding approval")
	}
	otherPayload := coregraph.Checkpoint{
		Seq: 2, Node: "work", Kind: coregraph.EntryInterrupt,
		Interrupt: &coregraph.Interrupt{InterruptID: "x", Payload: "not an ask"},
	}
	if _, ok := sgraph.FromCheckpoint(otherPayload); ok {
		t.Fatal("a payload from another package must not be read as a sandbox approval")
	}
	// A node mismatch means the payload and the entry disagree about who asked.
	mismatch := coregraph.Checkpoint{
		Seq: 3, Node: "different", Kind: coregraph.EntryInterrupt,
		Interrupt: &coregraph.Interrupt{
			InterruptID: "ap",
			Payload:     sgraph.Ask{Node: "work", Session: "s", Approval: "ap"},
		},
	}
	if _, ok := sgraph.FromCheckpoint(mismatch); ok {
		t.Fatal("attributing an answer to the wrong node is worse than finding nothing")
	}
	matched := mismatch
	matched.Node = "work"
	ask, ok := sgraph.FromCheckpoint(matched)
	if !ok || ask.Session != "s" {
		t.Fatalf("the matching entry should be recoverable, got %+v ok=%v", ask, ok)
	}
	if len(sgraph.Outstanding([]coregraph.Checkpoint{completion, otherPayload, mismatch, matched})) != 1 {
		t.Fatal("Outstanding must count exactly the answerable questions")
	}
}

// TestAnswerDenyIsNotAnError covers the out-of-band denial path.
func TestAnswerDenyIsNotAnError(t *testing.T) {
	ctx := context.Background()
	sb := openSandbox(t, sandbox.WithPolicy(policyOn("deploy")))
	sess := session(t, sb, "c2")
	g := build(sgraph.Node("work", sess, sandbox.Shell("export deploy=1")))
	_, err := g.Run(ctx, nil)
	var susp *coregraph.Suspension
	if !errors.As(err, &susp) {
		t.Fatalf("want suspension, got %v", err)
	}
	ask := susp.Interrupts[0].Interrupt.Payload.(sgraph.Ask)
	res, err := ask.Answer(ctx, sb, false, "on-call")
	if err != nil {
		t.Fatalf("an out-of-band denial is a decision, not a fault: %v", err)
	}
	if !res.Refused || res.Ran() {
		t.Fatalf("want Refused with nothing run, got %+v", res)
	}
	if !strings.Contains(res.Reason, "on-call") {
		t.Fatalf("whoever answered should be recorded, got %q", res.Reason)
	}
	// Answering the same question twice is now an unknown approval, not a second verdict.
	if _, err := ask.Answer(ctx, sb, true, "again"); !errors.Is(err, sgraph.ErrNoApproval) {
		t.Fatalf("want ErrNoApproval on a spent question, got %v", err)
	}
}

// TestAnswerNeedsItsRecord covers the degenerate Ask values, which a hand-built checkpoint
// can easily produce.
func TestAnswerNeedsItsRecord(t *testing.T) {
	ctx := context.Background()
	sb := openSandbox(t)
	if _, err := (sgraph.Ask{Session: "s"}).Answer(ctx, sb, true, "x"); err == nil {
		t.Fatal("an Ask without an approval id cannot be answered")
	}
	if _, err := (sgraph.Ask{Approval: "a"}).Answer(ctx, sb, true, "x"); err == nil {
		t.Fatal("an Ask without a session cannot be answered")
	}
	if _, err := (sgraph.Ask{Session: "s", Approval: "a"}).Answer(ctx, nil, true, "x"); err == nil {
		t.Fatal("an Ask answered against no handle cannot succeed")
	}
}

// TestBoundReflectsTheQuestion pins the distinction a reader of a checkpoint needs: whether
// the answer means "run in that box" or only "run this work".
func TestBoundReflectsTheQuestion(t *testing.T) {
	if (sgraph.Ask{Approval: "a"}).Bound() {
		t.Fatal("a question asked before anything was provisioned has no box to be bound to")
	}
	if !(sgraph.Ask{Approval: "a", Lineage: "gen:1"}).Bound() {
		t.Fatal("a question taken while a box was live must record it")
	}
}
