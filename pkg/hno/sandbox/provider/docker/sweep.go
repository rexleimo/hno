package docker

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"

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

// Sweep returns managed containers this process is not holding. It deletes
// nothing: reporting and reclaiming are separate so a caller can inspect a leaked
// container before it disappears.
//
// The list includes containers another process is running, because they are
// genuinely foreign to this one and hiding them would make the inventory look clean.
// Prune is where the safety decision lives.
func (s sweeper) Sweep(ctx context.Context) ([]string, error) {
	detail, err := s.Inspect(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, o := range detail.Orphans {
		out = append(out, o.ID)
	}
	for _, r := range detail.RunningForeign {
		out = append(out, r.ID)
	}
	return out, nil
}

// Inspect separates what may be reclaimed from what must not, and says why.
//
// The rule changed after a two-process experiment on a real engine: one process held
// a live session with data written into its container, another process called Prune,
// and the container was deleted underneath it — because "not in my in-memory map" had
// been taken as evidence of abandonment. It is not. Concurrent processes on one
// engine are the normal case for an agent workload or CI, so the only defensible
// default is that a running container belongs to somebody until the engine says
// otherwise.
//
// Orphans are managed containers this process does not hold and the engine reports as
// not running: crash residue, safe to reclaim. RunningForeign are managed containers
// running under another process: reported so an operator can see them, never deleted
// unless the provider explicitly opted in.
func (s sweeper) Inspect(ctx context.Context) (*SweepDetail, error) {
	items, err := s.p.dial.listed(ctx, s.p.runner, s.p.bin)
	if err != nil {
		return nil, s.p.unavailable("container listing failed", err,
			"run the container CLI manually once to see the engine error")
	}
	// Snapshot the live set under the lock, then decide outside it: holding the
	// provider lock across an engine round trip would stall every other
	// session's cleanup behind a single slow daemon.
	live := s.p.liveIDs()
	detail := &SweepDetail{}
	for _, it := range items {
		if _, owned := live[it.ID]; owned {
			detail.Owned = append(detail.Owned, it)
			continue
		}
		// "Not in my map" is not evidence of abandonment. Another live process on this
		// host may be between create and start, or may have a session idle on disk: its
		// stopped container is its working data, not residue. The owner label answers
		// the question the in-memory map cannot (measured: a concurrent gate run's
		// just-created container was pruned out from under it).
		if it.Owner != "" && ownerAlive(it.Owner) {
			if it.Running {
				detail.RunningForeign = append(detail.RunningForeign, it)
			} else {
				detail.HeldElsewhere = append(detail.HeldElsewhere, it)
			}
			continue
		}
		if it.Running {
			// Running with no reachable owner: still someone's live workload, so refuse.
			detail.RunningForeign = append(detail.RunningForeign, it)
			continue
		}
		detail.Orphans = append(detail.Orphans, it)
	}
	return detail, nil
}

// SweepDetail is the engine-side inventory classified by reclaim safety.
type SweepDetail struct {
	// Owned is held by this process and is never a sweep candidate.
	Owned []listedContainer
	// Orphans is managed, not held here, and not running: safe to reclaim.
	Orphans []listedContainer
	// RunningForeign is managed and running under another process. Deleting it is
	// data loss, so Prune refuses unless the provider opts into PruneRunning.
	RunningForeign []listedContainer
	// HeldElsewhere is a stopped container whose owner process is alive on this host.
	// Its owner is between operations, not gone, so Prune refuses it too: a concurrent
	// run's container was deleted between create and start by another process's prune.
	HeldElsewhere []listedContainer
}

// Refused names what a safe Prune declined to delete, for an operator report.
func (d *SweepDetail) Refused() []string {
	var out []string
	for _, c := range d.HeldElsewhere {
		out = append(out, fmt.Sprintf("%s (stopped but owner %s is alive)", c.ID, c.Owner))
	}
	for _, c := range d.RunningForeign {
		who := c.Owner
		if who == "" {
			who = "unknown owner"
		}
		out = append(out, fmt.Sprintf("%s (running, owner %s)", c.ID, who))
	}
	return out
}

// Prune deletes the abandoned containers Sweep reported and returns what it removed.
//
// Running containers held by another process are refused by default; the refusal is
// recorded on the provider so a caller who asked for a prune can see that something
// was skipped, rather than reading an empty result as "nothing was left".
func (s sweeper) Prune(ctx context.Context) ([]string, error) {
	detail, err := s.Inspect(ctx)
	if err != nil {
		return nil, err
	}
	var removed []string
	for _, it := range detail.Orphans {
		if err := s.p.remove(ctx, it.ID); err != nil {
			return removed, err
		}
		removed = append(removed, it.ID)
	}
	if len(detail.RunningForeign) > 0 || len(detail.HeldElsewhere) > 0 {
		if !s.p.d.PruneRunning {
			s.p.noteSkippedPrune(detail.Refused())
		} else {
			for _, it := range append(append([]listedContainer{}, detail.RunningForeign...), detail.HeldElsewhere...) {
				if err := s.p.remove(ctx, it.ID); err != nil {
					return removed, err
				}
				removed = append(removed, it.ID)
			}
		}
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

// ownerAlive reports whether the process named by an owner label is still running on this
// host. The label is host/pid/nonce; a label from a different host proves nothing about
// liveness here, so it is treated as unreachable and the container stays reclaimable only
// if it is stopped with no other claim.
func ownerAlive(owner string) bool {
	host, rest, _ := strings.Cut(owner, "/")
	if host != ownerHostOf(ownerID) {
		return false
	}
	pidStr, _, _ := strings.Cut(rest, "/")
	pid, err := strconv.Atoi(pidStr)
	if err != nil || pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// Signal 0 checks existence without disturbing the process.
	return proc.Signal(syscall.Signal(0)) == nil
}

// ownerHostOf extracts the host part of an owner label.
func ownerHostOf(owner string) string {
	host, _, _ := strings.Cut(owner, "/")
	return host
}

// splitOwner breaks an owner label into its host, pid and nonce parts.
func splitOwner(owner string) (host, pid, nonce string) {
	host, rest, _ := strings.Cut(owner, "/")
	pid, nonce, _ = strings.Cut(rest, "/")
	return host, pid, nonce
}
