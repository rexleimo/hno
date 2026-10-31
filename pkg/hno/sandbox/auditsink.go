package sandbox

import (
	"context"
	"time"
)

// AuditRecord is the compliance trail of one execution. It exists because the
// current file sandbox audits and the execution path does not: the half that
// actually runs untrusted code is the half nobody sees.
//
// Never put code, stdin, env values, argv secrets or absolute host paths in
// here — the projection is deliberately lossy for that reason.
type AuditRecord struct {
	Time        time.Time `json:"time"`
	Provider    string    `json:"provider"`
	Isolation   string    `json:"isolation"`
	Formation   string    `json:"formation"`
	RunID       string    `json:"run_id"`
	SessionID   string    `json:"session_id,omitempty"`
	AgentID     string    `json:"agent_id,omitempty"`
	TeamID      string    `json:"team_id,omitempty"`
	Session     string    `json:"session,omitempty"`
	User        string    `json:"user,omitempty"`
	TraceID     string    `json:"trace_id,omitempty"`
	Runtime     string    `json:"runtime,omitempty"`
	Argv        []string  `json:"argv,omitempty"`
	CodeBytes   int       `json:"code_bytes"`
	FileCount   int       `json:"file_count"`
	Workspace   string    `json:"workspace,omitempty"`
	MountMode   string    `json:"mount_mode,omitempty"`
	Effect      string    `json:"effect,omitempty"`
	Delivery    string    `json:"delivery,omitempty"`
	EgressMode  string    `json:"egress_mode,omitempty"`
	ExitCode    int       `json:"exit_code"`
	Duration    string    `json:"duration"`
	TimedOut    bool      `json:"timed_out,omitempty"`
	Lost        bool      `json:"lost,omitempty"`
	BlockedHost string    `json:"blocked_host,omitempty"`
	Artifacts   []string  `json:"artifacts,omitempty"`
	Failure     string    `json:"failure,omitempty"`
	Error       string    `json:"error,omitempty"`
}

// AuditSink receives one record per execution, terminal or refused. A sink that
// cannot keep up reports a fault: audit is never a blocking dependency of a run.
type AuditSink interface {
	WriteAudit(ctx context.Context, rec AuditRecord) error
}

// AuditSinkFunc adapts a function to AuditSink.
type AuditSinkFunc func(ctx context.Context, rec AuditRecord) error

// WriteAudit implements AuditSink.
func (f AuditSinkFunc) WriteAudit(ctx context.Context, rec AuditRecord) error { return f(ctx, rec) }

// // WithAudit installs the execution audit sink. This is configuration, not an
// observer: it is required for compliance use and its failure never changes
// execution results (§5.2).
func WithAudit(sink AuditSink, opts ...WithAuditOptions) Option {
	return func(c *Config) {
		box := &auditBox{sink: sink}
		if len(opts) > 0 {
			box.options = opts[0]
		}
		c.audit = box
	}
}

// auditBox pairs a sink with its projection so the handle keeps one dependency.
type auditBox struct {
	sink    AuditSink
	options WithAuditOptions
}

// WriteAudit implements AuditSink, so the box can be stored as a sink.
func (b *auditBox) WriteAudit(ctx context.Context, rec AuditRecord) error {
	if b == nil || b.sink == nil {
		return nil
	}
	b.options.projectRequest(nil, &rec)
	return b.sink.WriteAudit(ctx, rec)
}

// WithAuditOptions tunes audit projection for a handle.
type WithAuditOptions struct {
	// IncludeArgv records argv values; off by default because argv often
	// carries tokens.
	IncludeArgv bool
	// IncludeCodeSize records payload size only, never content.
	IncludeCodeSize bool
}

// auditOptions is the resolved form.
func (o WithAuditOptions) projectRequest(rc *RunContext, rec *AuditRecord) {
	if rc == nil || rc.Request == nil {
		return
	}
	req := rc.Request
	rec.Runtime = req.Runtime
	rec.CodeBytes = len(req.Code)
	rec.FileCount = len(req.Files)
	rec.Workspace = workspaceLabel(req.Workspace)
	rec.MountMode = req.Mode.String()
	rec.Effect = req.Effect.String()
	rec.Delivery = req.Delivery.String()
	rec.EgressMode = req.Egress.Mode.String()
	if o.IncludeArgv {
		rec.Argv = append([]string(nil), req.Argv...)
	}
}

// projectRequest applies the configured projection onto an audit record.
