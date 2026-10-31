package sandbox_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rexleimo/agno-go/pkg/hno/sandbox"
)

// approvalPolicy demands a human answer for requests whose code mentions the trigger.
// The id is stable per trigger so a resubmission is recognisable as the same question.
func approvalPolicy(trigger string) sandbox.PolicyFunc {
	return func(_ context.Context, rc *sandbox.RunContext) (sandbox.Decision, error) {
		if strings.Contains(rc.Request.Code, trigger) {
			return sandbox.Decision{
				Action: sandbox.ActRequireApproval,
				Reason: "touching " + trigger,
				Policy: "guard-" + trigger,
			}, nil
		}
		return sandbox.Decision{Action: sandbox.ActAllow}, nil
	}
}

// TestApprovalSuspendAnswerResume is the B4 completion gate in miniature: a run suspends,
// a human answers, and the same work runs again in the same environment.
func TestApprovalSuspendAnswerResume(t *testing.T) {
	ctx := context.Background()
	sb := openHandle(t, sandbox.WithPolicy(approvalPolicy("DEPLOY")))
	sess, err := sb.Session(ctx, "hitl", sandbox.WithCreateIfMissing(true))
	if err != nil {
		t.Fatalf("Session: %v", err)
	}

	_, err = sess.Exec(ctx, sandbox.Shell("export DEPLOY=1"))
	if err == nil {
		t.Fatal("a policy demanding approval must not let the run through")
	}
	var pending *sandbox.ApprovalPendingError
	if !errors.As(err, &pending) {
		t.Fatalf("want *ApprovalPendingError, got %T: %v", err, err)
	}
	if pending.ApprovalID == "" {
		t.Fatal("the error must carry the id a human answers")
	}
	if len(pending.Remediation()) == 0 {
		t.Fatal("an error that says pending without saying who can un-pend it is a status line")
	}
	if sandbox.ClassOf(err) != sandbox.ClassDenied {
		t.Fatalf("a wait is denied-but-resumable, got %s", sandbox.ClassOf(err))
	}

	// The caller can see what is parked without having installed an Observer. That is the
	// difference between a workflow that reports a hang and one that only emits one.
	awaiting := sess.Awaiting()
	if len(awaiting) != 1 {
		t.Fatalf("Awaiting must show the suspended request without an Observer, got %d", len(awaiting))
	}
	if awaiting[0].ID != pending.ApprovalID {
		t.Fatalf("awaiting id = %q, want %q", awaiting[0].ID, pending.ApprovalID)
	}
	if awaiting[0].Policy != "guard-DEPLOY" {
		t.Fatalf("the record must say which policy asked, got %q", awaiting[0].Policy)
	}

	// Answering without resuming must not run anything.
	if err := sess.Approve(ctx, pending.ApprovalID, "rex"); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if got := sess.Awaiting(); len(got) != 0 {
		t.Fatalf("a granted approval is no longer outstanding, got %d", len(got))
	}

	rep, err := sess.Resume(ctx, pending.ApprovalID)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if rep == nil || rep.ExitCode != 0 {
		t.Fatalf("the approved work must actually run, got %+v", rep)
	}
	// Spent, so it cannot be replayed a second time.
	if _, err := sess.Resume(ctx, pending.ApprovalID); err == nil {
		t.Fatal("a spent approval must not authorise a second run")
	} else {
		var unk *sandbox.UnknownApprovalError
		if !errors.As(err, &unk) {
			t.Fatalf("want *UnknownApprovalError for a spent id, got %T: %v", err, err)
		}
	}
}

