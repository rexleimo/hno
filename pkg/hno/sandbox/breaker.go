package sandbox

import "errors"

// ErrBreakerOpen rejects work while a provider is cooling down. It is a
// rejection, not a failure to burn attempts on: an open breaker must never
// consume retry budget, matching the existing runner semantics (§16.5).
var ErrBreakerOpen = errors.New("sandbox: provider breaker open")

// Breaker is the circuit-breaking contract the core uses. The method set
// matches pkg/hno/observability.CircuitBreaker exactly, so callers wire it in
// without the core importing an instrumentation package.
type Breaker interface {
	Allow() bool
	Success()
	Failure()
}

// BreakerPolicy decides which failure classes count against a provider.
// Only engine-side noise belongs here: a high user-code failure rate is a
// model or prompt problem, and rebooting VMs for it is exactly the wrong
// remedy (§16.5).
type BreakerPolicy struct {
	Counted map[FailureClass]bool
}

// DefaultBreakerPolicy counts transient and lost-environment failures.
func DefaultBreakerPolicy() BreakerPolicy {
	return BreakerPolicy{Counted: map[FailureClass]bool{ClassTransient: true, ClassLost: true}}
}

// counts reports whether a class should trip the breaker.
func (p BreakerPolicy) counts(class FailureClass) bool {
	if p.Counted == nil {
		return DefaultBreakerPolicy().counts(class)
	}
	return p.Counted[class]
}
