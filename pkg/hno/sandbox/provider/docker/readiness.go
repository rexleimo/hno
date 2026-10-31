package docker

import (
	"context"
	"fmt"
	"strings"

	"github.com/rexleimo/agno-go/pkg/hno/sandbox"
)

// ProbeReady implements sandbox.ReadinessProber.
//
// The core matches ReadyProbe.Pattern on output it already receives, but "this port
// accepts connections" and "this URL answers" are facts about the guest, so a provider
// that wants them has to go ask. Before this the docker provider simply did not answer
// and the core refused with *UnsupportedProbeError, which was the honest behaviour; it is
// now the useful one.
func (p *Provider) ProbeReady(ctx context.Context, env sandbox.Environment, probe sandbox.ReadyProbe) (bool, string, error) {
	e, err := p.environmentOf(env)
	if err != nil {
		return false, "", err
	}
	switch {
	case probe.Port > 0:
		return e.probePort(ctx, probe.Port)
	case probe.URL != "":
		return e.probeURL(ctx, probe.URL)
	default:
		// A pattern-only probe is the core's job. Answering here would be the core's
		// work done twice, possibly differently.
		return false, "", &sandbox.UnsupportedProbeError{
			Provider: p.Name(),
			Kind:     "pattern",
			Fix:      "ReadyProbe.Pattern is evaluated by the core on the run's own output; a provider should not be asked to match it",
		}
	}
}

// environmentOf unwraps the core's closer guard to reach the live container. The core
// wraps an environment it created so cleanup runs once; a provider that checked the
// wrapper's type would reject a container it built itself, so the unwrap loop is the
// provider's own and stops at the first type it recognises.
func (p *Provider) environmentOf(env sandbox.Environment) (*Environment, error) {
	for {
		if w, ok := env.(interface{ Unwrap() sandbox.Environment }); ok {
			if inner := w.Unwrap(); inner != nil && inner != env {
				env = inner
				continue
			}
		}
		break
	}
	e, ok := env.(*Environment)
	if !ok {
		return nil, fmt.Errorf("docker: readiness probe for a foreign environment %q", env.ID())
	}
	return e, nil
}

// probePort reports whether the guest is listening on a TCP port.
//
// Two strategies, tried in order of how well they answer the question:
//
//  1. connect to 127.0.0.1:port from inside the guest. This is the real "accepts a
//     connection" test, and needs bash's /dev/tcp.
//  2. read the kernel's listening table. /proc/net/tcp carries the port as four upper-case
//     hex digits and state 0A means LISTEN, which works on any Linux guest with a shell
//     and no bash.
//
// A guest with neither is reported as not evaluable rather than as ready: a probe that
// cannot look must not become a pass.
func (e *Environment) probePort(ctx context.Context, port int) (bool, string, error) {
	if port < 1 || port > 65535 {
		return false, "", fmt.Errorf("docker: readiness port %d out of range", port)
	}
	hexPort := fmt.Sprintf("%04X", port)

	connect := fmt.Sprintf("bash -c ': </dev/tcp/127.0.0.1/%d' && echo connected", port)
	if out, ok := e.runProbeShell(ctx, connect); ok {
		return true, strings.TrimSpace(out), nil
	}

	listen := fmt.Sprintf(`cat /proc/net/tcp /proc/net/tcp6 2>/dev/null | awk -v p=%s '$2 ~ (":" p "$") && $4 == "0A" {found=1} END {if (found) print "listening"; else exit 1}'`, hexPort)
	out, ok := e.runProbeShell(ctx, listen)
	if ok {
		return true, strings.TrimSpace(out), nil
	}
	// Distinguish "the service is not up yet" from "this guest cannot be asked".
	if probeShellMissing(ctx, e) {
		return false, "", &sandbox.UnsupportedProbeError{
			Provider: e.provider.Name(),
			Kind:     "port",
			Fix:      fmt.Sprintf("image %q has no shell or /proc/net/tcp, so a port probe cannot be evaluated inside it; use ReadyProbe.Pattern instead", e.imageName()),
		}
	}
	return false, "no listener on " + fmt.Sprint(port), nil
}

// runProbeShell executes one probe command in the guest and reports whether it succeeded.
// A non-zero exit is treated as "not ready yet", because the whole point of the probe is
// to be polled while a service starts.
func (e *Environment) runProbeShell(ctx context.Context, script string) (string, bool) {
	res, err := e.provider.runner.Run(ctx, Command{
		Bin:  e.provider.bin,
		Args: []string{"exec", e.id, "sh", "-c", script},
	})
	if err != nil {
		return "", false
	}
	if res.Exit != 0 {
		return "", false
	}
	return res.Stdout, true
}

// probeShellMissing reports whether the guest has no way to be asked at all. One probe
// with a trivially true command answers that: a failure here is about the image, not
// about the service.
func probeShellMissing(ctx context.Context, e *Environment) bool {
	res, err := e.provider.runner.Run(ctx, Command{
		Bin:  e.provider.bin,
		Args: []string{"exec", e.id, "sh", "-c", "test -r /proc/net/tcp"},
	})
	if err != nil {
		return true
	}
	return res.Exit != 0
}

// probeURL reports whether the guest answers an HTTP URL.
//
// The guest needs a client. curl and wget are the two that exist in practice; when
// neither is installed the answer is "cannot evaluate", not "ready", and the caller is
// told to use the port or pattern probe instead.
func (e *Environment) probeURL(ctx context.Context, url string) (bool, string, error) {
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return false, "", fmt.Errorf("docker: readiness URL %q is not http(s)", url)
	}
	quoted := strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `\$`, "`", "\\`").Replace(url)
	script := fmt.Sprintf(`if command -v curl >/dev/null 2>&1; then curl -sS -o /dev/null -w '%%{http_code}' --max-time 5 %q; elif command -v wget >/dev/null 2>&1; then wget -q -O /dev/null %q && echo 200; else echo no-http-client; exit 97; fi`, quoted, quoted)
	res, err := e.provider.runner.Run(ctx, Command{
		Bin:  e.provider.bin,
		Args: []string{"exec", e.id, "sh", "-c", script},
	})
	if err != nil {
		return false, "", err
	}
	observed := strings.TrimSpace(res.Stdout + " " + res.Stderr)
	switch {
	case res.Exit == 97 || strings.Contains(observed, "no-http-client"):
		return false, "", &sandbox.UnsupportedProbeError{
			Provider: e.provider.Name(),
			Kind:     "url",
			Fix:      fmt.Sprintf("image %q has no curl or wget, so a URL probe cannot be evaluated inside it; use ReadyProbe.Port or ReadyProbe.Pattern", e.imageName()),
		}
	case res.Exit == 0 && strings.HasSuffix(observed, "200"):
		return true, observed, nil
	case res.Exit == 0:
		// A client that ran but got a non-2xx answer is "not ready yet": the design
		// promise is 2xx, and 503 while a service warms up is the normal case.
		return false, observed, nil
	default:
		return false, observed, nil
	}
}

var _ sandbox.ReadinessProber = (*Provider)(nil)

// imageName reports the image the environment was created from, for probe diagnostics.
func (e *Environment) imageName() string {
	if base := e.Spec().Base; base != "" {
		return base
	}
	return "unknown image"
}
