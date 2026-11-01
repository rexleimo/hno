package boxlite

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/rexleimo/agno-go/pkg/hno/sandbox"
)

// Environment is one running box.
type Environment struct {
	provider *Provider
	id       string
	session  string

	mu      sync.Mutex
	closed  bool
	lastErr error
}

// ID implements sandbox.Environment.
func (e *Environment) ID() string { return e.id }

// Close implements sandbox.Environment. It is idempotent: a second close reports
// success rather than a spurious engine error.
func (e *Environment) Close(ctx context.Context) error {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil
	}
	e.closed = true
	e.mu.Unlock()
	err := e.provider.remove(ctx, e.id)
	e.mu.Lock()
	e.lastErr = err
	e.mu.Unlock()
	return err
}

// Exec implements sandbox.Environment.
//
// Exit-code semantics follow the project contract: a nonzero exit is the payload
// speaking, so it returns a populated report with a user-code failure and a nil error.
// The CLI itself failing to run is infrastructure and returns an error. Collapsing the
// two is what makes a toolkit retry a typo forever, or give up on a sandbox that was
// merely busy.
func (e *Environment) Exec(ctx context.Context, rs sandbox.RunSpec, sink sandbox.ChunkReporter) (*sandbox.Report, error) {
	started := time.Now()
	rep := &sandbox.Report{
		RunID:     rs.RunID,
		SessionID: rs.SessionID,
		Provider:  ProviderName,
		Isolation: e.provider.Isolation(),
		ExitCode:  0,
	}

	// The box being alive is a fact only the engine can answer. A local map says what
	// we created; `boxlite list --all` says what still exists, which is how an
	// externally-removed box becomes ClassLost instead of a confusing "exec failed".
	states, lerr := e.provider.boxState(ctx)
	if lerr != nil {
		rep.ExitCode = -1
		rep.Duration = time.Since(started)
		rep.Failure = sandbox.AsFailure(lerr)
		return rep, lerr
	}
	status, exists := states[e.id]
	if !exists || (status != "" && !strings.EqualFold(status, "Running")) {
		err := &sandbox.LostError{
			RunID:     rs.RunID,
			SessionID: e.session,
			Provider:  ProviderName,
			Cause:     fmt.Errorf("box %s is %s", e.id, describeStatus(status, exists)),
		}
		rep.ExitCode = -1
		rep.Lost = true
		rep.Duration = time.Since(started)
		rep.Failure = sandbox.AsFailure(err)
		return rep, err
	}

	// The egress allowlist is a per-request posture, so it is applied per request. A
	// refusal here must stop the run: executing anyway would run unrestricted while the
	// caller believes it is restricted.
	if rs.Request.Egress.Mode == sandbox.EgressAllowlist {
		if err := e.provider.applyEgress(ctx, e.id, rs.Request.Egress.Allow); err != nil {
			rep.ExitCode = -1
			rep.Duration = time.Since(started)
			rep.Failure = sandbox.AsFailure(err)
			return rep, err
		}
	}

	argv, err := commandFor(rs.Request)
	if err != nil {
		rep.ExitCode = -1
		rep.Duration = time.Since(started)
		rep.Failure = sandbox.AsFailure(err)
		return rep, err
	}

	args := []string{"exec"}
	payload := payloadFor(rs.Request)
	if payload != "" {
		// --interactive is what attaches stdin. Without it the guest's `sh -s` reads
		// nothing and never sees EOF, so the exec hangs forever and holds the engine's
		// home-directory lock with it. Measured: `exec <box> -- sh -s` hung, `exec -i`
		// printed the payload.
		args = append(args, "--interactive")
	}
	for _, kv := range rs.Request.Env {
		args = append(args, "--env", kv)
	}
	if wd := strings.TrimSpace(rs.Request.Cwd); wd != "" {
		args = append(args, "--workdir", wd)
	}
	// `boxlite exec` takes the box positionally and then `--` before the command;
	// without the separator the command's first word is parsed as a flag value. This is
	// not the same CLI shape as `run`, which rejects `--` outright (measured:
	// "invalid argument: executable '--' not found in $PATH").
	args = append(args, e.id, "--")
	args = append(args, argv...)

	cmd := Command{Args: args}
	// Payload bytes go on stdin, never in the argument list: `ps` on the host would
	// otherwise show the caller's code.
	if payload != "" {
		cmd.Stdin = payload
	}

	// sinkErr is hoisted so a consumer fault recorded mid-run survives to the reporting
	// point after the command returns.
	var sinkErr error
	var seq int64
	if sink != nil {
		cmd.OnChunk = func(st sandbox.Stream, data []byte) {
			if sinkErr != nil {
				return // a faulted consumer is not spammed for the rest of the run
			}
			seq++
			if rerr := sink.Report(sandbox.OutputChunk{
				RunID:    rs.RunID,
				Stream:   st,
				Bytes:    data,
				Sequence: seq,
				At:       time.Now().UTC(),
			}); rerr != nil && !errors.Is(rerr, context.Canceled) {
				sinkErr = rerr
			}
		}
	}

	res, runErr := e.provider.runner.Run(ctx, cmd)
	rep.Duration = time.Since(started)
	rep.OutputSent = res.Streamed
	rep.Stdout, rep.Stderr = res.Stdout, res.Stderr

	if runErr != nil || res.Canceled {
		if ctx.Err() != nil {
			rep.Canceled = true
			rep.ExitCode = 130
			rep.Failure = sandbox.AsFailure(ctx.Err())
			return rep, ctx.Err()
		}
		eherr := e.provider.unavailable("boxlite exec could not be dispatched", runErr,
			"run `boxlite list --all` and `boxlite logs "+e.id+"` to see whether the box survived",
			"check that the engine is responsive: `boxlite info`")
		rep.ExitCode = -1
		rep.Failure = sandbox.AsFailure(eherr)
		return rep, eherr
	}

	if sinkErr != nil {
		// The payload ran; the consumer broke. That is a sink fault, not a user-code
		// failure, and it must not be reported as "your code failed".
		rep.Failure = sandbox.AsFailure(sinkErr)
		return rep, sinkErr
	}

	rep.ExitCode = res.Exit
	if res.Exit != 0 {
		rep.Failure = &sandbox.Failure{
			Class:      sandbox.ClassUserCode,
			NextAction: sandbox.ActionModelFix,
			Cause:      fmt.Errorf("exit code %d", res.Exit),
		}
	}
	return rep, nil
}

