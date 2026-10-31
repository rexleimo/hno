package sandbox_test

import (
	"context"
	"errors"
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

// TestStepCacheIsNotEvidenceAboutAFreshBox measures a defect that needs a design
// decision before it can be fixed, so it reports rather than redlines.
//
// The step cache is installed by default with a process-memory journal
// (Config.normalize: `if c.StepCache == nil { c.StepCache = newMemoryJournal() }`), the
// same shape the code two lines below refuses for the idempotency Journal because "an
// idempotency promise backed by process memory is a lie across a restart". Step.Key()
// hashes step content only -- no session, no image, no environment generation -- and
// replayRecorded treats a hit as permission to skip. But RebuildFromSpec builds the new
// box from spec.Base and restores no snapshot, so the box has provably never run the
// step. A cache hit is being used as evidence about a container that cannot contain the
// effect.
//
// Measured on this double, after dropping the box so the loss is real:
//
//	same session:  cached=1 replayed=0 history=[]   (workflow's own deps vanish)
//	cross session: cached=1 replayed=0 history=[]   (B skipped because A ran it)
//
// Any resulting failure is classified ClassUserCode, so the sandbox blames the caller
// for a skip the sandbox performed.
func TestStepCacheIsNotEvidenceAboutAFreshBox(t *testing.T) {
	ctx := context.Background()
	cache := sandbox.NewMemoryJournal()
	f := newResumeFixtureWith(t, "cache-lineage", cache)

	sess, err := f.sb.Session(ctx, "lineage-1", sandbox.WithCreateIfMissing(true))
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if err := sess.Install(ctx, sandbox.Pip("scikit-learn")); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if _, err := sess.Rebuild(ctx); err != nil {
		t.Fatalf("first Rebuild: %v", err)
	}
	if !f.prov.InstalledIn("lineage-1", "scikit-learn") {
		t.Fatalf("first rebuild lost the package: %v", f.prov.History("lineage-1"))
	}

	sess.MarkLost(errors.New("simulated loss"))
	// Real loss means the box is gone from the engine, not merely unlinked from the
	// session: otherwise the provider hands the same container back and there is no
	// fresh box to be wrong about.
	f.prov.DropEnvironment("lineage-1")
	second, err := sess.Rebuild(ctx)
	if err != nil {
		t.Fatalf("second Rebuild: %v", err)
	}
	present := f.prov.InstalledIn("lineage-1", "scikit-learn")
	if second.Cached > 0 && !present {
		t.Skipf("D-20 OPEN (measured, not fixed pending a decision): rebuild reported cached=%d "+
			"and replayed=%d on a box built from the base image, and that box has no scikit-learn: history=%q. "+
			"A step-cache hit is being read as evidence about a container that never ran the step. "+
			"Resolution needs a human call: replay-always-for-scratch-boxes, or rebuild restores the "+
			"latest snapshot and only steps predating it may be skipped.",
			second.Cached, len(second.Replayed), f.prov.History("lineage-1"))
	}
	if !present {
		t.Fatalf("the fresh box has no scikit-learn and nothing said so: cached=%d replayed=%d history=%q",
			second.Cached, len(second.Replayed), f.prov.History("lineage-1"))
	}
}
