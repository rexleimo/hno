package sandbox_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/rexleimo/agno-go/pkg/hno/sandbox"
	"github.com/rexleimo/agno-go/pkg/hno/sandbox/sandboxtest"
)

// eventCollector records the kinds an Observer is handed, so a promise about an event is
// checked against what was actually emitted.
type eventCollector struct {
	mu    sync.Mutex
	kinds []sandbox.EventKind
}

func (c *eventCollector) Observe(_ context.Context, ev sandbox.Event) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.kinds = append(c.kinds, ev.Kind)
	return nil
}

func (c *eventCollector) saw(k sandbox.EventKind) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, got := range c.kinds {
		if got == k {
			return true
		}
	}
	return false
}

// keepAlive builds a handle whose limits are expressed in milliseconds so a timeout can
// be proven without sleeping for the documented defaults.
func keepAlive(t *testing.T, ttl, idle time.Duration, obs ...sandbox.Observer) *sandbox.Sandbox {
	t.Helper()
	opts := []sandbox.Option{
		sandbox.WithTimeouts(sandbox.Timeouts{SessionTTL: ttl, Idle: idle, Cleanup: time.Second}),
	}
	for _, o := range obs {
		opts = append(opts, sandbox.WithObserver(o))
	}
	return openHandle(t, opts...)
}

// TestIdleReleasesAnUntouchedSession is the promise the config never kept: Timeouts.Idle
// was documented, defaulted to 15m, and had no consumer anywhere, while SessionTTL only
// produced an ExpiresAt that nothing acted on. An untouched session therefore held its
// environment for the life of the process.
func TestIdleReleasesAnUntouchedSession(t *testing.T) {
	ctx := context.Background()
	seen := &eventCollector{}
	sb := keepAlive(t, 0, 30*time.Millisecond, seen)
	sess, err := sb.Session(ctx, "idle", sandbox.WithCreateIfMissing(true))
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if _, err := sess.Exec(ctx, sandbox.Shell("echo hi")); err != nil {
		t.Fatalf("first Exec: %v", err)
	}
	time.Sleep(60 * time.Millisecond)

	if _, err := sess.Exec(ctx, sandbox.Shell("echo again")); err == nil {
		t.Fatal("a session past its idle limit still accepted work")
	} else {
		var se *sandbox.StateError
		if !errors.As(err, &se) {
			t.Fatalf("err = %T %v, want *StateError", err, err)
		}
		if se.Reason == "" {
			t.Fatal("the error must say the runtime released the session, not just that the state is illegal")
		}
		if se.Fix == "" {
			t.Fatal("the error must name the call that makes the action legal again")
		}
	}
	if st := sess.Status(); st.State != sandbox.StateReleased {
		t.Fatalf("state = %s, want released", st.State)
	}
	if !seen.saw(sandbox.EventSessionExpired) {
		t.Fatal("expiry must emit EventSessionExpired")
	}
}

// TestActivityPostponesIdleButNotTheAbsoluteLimit checks the two halves are genuinely
// different clocks. SessionTTL was previously computed from lastSeen, so a busy session
// could never reach its absolute deadline no matter how long it lived.
func TestActivityPostponesIdleButNotTheAbsoluteLimit(t *testing.T) {
	ctx := context.Background()
	sb := keepAlive(t, 120*time.Millisecond, 40*time.Millisecond)
	sess, err := sb.Session(ctx, "aged", sandbox.WithCreateIfMissing(true))
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	// Keep the session busy at a rate faster than the idle limit, for longer than the
	// absolute limit.
	deadline := time.Now().Add(200 * time.Millisecond)
	runs := 0
	for time.Now().Before(deadline) {
		if _, err := sess.Exec(ctx, sandbox.Shell("echo tick")); err != nil {
			break
		}
		runs++
		time.Sleep(20 * time.Millisecond)
	}
	if runs == 0 {
		t.Fatal("no run executed")
	}
	if _, err := sess.Exec(ctx, sandbox.Shell("echo after")); err == nil {
		t.Fatal("activity kept the session alive past its absolute lifetime; SessionTTL is not an idle timer")
	}
}

// TestNegativeLimitsMeanNoLimit proves a session can be made not to age. Zero cannot
// express that, because normalize() reads zero as "the caller did not decide" and fills
// the documented default; only an unset ExpiresAt proves the clock is off, since a zero
// deadline would otherwise read as "expires immediately".
func TestNegativeLimitsMeanNoLimit(t *testing.T) {
	ctx := context.Background()
	sb := keepAlive(t, -1, -1)
	sess, err := sb.Session(ctx, "forever", sandbox.WithCreateIfMissing(true))
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	time.Sleep(30 * time.Millisecond)
	if _, err := sess.Exec(ctx, sandbox.Shell("echo still here")); err != nil {
		t.Fatalf("a disabled clock must not release the session, got %v", err)
	}
	if st := sess.Status(); !st.ExpiresAt.IsZero() {
		t.Fatalf("with no limits configured ExpiresAt must be zero, got %v", st.ExpiresAt)
	}
}

// TestExpiryKeepsTheRecordedSpec is what makes handing control to the TTL safe: the
// session id and its replayable spec survive, so re-provisioning rebuilds the same
// environment rather than starting a blank one.
func TestExpiryKeepsTheRecordedSpec(t *testing.T) {
	ctx := context.Background()
	sb := keepAlive(t, 0, 20*time.Millisecond)
	sess, err := sb.Session(ctx, "spec", sandbox.WithCreateIfMissing(true))
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if err := sess.Install(ctx, sandbox.Pip("pandas==2.0.3")); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if err := sess.ExpireNow(ctx); err != nil {
		t.Fatalf("ExpireNow: %v", err)
	}
	if n := len(sess.Spec().Steps); n != 1 {
		t.Fatalf("expiry dropped the recorded spec: %d steps left", n)
	}
	// Coming back on the same id must re-provision and replay, not hand back a box
	// that claims to be ready while empty.
	again, err := sb.Session(ctx, "spec", sandbox.WithCreateIfMissing(true))
	if err != nil {
		t.Fatalf("re-acquire: %v", err)
	}
	if _, err := again.Exec(ctx, sandbox.Shell("echo back")); err != nil {
		t.Fatalf("Exec after re-acquire: %v", err)
	}
	if !sandboxtest.InstalledIn(t, "spec", "pandas==2.0.3") {
		t.Fatal("the re-provisioned session did not replay the recorded install")
	}
}
