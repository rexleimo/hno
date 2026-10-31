package sandbox_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/rexleimo/agno-go/pkg/hno/sandbox"
	"github.com/rexleimo/agno-go/pkg/hno/sandbox/sandboxtest"
)

// D-36: "the engine can create" and "this image can start" are different claims, and the
// health report used to answer only the first while looking like it answered both.
//
// Every test here fails against the pre-fix code for the same reason: there was no image
// check at all, so an absent image produced "all checks passed".

// queryProvider wraps the memory double and answers the image question from knobs the test
// controls.
type queryProvider struct {
	*sandboxtest.Provider
	name string
}

// noQueryProvider is a provider that can create environments but cannot answer the image
// question. Delegation rather than embedding is the point: embedding would promote the
// method and the assertion this test makes would always succeed.
type noQueryProvider struct {
	inner sandbox.Provider
}

func (p noQueryProvider) Name() string                 { return p.inner.Name() }
func (p noQueryProvider) Isolation() sandbox.Isolation { return p.inner.Isolation() }

// Capabilities is deliberately empty. The wrapper delegates only the six core methods, so
// forwarding the double's full set would claim FileBridge, Snapshotter and Attacher that it
// no longer provides -- and the core refuses exactly that over-claim at bind time. The first
// version of this fixture did forward them and Open refused it, which is the core being right
// and the fixture being wrong.
func (p noQueryProvider) Capabilities() sandbox.CapabilitySet { return 0 }
func (p noQueryProvider) Probe(ctx context.Context) error     { return p.inner.Probe(ctx) }
func (p noQueryProvider) NewEnvironment(ctx context.Context, spec sandbox.EnvSpec) (sandbox.Environment, error) {
	return p.inner.NewEnvironment(ctx, spec)
}
func (p noQueryProvider) Close(ctx context.Context) error { return p.inner.Close(ctx) }

var registerOnce sync.Map

// registerFixture installs a named provider once per test, so tests do not fight over the
// registry.
func registerFixture(t *testing.T, name string, factory func(sandbox.ProviderConfig) (sandbox.Provider, error)) {
	t.Helper()
	if _, loaded := registerOnce.LoadOrStore(name, true); loaded {
		t.Fatalf("fixture provider %q was already registered; use a name of your own", name)
	}
	if err := sandbox.RegisterProvider(name, factory); err != nil {
		t.Fatalf("register %q: %v", name, err)
	}
}

func openWith(t *testing.T, name string, image string) *sandbox.Sandbox {
	t.Helper()
	sb, err := sandbox.Open(context.Background(),
		sandbox.WithProvider(name),
		sandbox.WithAllowedProviders(name),
		sandbox.WithImage(image),
	)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = sb.Close() })
	return sb
}

func findCheck(diag *sandbox.DiagReport, env string) (sandbox.Check, bool) {
	for _, c := range diag.Checks {
		if c.Env == env {
			return c, true
		}
	}
	return sandbox.Check{}, false
}

func TestDiagnoseImageAbsentIsNotHealthy(t *testing.T) {
	name := "image-absent-fixture"
	var prov *sandboxtest.Provider
	registerFixture(t, name, func(cfg sandbox.ProviderConfig) (sandbox.Provider, error) {
		base, err := sandboxtest.NewProvider(cfg)
		if err != nil {
			return nil, err
		}
		prov = base.(*sandboxtest.Provider)
		prov.AbsentImages = []string{"needs-a-pull:1"}
		return base, nil
	})

	sb := openWith(t, name, "needs-a-pull:1")
	diag, err := sb.Diagnose(context.Background())
	if err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	c, hit := findCheck(diag, "image")
	if !hit {
		t.Fatal(`no "image" check at all: the report answers "can the engine create" and prints it as if it had answered "can this image start"`)
	}
	if c.OK {
		t.Errorf("an image the engine does not have was reported as healthy: %+v", c)
	}
	if !strings.Contains(c.Got, "needs-a-pull:1") {
		t.Errorf("the failing check must name the image, got %q", c.Got)
	}
	if !strings.Contains(c.Fix, "pull") && !strings.Contains(c.Fix, "WithImage") {
		t.Errorf("the fix has to say what to do about the image, got %q", c.Fix)
	}
	// A finding that only appears in the check list is easy to miss in a summary line.
	if !strings.Contains(diag.Summary, "image") && !containsFold(diag.Fixes, "needs-a-pull:1") {
		t.Errorf("the image problem must also reach the report's summary or fixes, got %q / %v", diag.Summary, diag.Fixes)
	}
}

