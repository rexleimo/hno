package sandbox

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"
)

// ErrJournalMissing reports that delivery semantics were requested without a
// journal store. It fails closed: silence here would let an external effect
// happen twice under the illusion of idempotency (§16.3).
var ErrJournalMissing = errors.New("sandbox: delivery promise requires a journal store")

// RunState is the journalled lifecycle of one idempotent run.
type RunState uint8

const (
	// RunStateUnknown is the zero value.
	RunStateUnknown RunState = iota
	// RunStateSubmitted means the engine accepted the run.
	RunStateSubmitted
	// RunStateDone means a terminal result is recorded.
	RunStateDone
	// RunStateUncertain means the outcome cannot be confirmed (disconnect with
	// no status query capability).
	RunStateUncertain
	// RunStateFailed means the run terminated with a classified failure.
	RunStateFailed
)

// String names the state.
func (s RunState) String() string {
	switch s {
	case RunStateSubmitted:
		return "submitted"
	case RunStateDone:
		return "done"
	case RunStateUncertain:
		return "uncertain"
	case RunStateFailed:
		return "failed"
	default:
		return "unknown"
	}
}

// JournalEntry is one idempotency record. It must survive process restarts,
// which is why it lives in a store rather than a map.
type JournalEntry struct {
	Key         string
	RunID       string
	SessionID   string
	State       RunState
	PayloadHash string
	// ResultRef points at a persisted report or artefact bundle so a replay can
	// return the original outcome instead of re-running.
	ResultRef string
	UpdatedAt time.Time
}

// JournalStore persists idempotency records. The interface is intentionally
// three methods: pkg/hno/store and graph.Checkpointer both satisfy it with a
// thin adapter owned by the caller, so the core imports neither (§16.3).
type JournalStore interface {
	Load(ctx context.Context, key string) (JournalEntry, bool, error)
	Save(ctx context.Context, entry JournalEntry) error
}

// RetryBudget is the shared ceiling across every retry layer. Each layer may
// spend from it but no layer may invent new attempts, which is how "Core 2 ×
// node 3 = 6 boots" becomes impossible (§16.2).
type RetryBudget struct {
	// MaxAttempts bounds attempts for one logical run. 0 and 1 both mean a
	// single attempt: the clamp is deliberate, never an unlimited default.
	MaxAttempts int
	// PerAttempt bounds one attempt; it must cover boot plus execution
	// (§16.2 fake-timeout trap).
	PerAttempt time.Duration
	// WallClock bounds every attempt plus backoff together.
	WallClock time.Duration
	// Class budgets keep infra noise from eating the queue-retry allowance
	// (§16.1).
	ClassBudget map[FailureClass]int
}

// attempts resolves the class budget with the documented defaults: transient
// gets the general budget, lost gets one rebuild, everything else one.
func (b RetryBudget) attempts(class FailureClass) int {
	if n, ok := b.ClassBudget[class]; ok && n > 0 {
		return n
	}
	switch class {
	case ClassTransient:
		if b.MaxAttempts > 0 {
			return b.MaxAttempts
		}
		return 2
	case ClassResource:
		if b.MaxAttempts > 0 {
			return b.MaxAttempts
		}
		return 3
	case ClassLost:
		return 1 // one rebuild attempt; more means the environment is not recoverable
	default:
		return 1
	}
}

// allow decides whether another attempt fits inside every constraint at once.
func (b RetryBudget) allow(attempt int, class FailureClass, elapsed time.Duration) bool {
	if attempt >= b.attempts(class) {
		return false
	}
	if b.WallClock > 0 && elapsed >= b.WallClock {
		return false
	}
	return true
}

// memoryJournal is the default in-process store, used when no persistent one is
// configured. It only supports AtLeastOnce reasoning; delivery promises that
// need durability require an explicit store (fail closed in Open).
type memoryJournal struct {
	mu    sync.Mutex
	items map[string]JournalEntry
}

func newMemoryJournal() *memoryJournal {
	return &memoryJournal{items: make(map[string]JournalEntry)}
}

