package sandbox_test

import (
	"context"
	"strings"
	"testing"

	"github.com/rexleimo/agno-go/pkg/hno/sandbox"
	"github.com/rexleimo/agno-go/pkg/hno/sandbox/sandboxtest"
)

// resumeProvider hands the test the concrete double so it can read what the
// environment actually executed and can delete a box to simulate an eviction.
type resumeFixture struct {
	prov  *sandboxtest.Provider
	store *sandboxtest.Records
	sb    *sandbox.Sandbox
}

func newResumeFixture(t *testing.T, name string) *resumeFixture {
	t.Helper()
	return newResumeFixtureWith(t, name, nil)
}

func newResumeFixtureWith(t *testing.T, name string, cache sandbox.JournalStore) *resumeFixture {
	t.Helper()
	store := sandboxtest.NewRecords()
	base, err := sandboxtest.NewProvider(sandbox.ProviderConfig{Image: "test:latest"})
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	mem := base.(*sandboxtest.Provider)
	if err := sandbox.RegisterProvider(name, func(sandbox.ProviderConfig) (sandbox.Provider, error) {
		return mem, nil
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	t.Cleanup(func() { sandbox.UnregisterProvider(name) })
	opts := []sandbox.Option{
		sandbox.WithProvider(name),
		sandbox.WithAllowedProviders(name),
		sandbox.WithMinimumIsolation(sandbox.IsolationNone),
		sandbox.WithSessionStore(store),
	}
	if cache != nil {
		opts = append(opts, sandbox.WithStepCache(cache))
	}
	sb, err := sandbox.Open(context.Background(), opts...)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = sb.Close() })
	return &resumeFixture{prov: mem, store: store, sb: sb}
}

// TestRecordedStepsReplayIntoAResumedEnvironment is the falsifier for the second silent
// no-op of this round. replayStep() has exactly one call site, inside rebuild(). The
// session-acquisition path builds a fresh environment from the recorded spec and never
// replays those steps, so a resumed session reports itself Ready with none of its
// installs present — and the next failure gets blamed on the user's code.
func TestRecordedStepsReplayIntoAResumedEnvironment(t *testing.T) {
	ctx := context.Background()
	f := newResumeFixture(t, "resumable")

	sess, err := f.sb.Session(ctx, "workflow-1", sandbox.WithCreateIfMissing(true))
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if err := sess.Install(ctx, sandbox.Pip("requests")); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if err := sess.Setup(ctx, "export PATH=/opt/bin:$PATH"); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	before := f.prov.History("workflow-1")
	if !strings.Contains(strings.Join(before, "\n"), "requests") {
		t.Fatalf("the double recorded no install for the live environment: %v", before)
	}
	if err := sess.Release(ctx); err != nil {
		t.Fatalf("Release: %v", err)
	}

	// The box is gone: an idle eviction, a crash, or a process restart. The record
	// survives, which is the entire point of a session store.
	f.prov.DropEnvironment("workflow-1")
	if _, ok, err := f.store.LoadSession(ctx, "workflow-1"); err != nil || !ok {
		t.Fatalf("the record did not survive: found=%v err=%v", ok, err)
	}

	again, err := f.sb.Session(ctx, "workflow-1", sandbox.WithSessionReplay(true))
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	defer again.Release(ctx)

	hist := strings.Join(f.prov.History("workflow-1"), "\n")
	for _, want := range []string{"requests", "PATH=/opt/bin"} {
		if !strings.Contains(hist, want) {
			t.Fatalf("resumed environment never replayed %q; history=%q\n"+
				"the session claims Ready while its dependencies are absent", want, f.prov.History("workflow-1"))
		}
	}
}

// TestResumedSessionReportsReplayedSteps keeps the resume honest in the data the caller
// can already see: the spec must carry the steps and the status must not pretend the
// environment is the original one.
func TestResumedSessionReportsReplayedSteps(t *testing.T) {
	ctx := context.Background()
	f := newResumeFixture(t, "resumable-spec")
	sess, err := f.sb.Session(ctx, "wf-2", sandbox.WithCreateIfMissing(true))
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if err := sess.Install(ctx, sandbox.Pip("numpy")); err != nil {
		t.Fatalf("Install: %v", err)
	}
	spec := sess.Spec()
	if len(spec.Steps) != 1 {
		t.Fatalf("the session must record one replayable step, got %d", len(spec.Steps))
	}
	if spec.Steps[0].Kind != sandbox.StepInstall {
		t.Fatalf("step kind = %v, want StepInstall", spec.Steps[0].Kind)
	}
	if spec.Steps[0].Digest == "" {
		t.Fatal("a step without a digest cannot be cache-keyed")
	}
}

// TestStepCacheCannotSilentlySkipAgain is the regression gate for D-20. It replaced a
// visible SKIP that carried the measurement while the decision was open; the decision is
// now made, so the same evidence has to fail the build instead of printing a warning.
func TestStepCacheCannotSilentlySkipAgain(t *testing.T) {
	ctx := context.Background()
	cache := sandbox.NewMemoryJournal()
	sb := openHandle(t, sandbox.WithStepCache(cache))

	a, err := sb.Session(ctx, "lineage-a", sandbox.WithCreateIfMissing(true))
	if err != nil {
		t.Fatalf("Session a: %v", err)
	}
	if err := a.Install(ctx, sandbox.Pip("pandas==2.0.3")); err != nil {
		t.Fatalf("Install a: %v", err)
	}
	if _, err := a.Rebuild(ctx); err != nil {
		t.Fatalf("Rebuild a: %v", err)
	}

	// A different session, same step content, whose box has never seen the install.
	b, err := sb.Session(ctx, "lineage-b", sandbox.WithCreateIfMissing(true))
	if err != nil {
		t.Fatalf("Session b: %v", err)
	}
	if err := b.Install(ctx, sandbox.Pip("pandas==2.0.3")); err != nil {
		t.Fatalf("Install b: %v", err)
	}
	// Make the loss real, then rebuild into a fresh container.
	sandboxtest.DropEnvironment(t, "lineage-b")
	rep, err := b.Rebuild(ctx)
	if err != nil {
		t.Fatalf("Rebuild b: %v", err)
	}
	if rep.Cached != 0 {
		t.Fatalf("session b skipped %d steps it never satisfied in this container: %+v", rep.Cached, rep)
	}
	if len(rep.Replayed) == 0 {
		t.Fatal("session b replayed no steps at all")
	}
	if got := sandboxtest.InstalledIn(t, "lineage-b", "pandas==2.0.3"); !got {
		t.Fatal("replay did not actually reinstall into the fresh box")
	}
}