// TestApprovalRefusedNeverRuns pins the other answer: Deny is terminal for that request.
func TestApprovalRefusedNeverRuns(t *testing.T) {
	ctx := context.Background()
	sb := openHandle(t, sandbox.WithPolicy(approvalPolicy("DROP_TABLE")))
	sess, err := sb.Session(ctx, "deny", sandbox.WithCreateIfMissing(true))
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	_, err = sess.Exec(ctx, sandbox.Shell("export DROP_TABLE=1"))
	var pending *sandbox.ApprovalPendingError
	if !errors.As(err, &pending) {
		t.Fatalf("want pending, got %v", err)
	}
	if err := sess.Deny(ctx, pending.ApprovalID, "destructive without backup"); err != nil {
		t.Fatalf("Deny: %v", err)
	}
	_, err = sess.Resume(ctx, pending.ApprovalID)
	var refused *sandbox.ApprovalRefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("want *ApprovalRefusedError, got %T: %v", err, err)
	}
	if !strings.Contains(refused.Error(), "backup") {
		t.Fatalf("the reason a human gave should reach the caller, got %q", refused.Error())
	}
	if sandbox.ClassOf(err) != sandbox.ClassDenied {
		t.Fatalf("a refusal is a denial, got %s", sandbox.ClassOf(err))
	}
}

// TestApprovalHoldIsConsumed makes the two unread fields answer for something. Setting a
// window must change observable behaviour, or the field is decoration.
func TestApprovalHoldIsConsumed(t *testing.T) {
	ctx := context.Background()
	sb := openHandle(t,
		sandbox.WithPolicy(approvalPolicy("slow-window")),
		sandbox.WithTimeouts(sandbox.Timeouts{ApprovalHold: 30 * time.Millisecond}),
	)
	sess, err := sb.Session(ctx, "hold", sandbox.WithCreateIfMissing(true))
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	_, err = sess.Exec(ctx, sandbox.Shell("export slow-window=1"))
	var pending *sandbox.ApprovalPendingError
	if !errors.As(err, &pending) {
		t.Fatalf("want pending, got %v", err)
	}
	if pending.Hold != 30*time.Millisecond {
		t.Fatalf("the error should report the window that governs it, got %s", pending.Hold)
	}
	if pending.ExpiresAt.IsZero() {
		t.Fatal("a bounded hold must report a deadline")
	}
	time.Sleep(60 * time.Millisecond)

	// Late answers are inert, and say so.
	if err := sess.Approve(ctx, pending.ApprovalID, "too late"); err == nil {
		t.Fatal("an answer after the hold window must not be accepted as if it were timely")
	} else {
		var exp *sandbox.ApprovalExpiredError
		if !errors.As(err, &exp) {
			t.Fatalf("want *ApprovalExpiredError, got %T: %v", err, err)
		}
		if len(exp.Remediation()) == 0 {
			t.Fatal("expiry without a remedy is a tombstone")
		}
	}
	if _, err := sess.Resume(ctx, pending.ApprovalID); err == nil {
		t.Fatal("an expired approval must not authorise work")
	} else {
		var exp *sandbox.ApprovalExpiredError
		if !errors.As(err, &exp) {
			t.Fatalf("want *ApprovalExpiredError from Resume, got %T: %v", err, err)
		}
	}
}

// TestApprovalExpiresOverrideIsHonoured checks the per-approval window beats the handle
// default, which is the only reason the field exists.
func TestApprovalExpiresOverrideIsHonoured(t *testing.T) {
	ctx := context.Background()
	sb := openHandle(t,
		sandbox.WithPolicy(sandbox.PolicyFunc(func(_ context.Context, rc *sandbox.RunContext) (sandbox.Decision, error) {
			if strings.Contains(rc.Request.Code, "short") {
				return sandbox.Decision{
					Action:   sandbox.ActRequireApproval,
					Reason:   "short-lived ask",
					Approval: &sandbox.Approval{ID: "ap-short", Expires: 20 * time.Millisecond},
				}, nil
			}
			return sandbox.Decision{Action: sandbox.ActAllow}, nil
		})),
		sandbox.WithTimeouts(sandbox.Timeouts{ApprovalHold: time.Hour}),
	)
	sess, err := sb.Session(ctx, "override", sandbox.WithCreateIfMissing(true))
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	_, err = sess.Exec(ctx, sandbox.Shell("export short=1"))
	var pending *sandbox.ApprovalPendingError
	if !errors.As(err, &pending) {
		t.Fatalf("want pending, got %v", err)
	}
	if pending.Hold > time.Second {
		t.Fatalf("the per-approval window should govern, got %s", pending.Hold)
	}
	time.Sleep(40 * time.Millisecond)
	if err := sess.Approve(ctx, pending.ApprovalID, "late"); err == nil {
		t.Fatal("the shorter of the two windows must win")
	}
}

