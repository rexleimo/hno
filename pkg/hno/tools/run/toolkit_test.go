package run

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

type recordingExecutor struct {
	spec     Spec
	result   Result
	err      error
	closeErr error
}

func (e *recordingExecutor) Run(_ context.Context, spec Spec) (Result, error) {
	e.spec = spec
	return e.result, e.err
}

func (e *recordingExecutor) Close() error { return e.closeErr }

func TestCodeExecutionToolkitRunCode(t *testing.T) {
	executor := &recordingExecutor{result: Result{ExitCode: 0, Stdout: "ok"}}
	toolkit := NewToolkit(executor)

	result, err := toolkit.Execute(context.Background(), "run_code", map[string]interface{}{
		"runtime":         "python",
		"code":            "print('ok')",
		"timeout_seconds": 2.5,
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	output := result.(map[string]interface{})
	if output["stdout"] != "ok" || output["exit_code"] != 0 {
		t.Fatalf("Execute() output = %#v", output)
	}
	if executor.spec.Runtime != "python" || executor.spec.Code != "print('ok')" || executor.spec.Timeout != 2500*time.Millisecond {
		t.Fatalf("executor spec = %#v", executor.spec)
	}
}

func TestCodeExecutionToolkitReturnsExecutionOutputOnFailure(t *testing.T) {
	executor := &recordingExecutor{
		result: Result{ExitCode: 4, Stdout: "partial", Stderr: "failed"},
		err:    errors.New("process exited with status 4"),
	}
	toolkit := NewToolkit(executor)

	result, err := toolkit.Execute(context.Background(), "run_code", map[string]interface{}{
		"runtime": "python",
		"code":    "raise SystemExit(4)",
	})
	if err != nil {
		t.Fatalf("Execute() error = %v, want structured tool result", err)
	}
	output := result.(map[string]interface{})
	if output["exit_code"] != 4 || output["stdout"] != "partial" || output["error"] == "" {
		t.Fatalf("Execute() output = %#v", output)
	}
}

func TestCodeExecutionToolkitFailsClosedWithNilExecutor(t *testing.T) {
	toolkit := NewToolkit(nil)
	_, err := toolkit.Execute(context.Background(), "run_code", map[string]interface{}{
		"runtime": "python",
		"code":    "print(1)",
	})
	if err == nil {
		t.Fatal("Execute() error = nil, want configuration error")
	}
}

func TestCodeExecutionToolkitCapsTimeout(t *testing.T) {
	executor := &recordingExecutor{result: Result{ExitCode: 0}}
	toolkit := NewToolkit(executor)

	_, err := toolkit.Execute(context.Background(), "run_code", map[string]interface{}{
		"runtime":         "python",
		"code":            "print(1)",
		"timeout_seconds": 100000.0,
	})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if executor.spec.Timeout != maxToolkitTimeout {
		t.Fatalf("spec.Timeout = %v, want capped %v", executor.spec.Timeout, maxToolkitTimeout)
	}
}

func TestCodeExecutionToolkitRejectsInvalidTimeout(t *testing.T) {
	toolkit := NewToolkit(&recordingExecutor{})
	_, err := toolkit.Execute(context.Background(), "run_code", map[string]interface{}{
		"runtime":         "python",
		"code":            "print(1)",
		"timeout_seconds": 0.0,
	})
	if err == nil {
		t.Fatal("Execute() error = nil, want timeout validation error")
	}
}

func TestCodeExecutionToolkitClose(t *testing.T) {
	want := errors.New("close failed")
	toolkit := NewToolkit(&recordingExecutor{closeErr: want})
	if err := toolkit.Close(); !errors.Is(err, want) {
		t.Fatalf("Close() error = %v, want %v", err, want)
	}
}

// The failure-visibility migration: an environment failure used to be folded into the result
// map with a nil error, so the agent's logger, its per-call outcome, and any policy watching
// for failures all saw a successful tool call. Only the model read the text, and it could not
// tell "the sandbox is broken" from "my code is wrong" -- which is why it retried the same
// payload. The map is kept for callers that read it, and the error is returned as well.

// infraExecutor returns whatever it is told, standing in for any of the three executors.
type failingExecutor struct {
	result Result
	err    error
}

func (e failingExecutor) Run(_ context.Context, _ Spec) (Result, error) { return e.result, e.err }
func (e failingExecutor) Close() error                                  { return nil }

func runCodeWith(t *testing.T, executor Executor, args map[string]interface{}) (map[string]interface{}, error) {
	t.Helper()
	tk := NewToolkit(executor)
	out, err := tk.Execute(context.Background(), "run_code", args)
	if out == nil {
		return nil, err
	}
	m, ok := out.(map[string]interface{})
	if !ok {
		t.Fatalf("toolkit returned %T, want the result map", out)
	}
	return m, err
}

var codeArgs = map[string]interface{}{"runtime": "shell", "code": "exit 3"}

// TestEnvironmentFailureIsDoubleWritten is the falsifier: the previous code returned a nil
// error here, which is what made an unavailable engine indistinguishable from a run.
func TestEnvironmentFailureIsDoubleWritten(t *testing.T) {
	m, err := runCodeWith(t, failingExecutor{
		result: Result{ExitCode: -1},
		err:    fmt.Errorf("%w: docker not found", ErrNoProvider),
	}, codeArgs)
	if err == nil {
		t.Fatal("an environment failure must reach the caller as an error, not only as map text")
	}
	if !errors.Is(err, ErrNoProvider) {
		t.Fatalf("the error must stay inspectable, got %T: %v", err, err)
	}
	// And the map must still be there, because existing callers read it.
	if m == nil || m["exit_code"] == nil {
		t.Fatalf("the payload map must survive the double write, got %#v", m)
	}
	if m["failure"] == nil {
		t.Fatalf("the map should name the failure class for callers that only read the map, got %#v", m)
	}
}

// TestUserCodeFailureStaysData is the other half, and the reason a naive "just return err"
// would be wrong: legacy executors also return an error when the payload exits non-zero.
// Turning that into a tool error would tell the model its script was an infrastructure outage.
func TestUserCodeFailureStaysData(t *testing.T) {
	m, err := runCodeWith(t, failingExecutor{
		result: Result{ExitCode: 4, Stderr: "SyntaxError"},
		err:    fmt.Errorf("sandbox run failed: exit status 4"),
	}, codeArgs)
	if err != nil {
		t.Fatalf("a non-zero exit is data about the payload, got error %v", err)
	}
	if m["exit_code"] != 4 || m["stderr"] != "SyntaxError" {
		t.Fatalf("want the payload result, got %#v", m)
	}
	if _, ok := m["failure"]; ok {
		t.Fatalf("a payload outcome must not be labelled an environment failure, got %#v", m)
	}
}

// TestTimeoutStaysData: the deadline is the caller's own budget being exceeded by the payload,
// and timed_out already says so in the map.
func TestTimeoutStaysData(t *testing.T) {
	m, err := runCodeWith(t, failingExecutor{
		result: Result{TimedOut: true},
		err:    fmt.Errorf("%w after 30s", ErrTimeout),
	}, codeArgs)
	if err != nil {
		t.Fatalf("a timeout must stay a payload result, got %v", err)
	}
	if m["timed_out"] != true {
		t.Fatalf("want timed_out surfaced, got %#v", m)
	}
}

// TestNoObservedExitIsAnEnvironmentFailure covers the -1/0-with-error shape every legacy
// executor produces when nothing reported an exit code.
func TestNoObservedExitIsAnEnvironmentFailure(t *testing.T) {
	for _, code := range []int{-1, 0} {
		_, err := runCodeWith(t, failingExecutor{
			result: Result{ExitCode: code},
			err:    errors.New("e2b sandbox response missing secure sandbox credentials"),
		}, codeArgs)
		if err == nil {
			t.Fatalf("exit code %d with an error and no observed run must not read as success", code)
		}
	}
}

// TestAdapterRefusalsAreEnvironmentFailures: the sandbox adapter refuses unsupported shapes
// before any engine runs, and that refusal is a configuration fault, not payload output.
func TestAdapterRefusalsAreEnvironmentFailures(t *testing.T) {
	_, err := runCodeWith(t, failingExecutor{
		err: fmt.Errorf("%w: backend podman", ErrUnsupportedBackend),
	}, codeArgs)
	if !errors.Is(err, ErrUnsupportedBackend) {
		t.Fatalf("want the sentinel preserved for callers, got %v", err)
	}
}
