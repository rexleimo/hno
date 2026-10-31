package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/rexleimo/agno-go/pkg/hno/sandbox"
	"github.com/rexleimo/agno-go/pkg/hno/sandbox/provider/docker"
)

// The verbs this CLI provides. Remediation text inside the runtime is allowed to name
// one of these and nothing else: pkg/hno/sandbox/dx_gate_test.go parses this file, so a
// hint that drifts from the shipped command fails a test instead of sending an operator
// after a subcommand that was never written.
const (
	verbDoctor   = "doctor"
	verbPrune    = "prune"
	verbRun      = "run"
	verbBoundary = "boundary"
)

// sandboxVerbs is the one list the dispatcher, the usage text, and the unknown-verb
// message all read. It used to be three hand-copied strings in three places, so adding a
// verb meant the help text and the error message could silently disagree with what the
// command actually accepts.
var sandboxVerbs = []string{verbDoctor, verbPrune, verbRun, verbBoundary}

// defaultImage is what the runtime's own examples use, not a requirement of the engine.
const defaultImage = "golang:1.26"

// sandboxMain dispatches "hno sandbox <verb>".
func sandboxMain(argv []string, stdout, stderr io.Writer) int {
	if len(argv) == 0 {
		fmt.Fprint(stderr, `Usage:
  hno sandbox doctor   Why can't this run here?
  hno sandbox prune    Show or reclaim owned environments.
  hno sandbox run      Run one command inside a sandbox.
  hno sandbox boundary Is the isolation a measurement or a name?
`)
		return exitUsage
	}
	verb := argv[0]
	if !slices.Contains(sandboxVerbs, verb) {
		fmt.Fprintf(stderr, "hno sandbox: unknown verb %q (have %s)\n", verb, strings.Join(sandboxVerbs, ", "))
		return exitUsage
	}

	fs := flag.NewFlagSet("hno sandbox "+verb, flag.ContinueOnError)
	fs.SetOutput(stderr)
	cfg := commonFlags(fs)
	shell := fs.String("shell", "", "run this script through the guest shell instead of an argv (run)")
	session := fs.String("session", "", "reuse this session id, creating it if absent (run)")
	timeout := fs.Duration("timeout", 0, "per-run execution budget, e.g. 30s; 0 keeps the runtime default (run)")
	yes := fs.Bool("yes", false, "actually delete, rather than only report (prune)")
	asJSON := fs.Bool("json", false, "machine-readable output (doctor, boundary)")
	boxes := fs.Int("boxes", 0, fmt.Sprintf("how many boxes the boundary measurement starts (%d-%d); 0 uses the runtime default %d (boundary)", 2, sandbox.MaxBoundaryBoxes, sandbox.DefaultBoundaryBoxes))
	expect := fs.String("expect", "", "fail unless the measured boundary is this: shared-kernel or private-kernel (boundary)")
	fs.Usage = func() { usageSandbox(fs, stderr, verb) }
	if err := fs.Parse(argv[1:]); err != nil {
		return exitUsage
	}

	ctx := context.Background()
	switch verb {
	case verbDoctor:
		return doctorCmd(ctx, cfg, *asJSON, stdout, stderr)
	case verbPrune:
		return pruneCmd(ctx, cfg, *yes, stdout, stderr)
	case verbRun:
		return runCmd(ctx, cfg, *session, *shell, *timeout, fs.Args(), stdout, stderr)
	case verbBoundary:
		return boundaryCmd(ctx, cfg, *boxes, *expect, *asJSON, stdout, stderr)
	}
	return exitUsage
}

func usageSandbox(fs *flag.FlagSet, w io.Writer, verb string) {
	fmt.Fprintf(w, "hno sandbox %s\n\nFlags (each mirrors a runtime option, so the name here is the name there):\n", verb)
	fs.PrintDefaults()
}

// sandboxConfig is the flag set shared by every verb, kept as a value so tests can build
// one without touching the process command line.
type sandboxConfig struct {
	provider  string
	image     string
	isolation string
	runtime   string
	attrs     attrFlags
}

func commonFlags(fs *flag.FlagSet) *sandboxConfig {
	cfg := &sandboxConfig{}
	fs.StringVar(&cfg.provider, "provider", "docker", "registered provider name")
	fs.StringVar(&cfg.image, "image", defaultImage, "image to bind the environment to (doctor, run)")
	fs.StringVar(&cfg.isolation, "isolation", "", "minimum isolation: none, container, syscall-filtered, microvm, microvm-hosted (the names Isolation.String renders)")
	fs.StringVar(&cfg.runtime, "runtime", "", "engine runtime to create under, e.g. runsc (docker only)")
	fs.Var(&cfg.attrs, "attr", "engine attribute k=v, repeatable (maps to sandbox.WithAttrs)")
	return cfg
}