// TestApprovalDoesNotLoop is the trap in the obvious implementation: re-running an
// approved request re-runs the policy, which demands approval again, which a caller can
// answer forever. The grant must discharge its own obligation.
func TestApprovalDoesNotLoop(t *testing.T) {
	ctx := context.Background()
	var asks int
	sb := openHandle(t, sandbox.WithPolicy(sandbox.PolicyFunc(func(_ context.Context, rc *sandbox.RunContext) (sandbox.Decision, error) {
		if strings.Contains(rc.Request.Code, "loop") {
			asks++
			return sandbox.Decision{Action: sandbox.ActRequireApproval, Reason: "loop guard", Policy: "loop"}, nil
		}
		return sandbox.Decision{Action: sandbox.ActAllow}, nil
	})))
	sess, err := sb.Session(ctx, "loop", sandbox.WithCreateIfMissing(true))
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	_, err = sess.Exec(ctx, sandbox.Shell("export loop=1"))
	var pending *sandbox.ApprovalPendingError
	if !errors.As(err, &pending) {
		t.Fatalf("want pending, got %v", err)
	}
	if asks != 1 {
		t.Fatalf("the policy should have been asked once, got %d", asks)
	}
	if err := sess.Approve(ctx, pending.ApprovalID, "rex"); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if _, err := sess.Resume(ctx, pending.ApprovalID); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if asks != 2 {
		t.Fatalf("the resume should consult the policy once more and pass, got %d asks", asks)
	}
	// And it stayed passed: no second suspension is outstanding.
	if got := sess.Awaiting(); len(got) != 0 {
		t.Fatalf("resuming an approved request must not park a second approval, got %d", len(got))
	}
}

// TestApprovalRefusesADifferentBox is the trust invariant. An approval is permission for
// the environment that existed when a human answered; a session that lost and rebuilt its
// environment must not get to spend the old yes on the new box.
func TestApprovalRefusesADifferentBox(t *testing.T) {
	ctx := context.Background()
	sb := openHandle(t,
		sandbox.WithPolicy(approvalPolicy("grant")),
		// The environment has to be rebuildable for this test to mean anything: with no
		// retry mode the session refuses to re-provision at all, which is the contract,
		// and then there is no second box for the approval to be refused against.
		sandbox.WithRetryMode(sandbox.RebuildFromSpec),
	)
	sess, err := sb.Session(ctx, "box", sandbox.WithCreateIfMissing(true))
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	// Give the session a live environment first. Policies run before provisioning on
	// purpose (§6.3), so a request suspended on a brand-new session was never about a
	// box at all, and there would be nothing for the invariant to protect. The normal
	// HITL case is "this existing project box is about to do something".
	if _, err := sess.Exec(ctx, sandbox.Shell("export warm=1")); err != nil {
		t.Fatalf("warming Exec: %v", err)
	}

	_, err = sess.Exec(ctx, sandbox.Shell("export grant=1"))
	var pending *sandbox.ApprovalPendingError
	if !errors.As(err, &pending) {
		t.Fatalf("want pending, got %v", err)
	}
	awaited := sess.Awaiting()
	if len(awaited) != 1 || !awaited[0].BoundToEnvironment() {
		t.Fatalf("an approval taken while a box was live must be bound to it, got %+v", awaited)
	}
	if err := sess.Approve(ctx, pending.ApprovalID, "rex"); err != nil {
		t.Fatalf("Approve: %v", err)
	}

	// The environment dies and is replaced between the answer and the resume.
	sess.MarkLost(errors.New("gate: environment dropped"))
	if _, err := sess.Exec(ctx, sandbox.Shell("export unrelated=1")); err != nil {
		t.Fatalf("re-provisioning Exec: %v", err)
	}

	_, err = sess.Resume(ctx, pending.ApprovalID)
	if err == nil {
		t.Fatal("approved work must not be run in an environment the approval never saw")
	}
	var changed *sandbox.ApprovalEnvironmentChangedError
	if !errors.As(err, &changed) {
		t.Fatalf("want *ApprovalEnvironmentChangedError, got %T: %v", err, err)
	}
	if changed.Wanted == "" || changed.Got == "" || changed.Wanted == changed.Got {
		t.Fatalf("the error must name both environments, got wanted=%q got=%q", changed.Wanted, changed.Got)
	}
	if len(changed.Remediation()) == 0 {
		t.Fatal("the caller must be told how to proceed")
	}
	if sandbox.ClassOf(err) != sandbox.ClassLost {
		t.Fatalf("the environment the decision referred to is gone: %s", sandbox.ClassOf(err))
	}
	// The grant is not consumed by a refusal to use it, so a human answer is never
	// silently spent on the wrong thing.
	if !strings.Contains(err.Error(), "was not spent") {
		t.Fatalf("the error must say the approval was not consumed, got %q", err)
	}
}

