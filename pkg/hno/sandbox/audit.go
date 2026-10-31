package sandbox

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// recordAudit writes the compliance record for one execution. It runs on every
// terminal path — success, user failure, denial, timeout, loss — because a trail
// that skips the interesting cases is not an audit trail (§5.3).
func (sb *Sandbox) recordAudit(ctx context.Context, rc *RunContext, rep *Report, runErr error) {
	if sb.cfg.audit == nil {
		return
	}
	rec := AuditRecord{
		Time:      time.Now().UTC(),
		Provider:  sb.name,
		Isolation: sb.iso.String(),
		Formation: sb.cfg.Formation.String(),
		RunID:     rc.RunID,
		SessionID: rc.SessionID,
		AgentID:   rc.Caller.Agent,
		User:      rc.Caller.User,
		TraceID:   rc.Caller.TraceID,

		Runtime:     rc.Request.Runtime,
		CodeBytes:   len(rc.Request.Code),
		FileCount:   len(rc.Request.Files),
		Workspace:   workspaceLabel(rc.Request.Workspace),
		MountMode:   rc.Request.Mode.String(),
		Effect:      rc.Request.Effect.String(),
		Delivery:    rc.Request.Delivery.String(),
		EgressMode:  rc.Request.Egress.Mode.String(),
		ExitCode:    reportExit(rep),
		Duration:    reportDuration(rep).String(),
		TimedOut:    rep != nil && rep.TimedOut,
		Lost:        rep != nil && rep.Lost,
		BlockedHost: blockedHostOf(runErr, rep),
		Artifacts:   artifactNames(rep),
		Failure:     auditFailure(runErr, rep),
		Error:       errString(runErr),
	}
	if rec.ExitCode == 0 && runErr != nil {
		rec.ExitCode = -1
	}
	callCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
	defer cancel()
	// Audit failure never changes the run result: it is a finding for the
	// operator, not a reason to alter someone's execution (§5.3).
	_ = sb.cfg.audit.WriteAudit(callCtx, rec)
}

// auditFailure projects a failure into the audit vocabulary.
func auditFailure(runErr error, rep *Report) string {
	if rep != nil && rep.Failure != nil {
		return rep.Failure.Class.String()
	}
	if runErr != nil {
		return ClassOf(runErr).String()
	}
	return ""
}

// reportExit reads the exit code defensively.
func reportExit(rep *Report) int {
	if rep == nil {
		return -1
	}
	return rep.ExitCode
}

// reportDuration reads the duration defensively.
func reportDuration(rep *Report) time.Duration {
	if rep == nil {
		return 0
	}
	return rep.Duration
}

// blockedHostOf extracts the host a policy refused, when there is one.
func blockedHostOf(runErr error, rep *Report) string {
	if rep != nil && len(rep.BlockedHosts) > 0 {
		return rep.BlockedHosts[0]
	}
	var denied *DeniedError
	if errors.As(runErr, &denied) {
		return denied.Rule
	}
	return ""
}

// artifactNames lists collected artefacts for the trail.
func artifactNames(rep *Report) []string {
	if rep == nil {
		return nil
	}
	return rep.Artifacts.Names()
}

// errString renders an error or the empty string.
func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// firstLine renders the first line of an error for compact diagnostics.
func firstLine(err error) string {
	if err == nil {
		return ""
	}
	line, _, _ := strings.Cut(strings.TrimSpace(err.Error()), "\n")
	return line
}

// hookName resolves an extension's display name for faults and denials.
func hookName(prefix string, i int, v any) string {
	if n, ok := v.(Namer); ok {
		if name := n.Name(); name != "" {
			return name
		}
	}
	return fmt.Sprintf("%s#%d", prefix, i)
}

// reportFault publishes a hook fault through observers. A broken hook is a
// finding, never a reason to stop the run (§7).
func (sb *Sandbox) reportFault(ctx context.Context, kind, name string, cause error) {
	sb.emit(ctx, Event{Kind: EventHookFault, Detail: map[string]any{"kind": kind, "hook": name, "error": cause.Error()}})
}

// guardedPolicy calls one policy with panic recovery: a panicking guard is a
// fault, and faults fail closed (§6.3, §7).
func guardedPolicy(ctx context.Context, p Policy, rc *RunContext) (dec Decision, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	if p == nil {
		return Decision{}, fmt.Errorf("nil policy")
	}
	return p.Decide(ctx, rc)
}