// attrFlags collects repeated --attr k=v pairs into the engine-config channel.
type attrFlags map[string]any

func (a *attrFlags) String() string {
	if a == nil || len(*a) == 0 {
		return ""
	}
	keys := make([]string, 0, len(*a))
	for k := range *a {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(' ')
		}
		fmt.Fprintf(&b, "%s=%v", k, (*a)[k])
	}
	return b.String()
}

func (a *attrFlags) Set(raw string) error {
	key, val, ok := strings.Cut(raw, "=")
	if !ok {
		return fmt.Errorf("--attr wants k=v, got %q", raw)
	}
	if strings.TrimSpace(key) == "" {
		return fmt.Errorf("--attr got an empty key in %q", raw)
	}
	if *a == nil {
		*a = attrFlags{}
	}
	(*a)[key] = val
	return nil
}

// options turns the flags into runtime options. Unknown isolation is refused here rather
// than silently dropped, because an operator who asks for micro-vm and quietly gets
// container is the failure mode the isolation floor exists to prevent.
func (c *sandboxConfig) options() ([]sandbox.Option, error) {
	opts := []sandbox.Option{sandbox.WithProvider(c.provider)}
	if c.image != "" {
		opts = append(opts, sandbox.WithImage(c.image))
	}
	if c.isolation != "" {
		iso, err := sandbox.ParseIsolation(c.isolation)
		if err != nil {
			return nil, fmt.Errorf("--isolation: %w", err)
		}
		opts = append(opts, sandbox.WithMinimumIsolation(iso))
	}
	if len(c.attrs) > 0 {
		opts = append(opts, sandbox.WithAttrs(map[string]any(c.attrs)))
	}
	return opts, nil
}

// openHandle registers the providers this binary ships and opens a handle. Registration is
// explicit — nothing scans PATH at init time (D-03) — so a missing provider is a typed
// error the runtime already knows how to explain.
func openHandle(ctx context.Context, opts ...sandbox.Option) (*sandbox.Sandbox, error) {
	if err := docker.Register(); err != nil {
		return nil, fmt.Errorf("register docker provider: %w", err)
	}
	return sandbox.Open(ctx, opts...)
}

// boundaryCmd answers the question the isolation ladder has always assumed: how much
// kernel do two boxes actually share. It is not a health check and it is not free -- it
// provisions real boxes -- so it is a verb the operator chooses rather than something the
// runtime does behind a doctor run.
//
// Exit codes follow the same split as "run", because the two failures mean different
// things: 1 means the measurement completed and you did not like the answer, 70 means no
// answer exists. A script that treats "could not measure" as "isolation is fine" is the
// false green light D-36 was about.
func boundaryCmd(ctx context.Context, cfg *sandboxConfig, boxes int, expect string, asJSON bool, stdout, stderr io.Writer) int {
	opts, err := cfg.options()
	if err != nil {
		fmt.Fprintf(stderr, "hno sandbox boundary: %v\n", err)
		return exitUsage
	}
	want := sandbox.BoundaryUnmeasured
	if expect != "" {
		parsed, perr := sandbox.ParseKernelBoundary(expect)
		if perr != nil {
			fmt.Fprintf(stderr, "hno sandbox boundary: %v\n", perr)
			return exitUsage
		}
		want = parsed
	}
	sb, err := openHandle(ctx, opts...)
	if err != nil {
		fmt.Fprintf(stderr, "hno sandbox boundary: cannot open a handle: %v\n", err)
		return exitEnvFailure
	}
	defer sb.Close()

	report, err := sb.MeasureKernelBoundary(ctx, boxes)
	if err != nil {
		var budget *sandbox.BoundaryBudgetError
		if errors.As(err, &budget) {
			fmt.Fprintf(stderr, "hno sandbox boundary: %v\n%s\n", err, budget.Remediation())
			return exitUsage
		}
		var unsupported *sandbox.BoundaryUnsupportedError
		if errors.As(err, &unsupported) {
			// 70, not 2: nothing about the command line was wrong, the engine simply
			// cannot answer this question. Calling that a usage error would tell the
			// operator to fix flags that are already correct.
			fmt.Fprintf(stderr, "hno sandbox boundary: %v\n%s\n", err, unsupported.Remediation())
			return exitEnvFailure
		}
		fmt.Fprintf(stderr, "hno sandbox boundary: the measurement could not run: %v\n", err)
		return exitEnvFailure
	}

	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(report); err != nil {
			fmt.Fprintf(stderr, "hno sandbox boundary: %v\n", err)
			return exitEnvFailure
		}
	} else {
		fmt.Fprintf(stdout, "kernel boundary: %s\n", report.Verdict)
		fmt.Fprintf(stdout, "  boxes: %d, distinct kernel readings: %d\n", report.Boxes, report.Distinct())
		if report.GuestKernel != "" {
			fmt.Fprintf(stdout, "  guest kernel: %s (host: %s)\n", report.GuestKernel, report.HostKernel)
		}
		for _, e := range report.Evidence {
			fmt.Fprintf(stdout, "  - %s\n", e)
		}
		for _, f := range report.Fixes {
			fmt.Fprintf(stdout, "  fix: %s\n", f)
		}
	}

	if report.Verdict == sandbox.BoundaryUnknown {
		fmt.Fprintln(stderr, "hno sandbox boundary: the engine gave no readings worth believing; no isolation tier may be inferred from that")
		return exitDiagFailed
	}
	if expect != "" && report.Verdict != want {
		fmt.Fprintf(stderr, "hno sandbox boundary: measured %s, expected %s\n", report.Verdict, want)
		return exitDiagFailed
	}
	return exitOK
}