func (m *memoryJournal) Load(_ context.Context, key string) (JournalEntry, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.items[key]
	return e, ok, nil
}

func (m *memoryJournal) Save(_ context.Context, entry JournalEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if prev, ok := m.items[entry.Key]; ok && prev.PayloadHash != entry.PayloadHash {
		return &IdempotencyConflictError{Key: entry.Key, OriginalRun: prev.RunID, PayloadHash: entry.PayloadHash}
	}
	entry.UpdatedAt = time.Now().UTC()
	m.items[entry.Key] = entry
	return nil
}

// StormGuard is the sliding-window protection against retry storms (§16.5).
// Agent fan-out makes a storm the normal case, not the corner case: one broken
// provider plus three retry layers multiplies instead of failing.
type StormGuard struct {
	// Window is the look-back period.
	Window time.Duration
	// MaxPerKey bounds concurrent or repeated attempts sharing an idempotency
	// key. 0 means one in flight.
	MaxPerKey int
	// MaxPerSession bounds attempts attributed to one session.
	MaxPerSession int
	// MaxTotal bounds the whole handle.
	MaxTotal int

	mu        sync.Mutex
	byKey     map[string][]time.Time
	bySession map[string][]time.Time
	all       []time.Time
}

// allow records an attempt if it fits every window, and reports which limit it
// would have breached otherwise.
func (g *StormGuard) allow(key, session string) error {
	if g == nil || g.Window == 0 {
		return nil // guard disabled
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	g.trim(now)
	if g.MaxPerKey > 0 && len(g.byKey[key]) >= g.MaxPerKey {
		return &RetryBudgetError{Scope: "idempotency key " + key, Attempts: len(g.byKey[key]), Window: g.Window}
	}
	if g.MaxPerSession > 0 && len(g.bySession[session]) >= g.MaxPerSession {
		return &RetryBudgetError{Scope: "session " + session, Attempts: len(g.bySession[session]), Window: g.Window}
	}
	if g.MaxTotal > 0 && len(g.all) >= g.MaxTotal {
		return &RetryBudgetError{Scope: "handle total", Attempts: len(g.all), Window: g.Window}
	}
	g.all = append(g.all, now)
	if key != "" {
		g.byKey[key] = append(g.byKey[key], now)
	}
	if session != "" {
		g.bySession[session] = append(g.bySession[session], now)
	}
	return nil
}

// trim drops timestamps older than the window.
func (g *StormGuard) trim(now time.Time) {
	cutoff := now.Add(-g.Window)
	g.all = keepAfter(g.all, cutoff)
	for k, ts := range g.byKey {
		if kept := keepAfter(ts, cutoff); len(kept) > 0 {
			g.byKey[k] = kept
		} else {
			delete(g.byKey, k)
		}
	}
	for k, ts := range g.bySession {
		if kept := keepAfter(ts, cutoff); len(kept) > 0 {
			g.bySession[k] = kept
		} else {
			delete(g.bySession, k)
		}
	}
}

// keepAfter filters timestamps newer than cutoff.
func keepAfter(ts []time.Time, cutoff time.Time) []time.Time {
	out := ts[:0]
	for _, t := range ts {
		if t.After(cutoff) {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Before(out[j]) })
	return out
}

// newStormGuard builds a guard with sane defaults applied.
func newStormGuard(cfg StormGuardConfig) *StormGuard {
	if !cfg.Enabled {
		return nil
	}
	window := cfg.Window
	if window <= 0 {
		window = 30 * time.Second
	}
	perKey := cfg.MaxPerKey
	if perKey <= 0 {
		perKey = 1
	}
	return &StormGuard{
		Window:        window,
		MaxPerKey:     perKey,
		MaxPerSession: cfg.MaxPerSession,
		MaxTotal:      cfg.MaxTotal,
		byKey:         map[string][]time.Time{},
		bySession:     map[string][]time.Time{},
	}
}

// StormGuardConfig is the profile-facing knobs for the guard.
type StormGuardConfig struct {
	Enabled       bool
	Window        time.Duration
	MaxPerKey     int
	MaxPerSession int
	MaxTotal      int
}
