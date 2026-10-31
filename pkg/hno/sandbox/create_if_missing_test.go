package sandbox_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rexleimo/agno-go/pkg/hno/sandbox"
	"github.com/rexleimo/agno-go/pkg/hno/sandbox/sandboxtest"
)

// WithCreateIfMissing(false) is the default, and its documented promise is that an unknown id
// is a *StaleSessionError. The implementation guarded that refusal on a session store being
// configured, so on a store-less handle the option did nothing: it booted a fresh environment
// and returned it as though it were the session being resumed. These tests hold the promise
// for both shapes of handle, because "no store" is the more dangerous case, not an exemption.

// openNamedHandle opens a handle on a provider instance the test keeps a reference to, so
// "was an environment provisioned" is answerable directly instead of inferred.
func openNamedHandle(t *testing.T, name string, opts ...sandbox.Option) (*sandbox.Sandbox, *sandboxtest.Provider) {
	t.Helper()
	if err := sandboxtest.Register(); err != nil {
		t.Fatalf("register: %v", err)
	}
	base, err := sandboxtest.NewProvider(sandbox.ProviderConfig{Image: "test:latest"})
	if err != nil {
		t.Fatalf("provider: %v", err)
	}
	mem := base.(*sandboxtest.Provider)
	if err := sandbox.RegisterProvider(name, func(sandbox.ProviderConfig) (sandbox.Provider, error) {
		return mem, nil
	}); err != nil {
		t.Fatalf("register named: %v", err)
	}
	t.Cleanup(func() { sandbox.UnregisterProvider(name) })
	opts = append([]sandbox.Option{
		sandbox.WithProvider(name),
		sandbox.WithAllowedProviders(name),
		sandbox.WithImage("test:latest"),
		sandbox.WithMinimumIsolation(sandbox.IsolationNone),
	}, opts...)
	sb, err := sandbox.Open(context.Background(), opts...)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = sb.Close() })
	return sb, mem
}

func openStoreLess(t *testing.T, opts ...sandbox.Option) *sandbox.Sandbox {
	t.Helper()
	if err := sandboxtest.Register(); err != nil {
		t.Fatalf("register: %v", err)
	}
	base := []sandbox.Option{
		sandbox.WithProvider("memory"),
		sandbox.WithAllowedProviders("memory"),
		sandbox.WithImage("test:latest"),
		sandbox.WithMinimumIsolation(sandbox.IsolationNone),
	}
	sb, err := sandbox.Open(context.Background(), append(base, opts...)...)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = sb.Close() })
	return sb
}

// TestUnknownIdIsRefusedWithoutAStore is the falsifier: against the previous code this
// returned a usable session and a live environment, which is what made a stale resume look
// like a successful one.
func TestUnknownIdIsRefusedWithoutAStore(t *testing.T) {
	ctx := context.Background()
	sb, mem := openNamedHandle(t, "cim-refuse")
	before := sb.Status().Sessions

	sess, err := sb.Session(ctx, "resuming-something-gone")
	if err == nil {
		t.Fatalf("an unknown id must not be handed back as a resumed session, got %v", sess)
	}
	var stale *sandbox.StaleSessionError
	if !errors.As(err, &stale) {
		t.Fatalf("want *StaleSessionError, got %T: %v", err, err)
	}
	if stale.SessionID != "resuming-something-gone" {
		t.Fatalf("the refusal must name the id, got %q", stale.SessionID)
	}
	if stale.Cause == nil || !strings.Contains(stale.Cause.Error(), "WithCreateIfMissing") {
		t.Fatalf("the refusal has to say how to proceed, got %v", stale.Cause)
	}
	if got := sb.Status().Sessions; got != before {
		t.Fatalf("a refusal must not have registered a session: %d -> %d", before, got)
	}
	// And no environment was booted for the refused id. That is the whole cost argument for
	// the option -- a stale resume should not pay for a box -- so it is asserted, not assumed.
	if mem.HasEnvironment("resuming-something-gone") {
		t.Fatal("a refused unknown id must not have provisioned an environment")
	}
}