func doctorCmd(ctx context.Context, cfg *sandboxConfig, asJSON bool, stdout, stderr io.Writer) int {
	opts, err := cfg.options()
	if err != nil {
		fmt.Fprintf(stderr, "hno sandbox doctor: %v\n", err)
		return exitUsage
	}
	sb, err := openHandle(ctx, opts...)
	if err != nil {
		fmt.Fprintf(stderr, "hno sandbox doctor: cannot open a handle: %v\n", err)
		return exitEnvFailure
	}
	defer sb.Close()

	diag, err := sb.Diagnose(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "hno sandbox doctor: diagnose failed: %v\n", err)
		return exitEnvFailure
	}
	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(diag); err != nil {
			fmt.Fprintf(stderr, "hno sandbox doctor: %v\n", err)
			return exitEnvFailure
		}
	} else {
		printDiag(stdout, diag)
	}
	if allChecksOK(diag) {
		return exitOK
	}
	return exitDiagFailed
}

// allChecksOK is computed here rather than asked of the runtime: DiagReport is a struct of
// fields and has no OK() method, and inventing one in a guide would be exactly the defect
// this CLI's tests exist to catch.
func allChecksOK(diag *sandbox.DiagReport) bool {
	if diag == nil || len(diag.Checks) == 0 {
		return false
	}
	for _, c := range diag.Checks {
		if !c.OK {
			return false
		}
	}
	return true
}

func printDiag(w io.Writer, diag *sandbox.DiagReport) {
	fmt.Fprintf(w, "reached isolation: %v\n", diag.Reached)
	for _, c := range diag.Checks {
		mark := "FAIL"
		if c.OK {
			mark = "ok  "
		}
		fmt.Fprintf(w, "%s %-22s want=%s got=%s\n", mark, c.Env, trunc(c.Want, 40), trunc(c.Got, 60))
		if !c.OK && c.Fix != "" {
			fmt.Fprintf(w, "       fix: %s\n", c.Fix)
		}
	}
	for _, f := range diag.Fixes {
		fmt.Fprintf(w, "fix: %s\n", f)
	}
	fmt.Fprintf(w, "%s\n", diag.Summary)
}

func trunc(s string, n int) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// pruneCmd reports what this host owns and deletes only on an explicit --yes. The default
// is read-only because a reclaim command that guesses is worse than one that asks.
func pruneCmd(ctx context.Context, cfg *sandboxConfig, yes bool, stdout, stderr io.Writer) int {
	opts, err := cfg.options()
	if err != nil {
		fmt.Fprintf(stderr, "hno sandbox prune: %v\n", err)
		return exitUsage
	}
	sb, err := openHandle(ctx, opts...)
	if err != nil {
		fmt.Fprintf(stderr, "hno sandbox prune: cannot open a handle: %v\n", err)
		return exitEnvFailure
	}
	defer sb.Close()

	rec, hasReclaimer := sb.Reclaimer()
	if !hasReclaimer {
		// A provider that cannot reclaim has no orphans to leave behind, so the verb's
		// goal is met by construction — but that has to be said out loud, not printed as
		// "0 environments" and left to look like a clean result.
		fmt.Fprintf(stdout, "provider %q does not reclaim environments; nothing to delete\n", cfg.provider)
		return exitOK
	}

	left, err := rec.Sweep(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "hno sandbox prune: sweep failed: %v\n", err)
		return exitEnvFailure
	}
	fmt.Fprintf(stdout, "%d environment(s) left behind by this host\n", len(left))
	for _, name := range left {
		fmt.Fprintf(stdout, "  - %s\n", name)
	}
	if !yes {
		if len(left) > 0 {
			fmt.Fprintln(stdout, "read-only: pass --yes to delete them")
		}
		reportRefusals(stdout, rec)
		return exitOK
	}
	removed, err := rec.Prune(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "hno sandbox prune: prune failed: %v\n", err)
		return exitEnvFailure
	}
	fmt.Fprintf(stdout, "reclaimed %d environment(s)\n", len(removed))
	reportRefusals(stdout, rec)
	return exitOK
}

