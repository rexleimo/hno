package run

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/rexleimo/agno-go/pkg/hno/tools/toolkit"
)

// maxToolkitTimeout caps timeout_seconds from model-supplied arguments so a
// hostile or mistaken payload cannot stretch a billable E2B sandbox TTL.
const maxToolkitTimeout = 5 * time.Minute

// CodeExecutionToolkit exposes a fail-closed run_code tool to agents. It
// receives an already-configured Executor so provider selection and Cloud
// billing remain an application-owned decision.
type CodeExecutionToolkit struct {
	*toolkit.BaseToolkit
	executor    Executor
	executorErr error
}

// NewToolkit creates a code execution toolkit. A nil executor is retained as
// a configuration error so every run_code call fails closed rather than
// falling back to a host process.
func NewToolkit(executor Executor) *CodeExecutionToolkit {
	t := &CodeExecutionToolkit{BaseToolkit: toolkit.NewBaseToolkit("code_execution")}
	if executor == nil {
		t.executorErr = fmt.Errorf("sandbox executor cannot be nil")
	} else {
		t.executor = executor
	}
	t.registerFunctions()
	return t
}

// NewToolkitWithConfig creates a toolkit from explicit provider configuration.
func NewToolkitWithConfig(config Config) (*CodeExecutionToolkit, error) {
	executor, err := NewExecutor(config)
	if err != nil {
		return nil, err
	}
	return NewToolkit(executor), nil
}

// Close closes the configured executor.
func (t *CodeExecutionToolkit) Close() error {
	if t.executor == nil {
		return nil
	}
	return t.executor.Close()
}

func (t *CodeExecutionToolkit) registerFunctions() {
	t.RegisterFunction(&toolkit.Function{
		Name:        "run_code",
		Description: "Run code in a configured disposable sandbox. Supported runtimes are python, node, and shell. The executor fails closed when no sandbox provider is configured.",
		Parameters: map[string]toolkit.Parameter{
			"runtime": {
				Type:        "string",
				Description: "Runtime template to use",
				Required:    true,
				Enum:        []string{"python", "node", "shell"},
			},
			"code": {
				Type:        "string",
				Description: "Code or shell script to execute",
				Required:    true,
			},
			"timeout_seconds": {
				Type:        "number",
				Description: "Hard execution deadline in seconds (default: 30)",
				Required:    false,
			},
		},
		Handler: t.runCode,
	})
}

func (t *CodeExecutionToolkit) runCode(ctx context.Context, args map[string]interface{}) (interface{}, error) {
	if t.executorErr != nil {
		return nil, fmt.Errorf("code execution sandbox configuration: %w", t.executorErr)
	}
	runtime, ok := args["runtime"].(string)
	if !ok {
		return nil, fmt.Errorf("runtime must be a string")
	}
	code, ok := args["code"].(string)
	if !ok {
		return nil, fmt.Errorf("code must be a string")
	}

	spec := Spec{Runtime: runtime, Code: code}
	if value, exists := args["timeout_seconds"]; exists {
		seconds, ok := value.(float64)
		if !ok || seconds <= 0 {
			return nil, fmt.Errorf("timeout_seconds must be a positive number")
		}
		spec.Timeout = time.Duration(seconds * float64(time.Second))
		if spec.Timeout > maxToolkitTimeout {
			spec.Timeout = maxToolkitTimeout
		}
	}

	result, err := t.executor.Run(ctx, spec)
	payload := map[string]interface{}{
		"exit_code": result.ExitCode,
		"stdout":    result.Stdout,
		"stderr":    result.Stderr,
		"timed_out": result.TimedOut,
	}
	if err == nil {
		return payload, nil
	}
	// The error text stays in the map because callers already read it there.
	payload["error"] = err.Error()
	if isEnvironmentFailure(err, result) {
		// Double write: the map for callers that read tool output, a real error for the
		// agent's logger, its per-call outcome, and anything watching failures. Returning
		// nil here recorded an unusable sandbox as a successful tool call everywhere except
		// in the text the model was shown.
		payload["failure"] = "the execution environment failed; the code did not report an exit status"
		return payload, fmt.Errorf("run_code: %w", err)
	}
	return payload, nil
}

// isEnvironmentFailure separates "the sandbox is broken" from "my code is wrong". That
// separation is the whole point of returning an error at all: without it, a payload that
// exited non-zero would be reported as an outage, which sends the model retrying working
// code against a healthy engine.
//
// The evidence available at this layer is what the executors actually produce:
//   - ErrTimeout arrives with TimedOut set, because the payload ran past its own budget.
//   - podman and local derive ExitCode from *exec.ExitError and use -1 when the failure was
//     not an exit status, and e2b initialises ExitCode to -1 until the service reports one.
//     A positive exit code therefore means the payload really ran.
//   - Sentinels for a missing engine, an unknown backend, or a refusal from the sandbox
//     adapter describe a configuration that cannot run anything at all.
//
// Known imprecision, kept deliberate rather than silent: podman reports an output-capture
// fault alongside the exit code it did observe, and this rule files that as payload data. The
// full error text is still in the map; only the typed signal is missing, and only for that
// combination.
func isEnvironmentFailure(err error, result Result) bool {
	switch {
	case errors.Is(err, ErrNoProvider),
		errors.Is(err, ErrUnsupportedBackend),
		errors.Is(err, ErrSandboxUnsupported),
		errors.Is(err, ErrSandboxLimitMismatch):
		return true
	case result.TimedOut:
		return false
	case result.ExitCode > 0:
		return false
	default:
		return true
	}
}
