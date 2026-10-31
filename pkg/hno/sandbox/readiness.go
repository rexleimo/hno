package sandbox

import (
	"context"
	"fmt"
	"regexp"
	"time"
)

// ReadinessProber evaluates the in-guest half of a ReadyProbe.
//
// A pattern can be matched from the output the core already receives, but "port 8080
// is accepting connections" and "this URL answers 2xx" can only be answered from
// inside the sandbox's network namespace. Providers that claim they can wait for a
// service have to prove they can ask, so the requirement is an interface rather than
// an assumption.
type ReadinessProber interface {
	// ProbeReady reports whether the condition is satisfied now, plus what the
	// provider actually saw so a failure can be explained.
	ProbeReady(ctx context.Context, env Environment, probe ReadyProbe) (ready bool, observed string, err error)
}

// readinessPollInterval is how often the core re-checks a probe. It is short because
// a service that came up two seconds ago should not look slow, and bounded because a
// probe loop must not become a busy-wait against the provider.
const readinessPollInterval = 100 * time.Millisecond

// waitReady holds a background run to its stated contract: the run is not reported as
// started until the probe is satisfied, or the probe window expires and the caller
// gets a ReadinessError.
//
// Before this existed the field was decorative. Validate() rejected an empty probe and
// clone() copied the whole struct, but nothing read URL, Port, Pattern, or Deadline, so
// "start the service and wait until it is ready" silently degraded to "return
// immediately" — the caller's next request hit a connection refused and the sandbox
// reported a successful run.
func (sb *Sandbox) waitReady(ctx context.Context, env Environment, fan *fanout, ref *RunRef, probe *ReadyProbe, budget time.Duration) error {
	if probe == nil || ref == nil {
		return nil
	}
	window := probe.Deadline
	if window <= 0 {
		window = budget
	}
	if window <= 0 {
		window = sb.cfg.Timeouts.Setup
	}
	if window <= 0 {
		window = 30 * time.Second
	}
	deadline := time.Now().Add(window)
	pat := probe.Pattern
	var matcher *regexp.Regexp
	if pat != "" {
		if re, err := regexp.Compile(pat); err == nil {
			matcher = re
		}
	}
	var lastSeen string
	for {
		ready, observed, err := sb.probeOnce(ctx, env, fan, probe, matcher, pat)
		if err != nil {
			// A provider that cannot answer the question is not an answer of "no".
			return err
		}
		if ready {
			return nil
		}
		if observed != "" {
			lastSeen = observed
		}
		if !time.Now().Before(deadline) {
			p := *probe
			p.Deadline = window
			return &ReadinessError{RunID: ref.ID, Probe: p, LastSeen: lastSeen}
		}
		select {
		case <-ctx.Done():
			p := *probe
			p.Deadline = window
			return &ReadinessError{RunID: ref.ID, Probe: p, LastSeen: fmt.Sprintf("context ended: %v", ctx.Err())}
		case <-time.After(readinessPollInterval):
		}
	}
}

// probeOnce evaluates one sample of the readiness condition.
func (sb *Sandbox) probeOnce(ctx context.Context, env Environment, fan *fanout, probe *ReadyProbe, matcher *regexp.Regexp, literal string) (bool, string, error) {
	if probe.Pattern != "" {
		out := fan.outputSnapshot()
		if len(out) > 0 {
			if matcher != nil {
				if matcher.Match(out) {
					return true, "", nil
				}
			} else if containsBytes(out, []byte(literal)) {
				// A pattern that is not a valid regexp still has to be honoured as a
				// substring, so an invalid expression cannot turn into a refusal to
				// check at all.
				return true, "", nil
			}
			tail := out
			if len(tail) > 240 {
				tail = tail[len(tail)-240:]
			}
			return false, string(tail), nil
		}
		return false, "", nil
	}
	// Port and URL can only be answered from inside the guest.
	prober, ok := sb.provider.(ReadinessProber)
	if !ok {
		return false, "", &UnsupportedProbeError{
			Provider: sb.name,
			Kind:     probeKind(probe),
			Fix: fmt.Sprintf("provider %q cannot evaluate %s; use ReadyProbe.Pattern, which the core matches on the run's own output, or choose a provider that implements ReadinessProber",
				sb.name, probeKind(probe)),
		}
	}
	return prober.ProbeReady(ctx, env, *probe)
}

func probeKind(p *ReadyProbe) string {
	switch {
	case p.Port != 0:
		return fmt.Sprintf("port %d", p.Port)
	case p.URL != "":
		return "URL " + p.URL
	default:
		return "pattern " + p.Pattern
	}
}

func containsBytes(hay, needle []byte) bool {
	if len(needle) == 0 {
		return true
	}
	for i := 0; i+len(needle) <= len(hay); i++ {
		match := true
		for j := range needle {
			if hay[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// UnsupportedProbeError refuses a readiness condition the provider cannot evaluate.
//
// It is returned instead of quietly passing the probe, because "the field is set but
// nothing checks it" is the defect this file exists to remove.
type UnsupportedProbeError struct {
	Provider string
	Kind     string
	// Fix is the caller-facing remedy. Named Fix rather than Remediation because
	// Remediation() is the accessor the other failure types share, and a field cannot
	// occupy that name.
	Fix string
}

func (e *UnsupportedProbeError) Error() string {
	return fmt.Sprintf("sandbox: provider %q cannot evaluate ready probe %s", e.Provider, e.Kind)
}

// Class is permanent: no retry makes the provider grow a capability it lacks.
func (e *UnsupportedProbeError) Class() FailureClass { return ClassPermanent }

// Remediation returns the caller-facing fix.
func (e *UnsupportedProbeError) Remediation() string { return e.Fix }

// remediationer is the accessor the actionable failures in this package share. It is
// named here so a new error type cannot ship without one unnoticed.
type remediationer interface {
	Remediation() string
}

// Compile-time shape checks: these errors have to satisfy the interfaces the rest of
// the failure taxonomy does, or errors.As and the retry layer will not see them.
var (
	_ error         = (*UnsupportedProbeError)(nil)
	_ classifier    = (*UnsupportedProbeError)(nil)
	_ remediationer = (*UnsupportedProbeError)(nil)
	_ remediationer = (*ReadinessError)(nil)
)
