// Command hno is the operator-facing entry point for the HNO sandbox runtime.
//
// It exists because the runtime already had something to say to a human — Diagnose
// reports want, got and a fix; the provider reports what it declined to reclaim —
// and until now the only way to hear any of it was to write a Go program. Remediation
// text in the runtime pointed at "hno sandbox doctor" and "hno sandbox prune" long
// before this command existed, so the text was a promise nobody kept.
//
// The CLI adds no policy of its own: every verb is a thin shell over one exported
// runtime call, and it reports the distinction the runtime makes between "the program
// I ran failed" and "the environment could not run anything".
package main

import (
	"fmt"
	"io"
	"os"
)

// Exit codes. A payload's own status is passed through by `run`, so these are chosen
// to sit outside the range ordinary programs return.
const (
	exitOK         = 0
	exitDiagFailed = 1   // doctor: a check failed
	exitUsage      = 2   // the command line itself is wrong
	exitTimedOut   = 124 // the payload ran and exceeded its budget (timeout(1))
	exitEnvFailure = 70  // the environment could not be used at all (EX_IOERR)
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run dispatches on the command group. Tests call it directly with their own writers,
// so nothing in this binary has to be exercised by spawning a process.
func run(argv []string, stdout, stderr io.Writer) int {
	if len(argv) == 0 {
		usage(stdout)
		return exitOK
	}
	switch argv[0] {
	case "sandbox":
		return sandboxMain(argv[1:], stdout, stderr)
	case "help", "-h", "--help":
		usage(stdout)
		return exitOK
	case "version":
		fmt.Fprintln(stdout, "hno sandbox runtime CLI")
		return exitOK
	default:
		fmt.Fprintf(stderr, "hno: unknown command %q\n\n", argv[0])
		usage(stderr)
		return exitUsage
	}
}

func usage(w io.Writer) {
	fmt.Fprint(w, `hno - the HNO sandbox runtime

Usage:
  hno sandbox doctor   Why can't this run here? Checks, limits, capabilities, fixes.
  hno sandbox prune    Show (and with --yes, reclaim) environments this host owns.
  hno sandbox run      Run one command inside a sandbox and pass its status through.
  hno help             This text.

Every sandbox flag mirrors a runtime option; "hno sandbox <verb> -h" lists them with
the option they set, so the CLI never asks you to guess a name that does not exist.

Exit codes from "hno sandbox run":
  the program's own status   passed through unchanged
  124                        the program ran and hit its timeout
  70                         the sandbox could not run it; the status is unknown
  2                          the command line is wrong
`)
}
