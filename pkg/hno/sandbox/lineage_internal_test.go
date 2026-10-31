package sandbox

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
)

// lineageProbe is a minimal provider so the rule can be tested from inside the package
// without importing the test double, which would be an import cycle.
type lineageProbe struct{ n int }

func (p *lineageProbe) Name() string         { return "lineage-probe" }
func (p *lineageProbe) Isolation() Isolation { return IsolationContainer }
func (p *lineageProbe) Capabilities() CapabilitySet {
	return Capabilities(CapSession, CapRebuild, CapQueryStatus)
}
func (p *lineageProbe) Probe(context.Context) error { return nil }
func (p *lineageProbe) Close(context.Context) error { return nil }
func (p *lineageProbe) LoadSession(context.Context, string) (SessionRecord, bool, error) {
	return SessionRecord{}, false, nil
}
func (p *lineageProbe) SaveSession(context.Context, SessionRecord) error { return nil }
func (p *lineageProbe) DeleteSession(context.Context, string) error      { return nil }
func (p *lineageProbe) NewEnvironment(context.Context, EnvSpec) (Environment, error) {
	p.n++
	return &lineageEnv{id: fmt.Sprintf("env-%d", p.n)}, nil
}

type lineageEnv struct{ id string }

func (e *lineageEnv) ID() string           { return e.id }
func (e *lineageEnv) Image() string        { return "t:1" }
func (e *lineageEnv) Isolation() Isolation { return IsolationContainer }
func (e *lineageEnv) Alive() bool          { return true }
func (e *lineageEnv) Close(context.Context) error {
	return nil
}
func (e *lineageEnv) Exec(context.Context, RunSpec, ChunkReporter) (*Report, error) {
	return &Report{ExitCode: 0}, nil
}

func newLineageHandle(t *testing.T, cache JournalStore) *Sandbox {
	t.Helper()
	name := "lineage-probe"
	_ = RegisterProvider(name, func(ProviderConfig) (Provider, error) { return &lineageProbe{}, nil })
	sb, err := Open(context.Background(), WithProvider(name), WithAllowedProviders(name), WithImage("t:1"), WithStepCache(cache))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return sb
}

// TestStepCacheHonoursTheLineageRule pins the rule itself, not just its default
// outcome. Without it, cached=0 on every path would look like success while the
// mechanism silently never engaged -- the same silent-no-op shape this package has
// been auditing everywhere else.
func TestStepCacheHonoursTheLineageRule(t *testing.T) {
	cache := NewMemoryJournal()
	sb := newLineageHandle(t, cache)
	defer sb.Close()

	step := Step{Kind: StepInstall, Tool: "pip", Args: []string{"numpy==1.26"}, Sequence: 0}
	step.Digest = step.Key()

	// A box that inherited nothing can authorise no skip, whatever the cache holds.
	if hit, ok := sb.stepCached("gen:fresh", -1, step); ok && hit {
		t.Fatal("a scratch lineage authorised a skip")
	}
	// Recording happens under the lineage the step was satisfied in.
	sb.rememberStep("gen:abcd", step)

	// The same lineage, with a watermark covering the step, may skip.
	if hit, ok := sb.stepCached("gen:abcd", 0, step); !ok || !hit {
		t.Fatalf("an inheriting lineage could not skip the step it already satisfied (ok=%v hit=%v)", ok, hit)
	}
	// A different lineage may not, even with identical step content: that is the
	// cross-session leak D-20 measured.
	if hit, ok := sb.stepCached("gen:other", 5, step); ok && hit {
		t.Fatal("a foreign lineage skipped a step it never satisfied")
	}
	// Same lineage, but the step came after the inherited watermark: not present.
	if hit, ok := sb.stepCached("gen:abcd", -1, step); ok && hit {
		t.Fatal("a step beyond the watermark was skipped")
	}
	// No store installed means no skip either.
	bare := newLineageHandle(t, nil)
	defer bare.Close()
	if hit, ok := bare.stepCached("gen:abcd", 9, step); ok && hit {
		t.Fatal("a handle without a step cache reported a hit")
	}
}

// countingJournal records what the pipeline actually stored, so "the rule works when I
// call it by hand" cannot be mistaken for "the rule runs in production".
type countingJournal struct {
	mu     sync.Mutex
	saved  []string
	loaded map[string]bool
}

func (j *countingJournal) Load(_ context.Context, key string) (JournalEntry, bool, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	hit := j.loaded[key]
	return JournalEntry{Key: key}, hit, nil
}

func (j *countingJournal) Save(_ context.Context, entry JournalEntry) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.loaded == nil {
		j.loaded = map[string]bool{}
	}
	j.loaded[entry.Key] = true
	j.saved = append(j.saved, entry.Key)
	return nil
}

// TestReplayPipelineRecordsSatisfiedSteps is the falsifier for the shape this whole
// audit keeps finding: a mechanism that is correct in isolation and unreachable in the
// pipeline. An earlier version recovered the lineage by scanning live sessions for the
// environment, and rebuild nils that field before replaying, so the scan returned
// nothing, no step was ever recorded, and every test still passed because the outcome
// (replay everything) was the safe one.
func TestReplayPipelineRecordsSatisfiedSteps(t *testing.T) {
	store := &countingJournal{loaded: map[string]bool{}}
	sb := newLineageHandle(t, store)
	defer sb.Close()

	sess, err := sb.Session(context.Background(), "prov", WithCreateIfMissing(true))
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if err := sess.Install(context.Background(), Pip("numpy==1.26")); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if _, err := sb.rebuild(context.Background(), sess); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	store.mu.Lock()
	saved := append([]string(nil), store.saved...)
	store.mu.Unlock()
	if len(saved) == 0 {
		t.Fatal("the replay pipeline stored nothing, so the step cache can never engage on an inheriting rebuild")
	}
	for _, k := range saved {
		if !strings.Contains(k, "|") {
			t.Fatalf("cache key %q is not scoped to a state lineage; an unscoped key is what let one container's step silence another's", k)
		}
	}
}