// TestUnknownApprovalIdIsCorrectable pins the answer for a wrong id.
func TestUnknownApprovalIdIsCorrectable(t *testing.T) {
	ctx := context.Background()
	sb := openHandle(t)
	sess, err := sb.Session(ctx, "unknown", sandbox.WithCreateIfMissing(true))
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	err = sess.Approve(ctx, "never-existed", "rex")
	var unk *sandbox.UnknownApprovalError
	if !errors.As(err, &unk) {
		t.Fatalf("want *UnknownApprovalError, got %T: %v", err, err)
	}
	if !strings.Contains(strings.Join(unk.Remediation(), " "), "Awaiting") {
		t.Fatalf("the remedy should point at the list of real ids, got %v", unk.Remediation())
	}
}

// TestDoubleAnswerIsRefused keeps a second answer from silently rewriting a verdict.
func TestDoubleAnswerIsRefused(t *testing.T) {
	ctx := context.Background()
	sb := openHandle(t, sandbox.WithPolicy(approvalPolicy("twice")))
	sess, err := sb.Session(ctx, "double", sandbox.WithCreateIfMissing(true))
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	_, err = sess.Exec(ctx, sandbox.Shell("export twice=1"))
	var pending *sandbox.ApprovalPendingError
	if !errors.As(err, &pending) {
		t.Fatalf("want pending, got %v", err)
	}
	if err := sess.Approve(ctx, pending.ApprovalID, "first"); err != nil {
		t.Fatalf("first Approve: %v", err)
	}
	err = sess.Deny(ctx, pending.ApprovalID, "changed my mind")
	var answered *sandbox.ApprovalAnsweredError
	if !errors.As(err, &answered) {
		t.Fatalf("want *ApprovalAnsweredError, got %T: %v", err, err)
	}
	if answered.State != sandbox.ApprovalGranted {
		t.Fatalf("the record should show the answer that stands, got %s", answered.State)
	}
}