// TestExplicitCreateStillWorks keeps the option working in both directions: this is the
// ergonomic path, and it must not be collateral damage.
func TestExplicitCreateStillWorks(t *testing.T) {
	ctx := context.Background()
	sb := openStoreLess(t)
	sess, err := sb.Session(ctx, "fresh", sandbox.WithCreateIfMissing(true))
	if err != nil {
		t.Fatalf("WithCreateIfMissing(true) must provision: %v", err)
	}
	if sess.ID() != "fresh" {
		t.Fatalf("wrong session, got %q", sess.ID())
	}
	// Once it exists, re-acquiring without the option is a resumption of a known id, not an
	// unknown one: the refusal is about not being able to verify the id, not about blocking
	// repeat calls.
	again, err := sb.Session(ctx, "fresh")
	if err != nil {
		t.Fatalf("re-acquiring a known id should work without the option: %v", err)
	}
	if again != sess {
		t.Fatal("re-acquire must return the same session handle")
	}
}

// TestReplayImpliesIntentToResume keeps the replay path working: a caller asking to replay a
// recorded spec is resuming, so it must not be forced to also declare creation.
func TestReplayImpliesIntentToResume(t *testing.T) {
	ctx := context.Background()
	sb := openStoreLess(t)
	sess, err := sb.Session(ctx, "made-first", sandbox.WithCreateIfMissing(true))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := sess.Exec(ctx, sandbox.Shell("export A=1")); err != nil {
		t.Fatalf("exec: %v", err)
	}
	if _, err := sb.Session(ctx, "made-first", sandbox.WithSessionReplay(true)); err != nil {
		t.Fatalf("replay on a known id: %v", err)
	}
}

// TestStoreBackedUnknownIdRefused covers the other branch, whose wording names the missing
// record rather than the missing store.
func TestStoreBackedUnknownIdRefused(t *testing.T) {
	ctx := context.Background()
	if err := sandboxtest.Register(); err != nil {
		t.Fatalf("register: %v", err)
	}
	store := sandboxtest.NewRecords()
	sb, err := sandbox.Open(ctx,
		sandbox.WithProvider("memory"),
		sandbox.WithAllowedProviders("memory"),
		sandbox.WithImage("test:latest"),
		sandbox.WithMinimumIsolation(sandbox.IsolationNone),
		sandbox.WithSessionStore(store),
	)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = sb.Close() })

	_, err = sb.Session(ctx, "never-recorded")
	var stale *sandbox.StaleSessionError
	if !errors.As(err, &stale) {
		t.Fatalf("want *StaleSessionError with a store present, got %v", err)
	}
	if !strings.Contains(stale.Cause.Error(), "no session record") {
		t.Fatalf("with a store the cause should name the missing record, got %v", stale.Cause)
	}
}

// TestRunProjectCreatesItsSession guards the one core call site that relied on the lenient
// behaviour: RunProject runs on a session it creates, and says so now.
func TestRunProjectCreatesItsSession(t *testing.T) {
	ctx := context.Background()
	sb := openStoreLess(t)
	res, err := sb.RunProject(ctx, sandbox.Project{
		Phases: []sandbox.Phase{{
			Name:     "compile",
			Run:      sandbox.Shell("export BUILT=1"),
			Required: true,
		}},
	}, sandbox.WithRunID("project-test-1"))
	if err != nil {
		t.Fatalf("RunProject on a fresh handle: %v", err)
	}
	if res == nil || len(res.Phases) != 1 {
		t.Fatalf("the project's phase must have run, got %+v", res)
	}
	// The session it created is a real one, not a phantom that only appeared to work.
	sess, err := sb.Session(ctx, "project-test-1")
	if err != nil {
		t.Fatalf("re-acquire the project session: %v", err)
	}
	if sess.ID() != "project-test-1" {
		t.Fatalf("wrong session, got %q", sess.ID())
	}
}
