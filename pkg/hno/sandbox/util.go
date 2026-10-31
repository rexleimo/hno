package sandbox

import (
	"errors"
	"fmt"
	"strings"
)

// String implements fmt.Stringer.
func (d Delivery) String() string {
	switch d {
	case DeliveryAtMostOnce:
		return "at-most-once"
	case DeliveryAtLeastOnce:
		return "at-least-once"
	case DeliveryExactlyOnce:
		return "exactly-once"
	default:
		return ""
	}
}

// policyName resolves a policy's display name for DeniedError and faults.
func policyName(i int, p Policy) string {
	if n, ok := p.(Namer); ok {
		if name := n.Name(); name != "" {
			return name
		}
	}
	return fmt.Sprintf("policy#%d", i)
}

// describeRequest renders a one-line summary of a request for approval UI.
func describeRequest(req *Request) string {
	if req == nil {
		return "empty request"
	}
	switch {
	case len(req.Argv) > 0:
		return strings.Join(req.Argv, " ")
	case req.Runtime != "":
		return fmt.Sprintf("%s payload (%d bytes)", req.Runtime, len(req.Code))
	default:
		return "request"
	}
}

// describeFailure explains a report's failure in model-readable form (§16.6).
func describeFailure(rep *Report) string {
	if rep == nil {
		return "no report"
	}
	// The payload's own diagnostic is appended whenever there is one. "command
	// exited with code 1" is not something a model can act on; the reason lives in
	// the output the run already captured, and dropping it is what made the old
	// run toolkit look broken when user code merely failed (§16.4).
	withDetail := func(head string) string {
		detail := tailExcerpt(rep.Stderr, failureDetailBytes)
		if detail == "" {
			return head
		}
		return head + ": " + detail
	}
	switch {
	case rep.Lost:
		return withDetail("environment lost during run")
	case rep.TimedOut:
		return withDetail("run exceeded its deadline")
	case rep.ExitCode != 0:
		return withDetail(fmt.Sprintf("command exited with code %d", rep.ExitCode))
	default:
		return withDetail("run did not succeed")
	}
}

// privilegeHash scores how privileged a request is, so "a policy may only
// tighten" is a comparison instead of a hope (§6.3).
func privilegeHash(req *Request) int {
	if req == nil {
		return 0
	}
	score := 0
	// EgressUnset is scored as open, not as zero. Unset means "whatever the
	// profile or provider default is", and scoring it as harmless would reject
	// the single most useful policy of all: narrowing a request that never
	// mentioned egress.
	switch req.Egress.Mode {
	case EgressUnset, EgressOpen:
		score += 1000
	case EgressAllowlist:
		score += 10 + len(req.Egress.Allow)
	case EgressNone:
		if len(req.Egress.Allow) > 0 {
			// An allow-list under EgressNone is contradictory; score the asks so
			// a policy that removes them is still read as a tightening.
			score += len(req.Egress.Allow)
		}
	}
	if req.Workspace != nil && req.Workspace.Kind == SourceDir {
		score += 5
	}
	score += len(req.Env)
	return score
}

// fixesOf pulls remediation hints out of any error that carries them.
func fixesOf(err error) []string {
	var f fixer
	if errors.As(err, &f) {
		return f.Remediation()
	}
	return nil
}

// failureDetailBytes bounds how much of a phase's output goes into a Failure
// cause. Enough for a compiler to name the file and line, small enough that a
// failure that travels into a tool result cannot swamp the context window.
const failureDetailBytes = 1200

// tailExcerpt keeps the last maxBytes of s on a line boundary, prefixed with an
// omission marker when bytes were dropped. The tail rather than the head because
// build and test tools print the reason last.
func tailExcerpt(s string, maxBytes int) string {
	s = strings.TrimSpace(s)
	if s == "" || maxBytes <= 0 {
		return s
	}
	if len(s) <= maxBytes {
		return s
	}
	cut := len(s) - maxBytes
	// Advance to the next line start so the excerpt does not begin mid-token.
	if nl := strings.IndexByte(s[cut:], '\n'); nl >= 0 && cut+nl+1 < len(s) {
		cut += nl + 1
	}
	return fmt.Sprintf("[…%d bytes omitted] %s", cut, strings.TrimSpace(s[cut:]))
}

// formatPortMaps renders published ports for an error message.
func formatPortMaps(maps []PortMap) string {
	if len(maps) == 0 {
		return "no published ports"
	}
	parts := make([]string, 0, len(maps))
	for _, m := range maps {
		parts = append(parts, fmt.Sprintf("%d->%d", m.HostPort, m.GuestPort))
	}
	return strings.Join(parts, ", ")
}