// reportRefusals surfaces what the provider declined to touch. A count of zero deleted
// environments is not the same fact as "these belong to somebody else and I will not
// delete them", and conflating them is how a reclaim tool hides the interesting half.
// The reclaimer the handle hands back is the provider itself, which is where the
// refusal list lives.
func reportRefusals(w io.Writer, rec sandbox.Reclaimer) {
	rp, ok := rec.(interface{ SkippedPrune() []string })
	if !ok {
		return
	}
	skipped := rp.SkippedPrune()
	if len(skipped) == 0 {
		return
	}
	fmt.Fprintf(w, "declined to delete %d:\n", len(skipped))
	for _, s := range skipped {
		fmt.Fprintf(w, "  - %s\n", s)
	}
}

// runCmd executes one request and keeps the two failure kinds apart: the program's own
// status is passed through, an unusable environment is not reportable as a failing program.
func runCmd(ctx context.Context, cfg *sandboxConfig, session, shell string, timeout time.Duration, argv []string, stdout, stderr io.Writer) int {
	opts, err := cfg.options()
	if err != nil {
		fmt.Fprintf(stderr, "hno sandbox run: %v\n", err)
		return exitUsage
	}
	req, err := buildRequest(shell, argv)
	if err != nil {
		fmt.Fprintf(stderr, "hno sandbox run: %v\n", err)
		return exitUsage
	}
	var runOpts []sandbox.RunOption
	if timeout > 0 {
		runOpts = append(runOpts, sandbox.WithTimeout(timeout))
	}

	sb, err := openHandle(ctx, opts...)
	if err != nil {
		fmt.Fprintf(stderr, "hno: cannot open a sandbox: %v\n", err)
		return exitEnvFailure
	}
	defer sb.Close()

	var report *sandbox.Report
	if session != "" {
		sess, serr := sb.Session(ctx, session, sandbox.WithCreateIfMissing(true))
		if serr != nil {
			fmt.Fprintf(stderr, "hno: session %q is unusable: %v\n", session, serr)
			return exitEnvFailure
		}
		report, err = sess.Exec(ctx, req, runOpts...)
	} else {
		report, err = sb.Run(ctx, req, runOpts...)
	}

	if report != nil {
		if report.Stdout != "" {
			fmt.Fprint(stdout, report.Stdout)
		}
		if report.Stderr != "" {
			fmt.Fprint(stderr, report.Stderr)
		}
		if report.Truncated {
			fmt.Fprintln(stderr, "hno: output was truncated at the configured ceiling")
		}
	}
	if report != nil && report.TimedOut {
		// A payload that ran out of budget is the program's story, not the environment's:
		// the box executed it and stopped it on time. timeout(1) uses 124 for exactly this,
		// so a script wrapping this CLI can tell "too slow" from "crashed" from "broken
		// sandbox" without parsing stderr (same split D-33 makes for run_code).
		fmt.Fprintln(stderr, "hno: the run hit its timeout")
		return exitTimedOut
	}
	if err != nil && !payloadExplains(err, report) {
		fmt.Fprintf(stderr, "hno: the sandbox could not run this, so the program's status is unknown: %v\n", err)
		var pending *sandbox.ApprovalPendingError
		if errors.As(err, &pending) {
			fmt.Fprintf(stderr, "hno: awaiting human approval (%s): %s\n", pending.ApprovalID, pending.Reason)
		}
		return exitEnvFailure
	}
	if report == nil {
		fmt.Fprintln(stderr, "hno: no report was produced")
		return exitEnvFailure
	}
	// A payload's own exit status is its own: pass it through unchanged.
	code := report.ExitCode
	if code < 0 {
		// No exit code was observed. Reporting 0 here would claim success the runtime
		// cannot prove (D-33's classification, applied to a process exit status).
		fmt.Fprintln(stderr, "hno: the program never reported an exit status")
		return exitEnvFailure
	}
	return code
}

// payloadExplains reports whether the returned error is the payload failing rather than
// the environment failing. Only an observed non-zero exit counts as observed: a nil
// report, or an exit code the runtime could not read, is the environment's story.
func payloadExplains(err error, report *sandbox.Report) bool {
	return report != nil && report.ExitCode > 0
}

func buildRequest(shell string, argv []string) (sandbox.Request, error) {
	if strings.TrimSpace(shell) != "" && len(argv) > 0 {
		return sandbox.Request{}, errors.New("pass either --shell or a command after --, not both")
	}
	if strings.TrimSpace(shell) != "" {
		return sandbox.Shell(shell), nil
	}
	if len(argv) == 0 {
		return sandbox.Request{}, errors.New("nothing to run: pass --shell \"...\" or -- <command> [args...]")
	}
	return sandbox.Command(argv...), nil
}
