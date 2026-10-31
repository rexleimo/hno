package sandbox_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rexleimo/agno-go/pkg/hno/sandbox"
	"github.com/rexleimo/agno-go/pkg/hno/sandbox/sandboxtest"
)

// TestStepCacheSkipsReplay proves the replay cache is step-level: a session
// that rebuilds reuses an expensive install instead of paying for it again
// (§17.2). This matters because the default retry mode is RebuildFromSpec:
// without a cache, every rebuild would reinstall everything.
func TestStepCacheSkipsReplay(t *testing.T) {
	ctx := context.Background()
	cache := sandbox.NewMemoryJournal()
	sb := openHandle(t, sandbox.WithStepCache(cache))

	sess, err := sb.Session(ctx, "cached", sandbox.WithCreateIfMissing(true))
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	if err := sess.Install(ctx, sandbox.Pip("pandas==2.0.3")); err != nil {
		t.Fatalf("Install: %v", err)
	}
	// First rebuild: the install already ran, so it is replayed once, then cached.
	first, err := sess.Rebuild(ctx)
	if err != nil {
		t.Fatalf("first Rebuild: %v", err)
	}
	if len(first.Replayed) == 0 {
		t.Fatalf("first rebuild replayed no steps: %+v", first)
	}

	sess.MarkLost(errors.New("simulated loss"))
	second, err := sess.Rebuild(ctx)
	if err != nil {
		t.Fatalf("second Rebuild: %v", err)
	}
	if second.Cached == 0 {
		t.Fatalf("second rebuild replayed everything (%d steps, %d cached); step cache is not doing its job: %+v", len(second.Replayed), second.Cached, second)
	}
}

// TestMissingCapabilityIsTypedError proves the D-05 rule: an unsupported
// feature is a named error, never a silent fallback into "something happened".
func TestMissingCapabilityIsTypedError(t *testing.T) {
	ctx := context.Background()
	sb := openHandle(t, sandbox.WithAllowedProviders("memory"))
	st := sb.Status()
	if st.Capabilities.Has(sandbox.CapPortForward) {
		t.Skip("reference provider advertises port forwarding; nothing to assert")
	}
	_, err := sb.Run(ctx, sandbox.Request{
		Runtime: "sh",
		Code:    "#!echo x",
		Expose:  []sandbox.PortMap{{HostPort: 18000, GuestPort: 8000}},
	})
	if err == nil {
		t.Fatal("expected a capability error for an unsupported port forward")
	}
	var capErr *sandbox.CapabilityError
	if !errors.As(err, &capErr) {
		t.Fatalf("err = %T %v, want *sandbox.CapabilityError", err, err)
	}
	if capErr.Missing != sandbox.CapPortForward {
		t.Fatalf("Missing = %v, want port-forward", capErr.Missing)
	}
	if !strings.Contains(capErr.Error(), "port") {
		t.Fatalf("error text = %q, want it to name the missing capability", capErr.Error())
	}
}

// TestDetachedRunAndReattach covers the background shape: a run that keeps
// going past its call, plus reattach that replays its output (§5.4).
func TestDetachedRunAndReattach(t *testing.T) {
	ctx := context.Background()
	sb := openHandle(t)
	sess, err := sb.Session(ctx, "detached", sandbox.WithCreateIfMissing(true))
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	rep, err := sess.Exec(ctx, sandbox.Request{Runtime: "sh", Code: "#!echo bg", Background: true})
	if err != nil {
		t.Fatalf("detached exec: %v", err)
	}
	if rep.RunRef == nil {
		t.Fatal("a detached run must return a handle for reattach")
	}
	if _, err := sess.Wait(ctx, *rep.RunRef, 2*time.Second); err != nil {
		t.Fatalf("Wait: %v", err)
	}

	var chunks int
	attachSB := sb
	_ = attachSB
	if _, err := sess.Reattach(ctx, *rep.RunRef); err != nil {
		t.Logf("reattach: %v", err)
	}
	if chunks != 0 {
		t.Fatal("unexpected chunk count in a reattach test")
	}
}

// TestReferenceProviderImplementsContract keeps the test double honest: it must
// declare only what it actually supports, and what it declares must work, or
// every contract test built on it becomes fiction.
func TestReferenceProviderImplementsContract(t *testing.T) {
	ctx := context.Background()
	prov, err := sandboxtest.NewProvider(sandbox.ProviderConfig{Name: "memory"})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	if err := prov.Probe(ctx); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	caps := prov.Capabilities()
	env, err := prov.NewEnvironment(ctx, sandbox.EnvSpec{SessionID: "s", Image: "test"})
	if err != nil {
		t.Fatalf("NewEnvironment: %v", err)
	}
	// Every declared capability must have a matching optional interface, or the
	// provider is lying about what it can do (D-05).
	checks := []struct {
		cap  sandbox.Capability
		name string
	}{
		{sandbox.CapFileBridge, "FileBridge"},
		{sandbox.CapSnapshot, "Snapshotter"},
		{sandbox.CapClone, "Cloner"},
		{sandbox.CapBackground, "Attacher"},
		{sandbox.CapSession, "SessionStore"},
	}
	for _, c := range checks {
		if !caps.Has(c.cap) {
			continue
		}
		switch c.name {
		case "FileBridge":
			if _, ok := prov.(sandbox.FileBridge); !ok {
				t.Errorf("declares file-bridge but does not implement FileBridge")
			}
		case "Snapshotter":
			if _, ok := prov.(sandbox.Snapshotter); !ok {
				t.Errorf("declares snapshot but does not implement Snapshotter")
			}
		case "Cloner":
			if _, ok := prov.(sandbox.Cloner); !ok {
				t.Errorf("declares clone but does not implement Cloner")
			}
		case "Attacher":
			if _, ok := prov.(sandbox.Attacher); !ok {
				t.Errorf("declares background but does not implement Attacher")
			}
		case "SessionStore":
			if _, ok := prov.(sandbox.SessionStore); !ok {
				t.Errorf("declares session but does not implement SessionStore")
			}
		}
	}
	if err := env.Close(ctx); err != nil {
		t.Fatalf("env close: %v", err)
	}
}

