package docker

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/rexleimo/agno-go/pkg/hno/sandbox"
)

// sweeper adapts the provider to sandbox.LeaseSweeper: find and reclaim
// containers the engine still holds that no live session in this process owns.
//
// The query goes to the engine rather than to process-local state on purpose. A
// provider that only knows what it created cannot find the residue a crash left
// behind, which is precisely the residue that needs finding (§17.1: cleanup truth
// has to be observable from outside).
type sweeper struct{ p *Provider }

var _ sandbox.LeaseSweeper = sweeper{}

// Sweep returns managed containers this process is no longer holding. It deletes
// nothing: reporting and reclaiming are separate so a caller can inspect a leaked
// container before it disappears.
func (s sweeper) Sweep(ctx context.Context) ([]string, error) {
	items, err := s.p.dial.listed(ctx, s.p.runner, s.p.bin)
	if err != nil {
		return nil, s.p.unavailable("container listing failed", err,
			"run the container CLI manually once to see the engine error")
	}
	// Snapshot the live set under the lock, then decide outside it: holding the
	// provider lock across an engine round trip would stall every other
	// session's cleanup behind a single slow daemon.
	live := s.p.liveIDs()
	var orphans []string
	for _, it := range items {
		if _, owned := live[it.ID]; !owned {
			orphans = append(orphans, it.ID)
		}
	}
	return orphans, nil
}

// Prune deletes the orphans Sweep reported and returns what it removed.
func (s sweeper) Prune(ctx context.Context) ([]string, error) {
	orphans, err := s.Sweep(ctx)
	if err != nil {
		return nil, err
	}
	var removed []string
	for _, id := range orphans {
		if err := s.p.remove(ctx, id); err != nil {
			return removed, err
		}
		removed = append(removed, id)
	}
	return removed, nil
}

// Sessions maps session id to the containers the engine says belong to it. This
// answers "which sandboxes am I holding right now" from a different process than
// the one that opened them, which is what makes a restart's leftovers visible
// instead of invisible.
func (s sweeper) Sessions(ctx context.Context) (map[string][]string, error) {
	items, err := s.p.dial.listed(ctx, s.p.runner, s.p.bin)
	if err != nil {
		return nil, s.p.unavailable("container listing failed", err,
			"run the container CLI manually once to see the engine error")
	}
	out := map[string][]string{}
	for _, it := range items {
		if it.Session == "" {
			continue
		}
		out[it.Session] = append(out[it.Session], it.ID)
	}
	return out, nil
}

// syncMu is the provider's mutual exclusion. It is a thin wrapper rather than a
// bare sync.Mutex so the sweep's "snapshot then decide outside the lock" pattern
// reads as intent at the call site instead of looking like an ad-hoc lock.
type syncMu struct{ mu sync.Mutex }

func (m *syncMu) lock()   { m.mu.Lock() }
func (m *syncMu) unlock() { m.mu.Unlock() }

// engineError marks a failure raised by the container engine itself, as opposed
// to a payload exiting nonzero. Both are "exit code != 0", and telling them apart
// is the whole difference between "the sandbox broke" and "the model's code
// failed" — collapsing them is exactly the bug in the current run path.
type engineError struct {
	msg string
}

func (e *engineError) Error() string { return e.msg }

// newEngineError wraps a daemon message, keeping a blank message meaningful so a
// caller that prints it never sees an empty string where the reason should be.
func newEngineError(msg string) error {
	trimmed := strings.TrimSpace(msg)
	if trimmed == "" {
		trimmed = "the container engine reported no reason"
	}
	return &engineError{msg: fmt.Sprintf("container engine: %s", trimmed)}
}
