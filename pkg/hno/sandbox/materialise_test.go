package sandbox_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rexleimo/agno-go/pkg/hno/sandbox"
	"github.com/rexleimo/agno-go/pkg/hno/sandbox/sandboxtest"
)

// TestRecordedInputSurvivesRebuild is the real-engine finding applied offline. Recording
// an input used to store a path and a digest with no content behind them, so the replay
// could only assert the file existed -- in a box rendered from the base image it never
// did, and every rebuild of a session that had recorded an input failed.
func TestRecordedInputSurvivesRebuild(t *testing.T) {
	ctx := context.Background()
	sb := openHandle(t)
	sess, err := sb.Session(ctx, "mat", sandbox.WithCreateIfMissing(true))
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if err := sess.RecordInput(ctx, sandbox.Input{Path: "data/in.txt", Bytes: []byte("payload-42")}); err != nil {
		t.Fatalf("RecordInput: %v", err)
	}
	if _, err := sess.Exec(ctx, sandbox.Shell("echo used")); err != nil {
		t.Fatalf("Exec: %v", err)
	}

	sandboxtest.DropEnvironment(t, "mat")
	rep, err := sess.Rebuild(ctx)
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if rep.Cached != 0 {
		t.Fatalf("a fresh container inherited nothing, yet skipped %d steps", rep.Cached)
	}
	var replayed bool
	for _, r := range rep.Replayed {
		if r.Step.Kind == sandbox.StepMaterialize {
			replayed = true
			if r.Cached || r.Err != nil {
				t.Fatalf("materialise step replayed badly: %+v", r)
			}
		}
	}
	if !replayed {
		t.Fatalf("no materialise step was replayed: %+v", rep.Replayed)
	}
	// The point of replaying is the file being there again.
	have, err := sess.Files()
	if err != nil {
		t.Fatalf("Files: %v", err)
	}
	got, err := have.Read(ctx, "data/in.txt")
	if err != nil {
		t.Fatalf("the recorded input did not survive the rebuild: %v", err)
	}
	if string(got) != "payload-42" {
		t.Fatalf("content = %q, want the bytes recorded", got)
	}
}

// TestContentOnlyRunIsNotACommand checks the pipeline stops pretending a materialisation
// has an exit code. The provider had to invent a command, and the honest answer is that
// nothing ran.
func TestContentOnlyRunIsNotACommand(t *testing.T) {
	ctx := context.Background()
	sb := openHandle(t)
	sess, err := sb.Session(ctx, "files-only", sandbox.WithCreateIfMissing(true))
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	rep, err := sess.Exec(ctx, sandbox.Request{
		Inputs: []sandbox.Input{{Path: "a.txt", Bytes: []byte("one")}},
	})
	if err != nil {
		t.Fatalf("a content-only request must be a legal run, got %v", err)
	}
	if len(rep.Staged) != 1 || rep.Staged[0] != "a.txt" {
		t.Fatalf("the report must say what was placed, got %v", rep.Staged)
	}
	files, err := sess.Files()
	if err != nil {
		t.Fatalf("Files: %v", err)
	}
	got, err := files.Read(ctx, "a.txt")
	if err != nil {
		t.Fatalf("read staged: %v", err)
	}
	if string(got) != "one" {
		t.Fatalf("staged content = %q", got)
	}
}

// TestEmptyRequestStillFails keeps the new shape from swallowing a caller mistake: no
// content and no command is an error, not a successful no-op.
func TestEmptyRequestStillFails(t *testing.T) {
	ctx := context.Background()
	sb := openHandle(t)
	sess, err := sb.Session(ctx, "empty", sandbox.WithCreateIfMissing(true))
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if _, err := sess.Exec(ctx, sandbox.Request{}); err == nil {
		t.Fatal("an empty request must not report success")
	}
}

// TestReaderInputIsRefusedAtRecordTime is the D-05 rule applied here: a Reader is
// consumed on the way in, so a step recording one promises a rebuild can never perform.
// Refusing at record time beats discovering it during the retry that was supposed to
// rescue the run.
func TestReaderInputIsRefusedAtRecordTime(t *testing.T) {
	ctx := context.Background()
	sb := openHandle(t)
	sess, err := sb.Session(ctx, "stream", sandbox.WithCreateIfMissing(true))
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	err = sess.RecordInput(ctx, sandbox.Input{Path: "s.txt", Reader: strings.NewReader("gone next time")})
	if err == nil {
		t.Fatal("a Reader input must be refused, not recorded as replayable")
	}
	var uns *sandbox.UnrecoverableInputError
	if !errors.As(err, &uns) {
		t.Fatalf("want *UnrecoverableInputError, got %T: %v", err, err)
	}
	if uns.Fix == "" {
		t.Fatal("the refusal must name the replayable alternative")
	}
	if sandbox.ClassOf(err) != sandbox.ClassPermanent {
		t.Fatalf("retrying cannot change the recorded step, so the class must be permanent, got %s", sandbox.ClassOf(err))
	}
	if len(sess.Spec().Steps) != 0 {
		t.Fatalf("a refused record must leave no step behind, got %d", len(sess.Spec().Steps))
	}
}

// TestFailedRecordLeavesNoStep covers the other half: if the file cannot be placed now,
// the session must not keep a step claiming it was.
func TestFailedRecordLeavesNoStep(t *testing.T) {
	ctx := context.Background()
	sb := openHandle(t)
	sess, err := sb.Session(ctx, "bad-record", sandbox.WithCreateIfMissing(true))
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	before := len(sess.Spec().Steps)
	// A host path that does not exist cannot be read for replay.
	if err := sess.RecordInput(ctx, sandbox.Input{Path: "x.txt", HostPath: "/nonexistent/hno-path"}); err == nil {
		t.Fatal("a host input that cannot be read must be refused")
	}
	if len(sess.Spec().Steps) != before {
		t.Fatalf("a refused record changed the spec: %d -> %d", before, len(sess.Spec().Steps))
	}
}