// TestApprovalEventsUseful checks the events exist for dashboards, while Awaiting remains
// the primary surface: the events alone would leave a caller without an Observer stuck.
func TestApprovalEventsAreRecorded(t *testing.T) {
	ctx := context.Background()
	var seen []sandbox.EventKind
	events := sandbox.ObserverFunc(func(_ context.Context, ev sandbox.Event) error {
		seen = append(seen, ev.Kind)
		return nil
	})
	sb := openHandle(t, sandbox.WithPolicy(approvalPolicy("events")), sandbox.WithObserver(events))
	sess, err := sb.Session(ctx, "ev", sandbox.WithCreateIfMissing(true))
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	_, err = sess.Exec(ctx, sandbox.Shell("export events=1"))
	var pending *sandbox.ApprovalPendingError
	if !errors.As(err, &pending) {
		t.Fatalf("want pending, got %v", err)
	}
	if err := sess.Approve(ctx, pending.ApprovalID, "rex"); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if _, err := sess.Resume(ctx, pending.ApprovalID); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	want := map[sandbox.EventKind]bool{
		sandbox.EventApprovalRequested: false,
		sandbox.EventApprovalGranted:   false,
	}
	for _, k := range seen {
		if _, ok := want[k]; ok {
			want[k] = true
		}
	}
	for k, got := range want {
		if !got {
			t.Fatalf("missing %s event; saw %v", k, seen)
		}
	}
}

// TestDiagnoseReportsHangingApproval makes a stall visible to the handle-level
// diagnostic, so "the engine seems slow" can be told apart from "nobody has answered".
func TestDiagnoseReportsHangingApproval(t *testing.T) {
	ctx := context.Background()
	sb := openHandle(t, sandbox.WithPolicy(approvalPolicy("diag")))
	sess, err := sb.Session(ctx, "diag", sandbox.WithCreateIfMissing(true))
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if _, err := sess.Exec(ctx, sandbox.Shell("export diag=1")); err == nil {
		t.Fatal("want pending")
	}
	diag, err := sb.Diagnose(ctx)
	if err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	var found bool
	for _, c := range diag.Checks {
		if c.Env == "approvals" {
			found = true
			if c.OK {
				t.Fatal("an outstanding approval must not be reported as healthy")
			}
			if !strings.Contains(c.Fix, "Awaiting") {
				t.Fatalf("the fix should name how to find it, got %q", c.Fix)
			}
		}
	}
	if !found {
		t.Fatal("Diagnose reported nothing about suspended approvals")
	}
}

// TestApprovalAnswerDoesNotDeadlockClose guards the lock order. Emitting while holding the
// session lock deadlocks against Close, which takes the handle lock and then releases each
// session; a panic inside a locked section strands the handle lock the same way. Both show
// up as a test that never returns rather than a failure, so the shape has to be exercised.
func TestApprovalAnswerDoesNotDeadlockClose(t *testing.T) {
	ctx := context.Background()
	sb := openHandle(t, sandbox.WithPolicy(approvalPolicy("race")))
	ids := make([]string, 0, 4)
	for i := 0; i < 4; i++ {
		sess, err := sb.Session(ctx, "dl", sandbox.WithCreateIfMissing(true))
		if err != nil {
			t.Fatalf("Session: %v", err)
		}
		if _, err := sess.Exec(ctx, sandbox.Shell("export warm=1")); err != nil {
			t.Fatalf("warm Exec: %v", err)
		}
		_, err = sess.Exec(ctx, sandbox.Shell("export race=1"))
		var pending *sandbox.ApprovalPendingError
		if !errors.As(err, &pending) {
			t.Fatalf("want pending, got %v", err)
		}
		ids = append(ids, pending.ApprovalID)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		sess, err := sb.Session(ctx, "dl", sandbox.WithCreateIfMissing(true))
		if err != nil {
			t.Errorf("Session in goroutine: %v", err)
			return
		}
		for _, id := range ids {
			_ = sess.Approve(ctx, id, "concurrent")
			_, _ = sess.Resume(ctx, id)
			_ = sess.Awaiting()
			_, _ = sb.Diagnose(ctx)
		}
	}()

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("answering approvals concurrently with Diagnose hung: lock order regression")
	}
	// Closing must also return: this is where the stranded handle lock surfaced.
	closed := make(chan error, 1)
	go func() { closed <- sb.Close() }()
	select {
	case err := <-closed:
		_ = err
	case <-time.After(20 * time.Second):
		t.Fatal("Close hung after approval traffic: handle lock stranded")
	}
}
