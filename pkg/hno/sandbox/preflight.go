package sandbox

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"sort"
	"strings"
	"time"
)

// Preflight answers "why can't this run here?" before any handle exists.
//
// Sandbox.Diagnose cannot cover that case: it needs a provider that already
// resolved, and the moment a team most needs a diagnosis is the moment Open
// failed. Preflight walks the registry instead, reporting each candidate's
// verdict as data rather than as one opaque error (D-05).
func Preflight(ctx context.Context, opts ...Option) (*DiagReport, error) {
	cfg, err := buildPreflightConfig(opts)
	if err != nil {
		return nil, err
	}
	report := &DiagReport{Reached: IsolationUnknown}

	names := Providers()
	sort.Strings(names)
	registry := Check{
		Env:  "registry",
		Want: "at least one provider",
		Got:  fmt.Sprint(len(names)),
		OK:   len(names) > 0,
	}
	if !registry.OK {
		registry.Fix = "import a provider package, e.g. pkg/hno/sandbox/provider/boxlite"
	}
	report.Checks = append(report.Checks, registry)

	candidates := names
	if len(cfg.Allowed) > 0 {
		candidates = nil
		for _, n := range names {
			if containsString(cfg.Allowed, n) {
				candidates = append(candidates, n)
			}
		}
		allowed := Check{
			Env:  "allowed-providers",
			Want: strings.Join(cfg.Allowed, ", "),
			Got:  joinOrNone(candidates),
			OK:   len(candidates) > 0,
		}
		if !allowed.OK {
			allowed.Fix = "the profile allow-list excludes every registered provider: " + strings.Join(names, ", ")
		}
		report.Checks = append(report.Checks, allowed)
	}

	for _, name := range candidates {
		check := Check{Env: "provider:" + name, Want: cfg.MinIsolation.String()}
		prov, err := DefaultRegistry.build(ProviderConfig{
			Name:      name,
			Image:     cfg.Image,
			Formation: cfg.Formation,
		})
		if err != nil {
			check.OK = false
			check.Got = err.Error()
			if fix := firstFix(err); fix != "" {
				check.Fix = fix
				report.Fixes = append(report.Fixes, fix)
			}
			report.Checks = append(report.Checks, check)
			continue
		}
		iso := prov.Isolation()
		caps := prov.Capabilities()
		// Probe is the honest test: a provider can be registered and still be
		// unusable on this machine, and that is the finding a team needs.
		probeErr := probeAndClose(ctx, prov, cfg.Timeouts.Connect)
		check.Got = iso.String()
		check.OK = iso.AtLeast(cfg.MinIsolation) && probeErr == nil
		if probeErr != nil {
			check.Got = probeErr.Error()
			check.OK = false
			if fix := firstFix(probeErr); fix != "" {
				check.Fix = fix
			}
		} else if !check.OK {
			check.Fix = fmt.Sprintf("provider %q offers %s; either raise its isolation or lower the profile floor", name, iso)
		} else if iso.Rank() > report.Reached.Rank() {
			report.Reached = iso
			if !caps.Has(CapSession) {
				report.Fixes = append(report.Fixes, fmt.Sprintf("provider %q has no sessions: stateful work needs one", name))
			}
		}
		if check.Fix != "" {
			report.Fixes = append(report.Fixes, check.Fix)
		}
		report.Checks = append(report.Checks, check)
	}

	report.Checks = append(report.Checks, hostChecks()...)
	// A fix buried in a check row is a fix nobody reads: the report's Fixes list
	// is what Diagnose prints and what an agent can act on.
	for _, c := range report.Checks {
		if !c.OK && c.Fix != "" {
			report.Fixes = append(report.Fixes, c.Fix)
		}
	}
	report.Fixes = dedupeStrings(report.Fixes)
	if report.Reached.AtLeast(cfg.MinIsolation) {
		report.Summary = fmt.Sprintf("isolation %s is available", report.Reached)
	} else {
		report.Summary = summarizeChecks(report.Checks)
	}
	return report, nil
}

// hostChecks reports the machine-level facts that decide whether a micro-VM can
// run at all, independent of any provider.
func hostChecks() []Check {
	out := []Check{
		{Env: "os", Want: "linux or darwin", Got: runtime.GOOS, OK: runtime.GOOS == "linux" || runtime.GOOS == "darwin"},
		{Env: "arch", Want: "amd64 or arm64", Got: runtime.GOARCH, OK: runtime.GOARCH == "amd64" || runtime.GOARCH == "arm64"},
	}
	available := kvmAvailable()
	onHypervisor := runtime.GOOS == "darwin"
	kvm := Check{
		Env:  "kvm",
		Want: "/dev/kvm readable",
		Got:  kvmState(available),
		OK:   available || onHypervisor,
	}
	if !kvm.OK {
		// A fix on a passing check reads as a warning, so it is attached only
		// when the finding is real.
		kvm.Fix = "micro-VM providers need /dev/kvm on Linux; a VM or container needs nested virtualisation from its host"
	}
	out = append(out, kvm)
	return out
}

// kvmAvailable reports whether the machine exposes hardware virtualisation.
func kvmAvailable() bool {
	f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}

// kvmState renders the KVM finding for a report.
func kvmState(available bool) string {
	if available {
		return "present"
	}
	if runtime.GOOS == "darwin" {
		return "not applicable (Hypervisor.framework)"
	}
	return "absent"
}

// buildPreflightConfig resolves options for Preflight without requiring a
// provider to exist yet, which is the whole point of a pre-flight check.
func buildPreflightConfig(opts []Option) (*Config, error) {
	cfg := &Config{}
	for _, o := range opts {
		if o != nil {
			o(cfg)
		}
	}
	if cfg.Image == "" {
		cfg.Image = "python:3.12-slim"
	}
	if cfg.MinIsolation == IsolationUnknown {
		cfg.MinIsolation = IsolationContainer
	}
	return cfg, nil
}

// containsString reports membership without pulling in a set implementation.
func containsString(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}

// joinOrNone keeps a diagnostic line readable when the list is empty.
func joinOrNone(in []string) string {
	if len(in) == 0 {
		return "none"
	}
	return strings.Join(in, ", ")
}

// probeBounded asks a provider whether the engine answers, within the connect window.
//
// The question needs a deadline of its own. A CLI hanging on an unreachable daemon used
// to hang the caller for as long as it liked, while Timeouts.Connect was documented as the
// dial budget and nothing read it — the same "documented-shaped but unread" failure this
// package removes elsewhere. Ownership stays with the caller: a probe of the bound
// provider must not close the handle the sandbox is running on, so this only bounds the
// question.
func probeBounded(ctx context.Context, prov Provider, window time.Duration) error {
	if window <= 0 {
		window = 3 * time.Second
	}
	cctx, cancel := context.WithTimeout(ctx, window)
	defer cancel()
	return prov.Probe(cctx)
}

// probeAndClose is for candidates the caller is merely surveying: it answers the same
// question and then releases the instance, so a preflight sweep cannot leak a provider per
// name it tried.
func probeAndClose(ctx context.Context, prov Provider, window time.Duration) error {
	err := probeBounded(ctx, prov, window)
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), window)
	defer cancel()
	if cerr := prov.Close(cctx); cerr != nil && err == nil {
		return fmt.Errorf("sandbox: provider probed successfully but could not be closed: %w", cerr)
	}
	return err
}
