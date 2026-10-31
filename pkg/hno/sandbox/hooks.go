package sandbox

import (
	"context"
	"fmt"
	"time"
)

// emit publishes one lifecycle fact to every observer. An observer fault is
// reported and execution continues: observation is not a safety boundary,
// steering is (§6.3, §7).
func (sb *Sandbox) emit(ctx context.Context, ev Event) {
	if ev.At.IsZero() {
		ev.At = time.Now().UTC()
	}
	if ev.Provider == "" {
		ev.Provider = sb.name
	}
	if ev.Isolation == IsolationUnknown {
		ev.Isolation = sb.iso
	}
	notifyCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	for i, o := range sb.cfg.Observers {
		name := hookName("observer", i, o)
		if err := guardedObserve(notifyCtx, o, ev); err != nil {
			sb.reportFault(ctx, "observer", name, err)
		}
	}
}

// guardedObserve calls one observer with panic recovery.
func guardedObserve(ctx context.Context, o Observer, ev Event) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	if o == nil {
		return nil
	}
	return o.Observe(ctx, ev)
}
