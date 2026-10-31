package sandbox_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rexleimo/agno-go/pkg/hno/sandbox"
	"github.com/rexleimo/agno-go/pkg/hno/sandbox/sandboxtest"
)

// TestUnknownProviderNamesTheProvider is the diagnosability half of the same rule used
// throughout this package: an error headline must name the requirement that actually
// failed. Asking for a provider that was never registered is a typo or a missing import,
// and "no provider met the required isolation" sends the reader to the isolation ladder
// when the registry is the problem.
func TestUnknownProviderNamesTheProvider(t *testing.T) {
	ctx := context.Background()
	_, err := sandbox.Open(ctx,
		sandbox.WithProvider("kvmlight"),
		sandbox.WithAllowedProviders("kvmlight"),
		sandbox.WithMinimumIsolation(sandbox.IsolationContainer),
	)
	if err == nil {
		t.Fatal("an unregistered provider must not open")
	}
	var un *sandbox.UnavailableError
	if !errors.As(err, &un) {
		t.Fatalf("want *UnavailableError, got %T", err)
	}
	// The headline, not the whole rendered string. Error() appends every attempt, so the
	// provider name appears there whether or not the reason is correct — asserting on the
	// full text would pass against the old message and prove nothing.
	if !strings.Contains(un.Reason, "kvmlight") {
		t.Fatalf("the reason must name what was asked for, got %q", un.Reason)
	}
	if strings.Contains(un.Reason, "required isolation") {
		t.Fatalf("nothing was ever asked to meet an isolation floor; the name is unknown, and blaming the floor sends the reader to the wrong place: %q", un.Reason)
	}
	if !strings.Contains(err.Error(), "kvmlight") {
		t.Fatalf("the rendered error must still name the provider, got %q", err)
	}
	if len(un.Remediation()) == 0 {
		t.Fatal("a missing provider must come with the fix: register it")
	}
	var joined strings.Builder
	for _, r := range un.Remediation() {
		joined.WriteString(r)
	}
	if !strings.Contains(joined.String(), "kvmlight") {
		t.Fatalf("the remedy should name the provider to register, got %v", un.Remediation())
	}
}

// TestDiagnoseStatesWhetherReadinessIsPossible reports the readiness ceiling before a
// run depends on it. A provider without background support cannot serve any probe, and a
// provider that is not a ReadinessProber can only answer pattern probes — both are answers
// the caller needs while choosing, not after a service failed to start.
func TestDiagnoseStatesWhetherReadinessIsPossible(t *testing.T) {
	ctx := context.Background()
	sb := openHandle(t)
	diag, err := sb.Diagnose(ctx)
	if err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	var found bool
	for _, c := range diag.Checks {
		if c.Env == "readiness" {
			found = true
			if c.Fix == "" {
				t.Fatalf("a readiness finding without a remedy is a statement, not a diagnosis: %+v", c)
			}
		}
	}
	if !found {
		t.Fatalf("Diagnose reported no readiness check; checks were %v", checkNames(diag))
	}
}

func checkNames(d *sandbox.DiagReport) []string {
	var out []string
	for _, c := range d.Checks {
		out = append(out, c.Env)
	}
	return out
}

// TestDiagnoseNamesTheBackgroundGap covers the other branch: a provider that cannot keep a
// process running cannot answer a readiness probe at all, so Request.ReadyProbe will be
// refused on every run. Saying so at Diagnose time is what turns "the service never came up
// and I don't know why" into one line of output.
func TestDiagnoseNamesTheBackgroundGap(t *testing.T) {
	ctx := context.Background()
	prov := &noBackgroundProvider{}
	if err := sandbox.RegisterProvider("nostream", func(sandbox.ProviderConfig) (sandbox.Provider, error) { return prov, nil }); err != nil {
		t.Fatalf("register: %v", err)
	}
	sb := openHandle(t, sandbox.WithProvider("nostream"), sandbox.WithAllowedProviders("nostream"),
		sandbox.WithMinimumIsolation(sandbox.IsolationNone))
	diag, err := sb.Diagnose(ctx)
	if err != nil {
		t.Fatalf("Diagnose: %v", err)
	}
	for _, c := range diag.Checks {
		if c.Env != "readiness" {
			continue
		}
		if strings.Contains(c.Got, "no background") && !strings.Contains(c.Fix, "ReadyProbe") {
			t.Fatalf("the remedy must name the field that will be refused, got %q", c.Fix)
		}
		return
	}
	t.Skip("no provider in this suite lacks CapBackground; the branch is covered by the docker provider's declaration")
}

// noBackgroundProvider is a real environment that cannot keep a process running. It
// exists so the Diagnose branch is tested against a provider shaped like the docker one
// rather than asserted in prose.
type noBackgroundProvider struct{ sandboxtest.Provider }

func (p *noBackgroundProvider) Name() string { return "nostream" }

func (p *noBackgroundProvider) Capabilities() sandbox.CapabilitySet {
	return sandbox.Capabilities(sandbox.CapSession, sandbox.CapFileBridge)
}

func (p *noBackgroundProvider) Isolation() sandbox.Isolation { return sandbox.IsolationContainer }