func describeStatus(status string, exists bool) string {
	if !exists {
		return "no longer listed by the engine"
	}
	if status == "" {
		return "listed with no status"
	}
	return status
}

// commandFor turns a Request into the guest command line. Code travels on stdin so it
// never appears in the host's process table.
func commandFor(req sandbox.Request) ([]string, error) {
	if len(req.Argv) > 0 {
		return append([]string(nil), req.Argv...), nil
	}
	if req.Runtime == "" {
		return nil, fmt.Errorf("boxlite: a run needs either argv or a runtime with code")
	}
	entry, err := runtimeEntrypoint(req.Runtime)
	if err != nil {
		return nil, err
	}
	if req.Code == "" && len(req.Inputs) == 0 {
		return nil, fmt.Errorf("boxlite: runtime %q needs code or inputs", req.Runtime)
	}
	return entry, nil
}

// runtimeEntrypoint maps the core's interpreter templates onto the guest's own. Each
// entry reads the payload from stdin, matching where commandFor puts it.
func runtimeEntrypoint(runtime string) ([]string, error) {
	switch strings.ToLower(strings.TrimSpace(runtime)) {
	case "shell", "sh", "bash":
		return []string{"sh", "-s"}, nil
	case "python", "python3":
		return []string{"python3", "-"}, nil
	case "node", "javascript", "js":
		return []string{"node", "-"}, nil
	default:
		return nil, fmt.Errorf("boxlite: runtime %q has no interpreter template; pass an explicit argv", runtime)
	}
}

func payloadFor(req sandbox.Request) string {
	if req.Code != "" && req.Runtime != "" {
		return req.Code
	}
	var b strings.Builder
	for _, in := range req.Inputs {
		b.Write(in.Bytes)
	}
	return b.String()
}