func TestDiagnoseImagePresentIsHealthy(t *testing.T) {
	name := "image-present-fixture"
	registerFixture(t, name, func(cfg sandbox.ProviderConfig) (sandbox.Provider, error) {
		base, err := sandboxtest.NewProvider(cfg)
		if err != nil {
			return nil, err
		}
		return base, nil
	})
	sb := openWith(t, name, "already-here:2")
	diag, err := sb.Diagnose(context.Background())
	if err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	c, hit := findCheck(diag, "image")
	if !hit {
		t.Fatal(`expected an "image" check when the image is fine, so a reader can see it was asked`)
	}
	if !c.OK {
		t.Errorf("the double says the image is present, so the check must not read as a failure: %+v", c)
	}
}

// An engine that would not answer is the state most likely to be papered over, because both
// rounding directions look defensible. D-31's rule applies: cannot verify is a refusal, not
// a pass.
func TestDiagnoseImageUnknownIsNotAPass(t *testing.T) {
	name := "image-unknown-fixture"
	registerFixture(t, name, func(cfg sandbox.ProviderConfig) (sandbox.Provider, error) {
		base, err := sandboxtest.NewProvider(cfg)
		if err != nil {
			return nil, err
		}
		p := base.(*sandboxtest.Provider)
		p.ImageQueryErr = errors.New("registry says 401, no credentials")
		return p, nil
	})
	sb := openWith(t, name, "private.reg/img:tag")
	diag, err := sb.Diagnose(context.Background())
	if err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	c, hit := findCheck(diag, "image")
	if !hit {
		t.Fatal(`no "image" check: an unanswered image question disappeared from the report entirely`)
	}
	if c.OK {
		t.Errorf("an engine that refused to answer produced a healthy image check: %+v", c)
	}
	if !strings.Contains(strings.ToLower(c.Got), "unknown") {
		t.Errorf("the report must say the answer is unknown rather than guess, got %q", c.Got)
	}
	if !strings.Contains(c.Got, "401") {
		t.Errorf("the engine's own reason has to travel with the finding, got %q", c.Got)
	}
}

func TestDiagnoseProviderWithoutImageQueryIsVisible(t *testing.T) {
	name := "image-no-query-fixture"
	registerFixture(t, name, func(cfg sandbox.ProviderConfig) (sandbox.Provider, error) {
		base, err := sandboxtest.NewProvider(cfg)
		if err != nil {
			return nil, err
		}
		return noQueryProvider{inner: base}, nil
	})
	sb := openWith(t, name, "unverifiable:1")
	diag, err := sb.Diagnose(context.Background())
	if err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	c, hit := findCheck(diag, "image")
	if !hit {
		t.Fatal(`a provider that cannot answer the image question produced no image check, so the gap is invisible`)
	}
	if c.OK {
		t.Errorf("an unanswerable image question was reported as healthy: %+v", c)
	}
	if !strings.Contains(c.Fix, "ImageQueryable") {
		t.Errorf("the fix must name the interface to implement, got %q", c.Fix)
	}
}

// A handle that was never given an image still resolves one -- DefaultImage -- so the image
// question exists for it too. Written as "no image means no check", this test asserted a
// state that does not occur and the run output proved it: the check reported the default.
// The property that matters is that nobody is exempted from verification by leaving the
// field unset, so that is what this now pins.
func TestDiagnoseVerifiesTheResolvedDefaultImage(t *testing.T) {
	name := "image-unset-fixture"
	registerFixture(t, name, func(cfg sandbox.ProviderConfig) (sandbox.Provider, error) {
		return sandboxtest.NewProvider(cfg)
	})
	sb, err := sandbox.Open(context.Background(),
		sandbox.WithProvider(name),
		sandbox.WithAllowedProviders(name),
		sandbox.WithImage(""),
	)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer sb.Close()
	diag, err := sb.Diagnose(context.Background())
	if err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	c, hit := findCheck(diag, "image")
	if !hit {
		t.Fatal("WithImage(\"\") resolved to a real image, yet no image was verified: leaving the field unset would exempt a handle from the check while still booting against DefaultImage")
	}
	if !strings.Contains(c.Got, sandbox.DefaultImage) {
		t.Errorf("the check must name the image actually in force (%q), got %q", sandbox.DefaultImage, c.Got)
	}
}

func containsFold(list []string, want string) bool {
	for _, s := range list {
		if strings.Contains(s, want) {
			return true
		}
	}
	return false
}