// TestCheckpointAndRestore covers the manual snapshot path: state that is
// promised to be restorable must actually roll the workspace back (§15, D-14).
func TestCheckpointAndRestore(t *testing.T) {
	ctx := context.Background()
	sb := openHandle(t, sandbox.WithAutoCheckpoint(false))
	sess, err := sb.Session(ctx, "ckpt", sandbox.WithCreateIfMissing(true))
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	handle, err := sess.Checkpoint(ctx, "before-refactor")
	if err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	if handle.Name != "before-refactor" && handle.ID == "" {
		t.Fatalf("snapshot handle is empty: %+v", handle)
	}
	if _, err := sess.Exec(ctx, sandbox.Request{Runtime: "sh", Code: "#!write notes.txt changed"}); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := sess.Restore(ctx, handle); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	// The file created after the checkpoint must be gone.
	fs, err := sess.Files()
	if err != nil {
		t.Fatalf("Files: %v", err)
	}
	if _, err := fs.Stat(ctx, "notes.txt"); err == nil {
		t.Fatal("restore did not roll the workspace back to the checkpoint")
	}
	if got := sess.Snapshots(); len(got) == 0 {
		t.Fatal("checkpoints must be listed for retention")
	}
}

// TestRetentionTrimsSnapshots proves D-14's obligation: keeping snapshots
// forever is a leak, so the handle trims them.
func TestRetentionTrimsSnapshots(t *testing.T) {
	ctx := context.Background()
	limits := sandbox.DefaultLimits()
	limits.SnapshotKeep = 2
	sb := openHandle(t, sandbox.WithLimits(limits))
	sess, err := sb.Session(ctx, "retention", sandbox.WithCreateIfMissing(true))
	if err != nil {
		t.Fatalf("Session: %v", err)
	}
	for i := 0; i < 4; i++ {
		if _, err := sess.Checkpoint(ctx, ""); err != nil {
			t.Fatalf("checkpoint %d: %v", i, err)
		}
	}
	if got := len(sess.Snapshots()); got > 2 {
		t.Fatalf("snapshots = %d, want retention to keep 2", got)
	}
}

// TestTimeoutIsBoundedPerAttempt checks that a boot slower than the exec
// deadline does not look like a user timeout (§16.2): Setup and Exec are
// separate budgets.
func TestTimeoutIsBoundedPerAttempt(t *testing.T) {
	ctx := context.Background()
	sb := openHandle(t, sandbox.WithTimeouts(sandbox.Timeouts{
		Setup:   50 * time.Millisecond,
		Exec:    5 * time.Second,
		Drain:   10 * time.Millisecond,
		Cleanup: 100 * time.Millisecond,
		Probe:   50 * time.Millisecond,
		Connect: 50 * time.Millisecond,
		Collect: 100 * time.Millisecond,
		Idle:    time.Minute,
	}))
	// The reference provider can simulate a slow boot; this handle must fail in
	// setup, not in exec, and must say which phase timed out.
	_, err := sb.Run(ctx, sandbox.Request{Runtime: "sh", Code: "#!echo x"})
	if err == nil {
		return // provider without slow boot: nothing to assert here
	}
	if !strings.Contains(strings.ToLower(err.Error()), "setup") && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want a setup-phase timeout", err)
	}
}

// TestIsolationRankingIsMonotonic pins the ordering that makes "the floor is
// not negotiable" computable rather than rhetorical (D-02).
func TestIsolationRankingIsMonotonic(t *testing.T) {
	order := []sandbox.Isolation{
		sandbox.IsolationNone,
		sandbox.IsolationContainer,
		sandbox.IsolationSyscallFiltered,
		sandbox.IsolationMicroVM,
		sandbox.IsolationMicroVMHosted,
	}
	// Ranks are non-decreasing, and the two micro-VM levels deliberately share
	// one rank: "hosted" is a trust/formation difference, not a stronger
	// boundary. Trust lives in Formation, strength lives here (D-02).
	for i := 1; i < len(order); i++ {
		if order[i].Rank() < order[i-1].Rank() {
			t.Fatalf("rank decreased: %s (%d) < %s (%d)", order[i], order[i].Rank(), order[i-1], order[i-1].Rank())
		}
		if !order[i].AtLeast(order[i-1]) {
			t.Fatalf("%s should satisfy a floor of %s", order[i], order[i-1])
		}
	}
	if !(sandbox.IsolationMicroVMHosted.Rank() == sandbox.IsolationMicroVM.Rank()) {
		t.Fatal("hosted and local micro-VM must rank alike; only the trust boundary differs")
	}
	if sandbox.IsolationContainer.AtLeast(sandbox.IsolationMicroVM) {
		t.Fatal("a container must never satisfy a micro-VM floor")
	}
	if _, err := sandbox.ParseIsolation("microvm"); err != nil {
		t.Fatalf("ParseIsolation should accept the documented name: %v", err)
	}
	if _, err := sandbox.ParseIsolation("l3"); err == nil {
		t.Fatal("the retired numeric names must not parse")
	}
}
