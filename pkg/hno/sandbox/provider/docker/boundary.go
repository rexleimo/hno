package docker

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/rexleimo/agno-go/pkg/hno/sandbox"
)

// boundaryProbe is the guest-side reading. It is one command with three lines
// because every extra round trip is another exec into a box that costs a real boot:
//
//  1. /proc/sys/kernel/random/boot_id, which is kernel-global and not namespaced, so
//     two boxes showing the same value are on one kernel and two showing different
//     values are not;
//  2. seconds since that kernel booted, which corroborates the first independently —
//     a shared kernel cannot age differently in two places;
//  3. uname -sr, so the report shows what kernel the guest is actually running
//     against the host's, which is the difference between "a VM" and "a namespace".
const boundaryProbe = `cat /proc/sys/kernel/random/boot_id 2>/dev/null; cut -d' ' -f1 /proc/uptime 2>/dev/null; uname -sr 2>/dev/null`

// MeasureKernelBoundary answers how much kernel the engine shares between boxes,
// by starting boxes and reading their own kernels rather than by asserting what a
// provider of this name is supposed to do.
//
// The reason it exists is measured, not theoretical: on the machine this package
// was developed on, the `docker` binary turned out to be a compatibility shim over
// the Apple containerisation engine (docker info reports "Apple Containerization"),
// and every box it started had its own kernel — four boxes, four distinct boot ids
// and four distinct uptimes. A provider named "docker" returning
// IsolationContainer from a name lookup would have been wrong about its own engine,
// and the wrongness would have been invisible because it looked like a conservative
// answer.
//
// Everything goes through the same create/start/exec/close path a real run uses. A
// second, lighter engine path would be a path the ordinary lifecycle tests never
// cover, which is how a boundary measurement ends up leaking boxes the production
// path would have reclaimed.
// The named results below are load-bearing. Teardown runs in a defer because it must
// happen on every exit path, and a deferred append to a value that has already been
// copied into an anonymous return would silently drop the leak: the measurement would
// report a clean run with boxes still running.
func (p *Provider) MeasureKernelBoundary(ctx context.Context, boxes int) (report sandbox.KernelBoundaryReport, err error) {
	report = sandbox.KernelBoundaryReport{
		Boxes:      boxes,
		HostKernel: runtime.GOOS + "/" + runtime.GOARCH,
		Evidence:   []string{},
		Fixes:      []string{},
		Verdict:    sandbox.BoundaryUnknown,
	}
	if boxes < 2 || boxes > sandbox.MaxBoundaryBoxes {
		return report, &sandbox.BoundaryBudgetError{Boxes: boxes}
	}

	image := p.probeImage()
	// Ask the image question before paying for boots (D-36). A measurement that
	// provisioned three boxes to discover the image has no shell is a measurement
	// that wasted three boxes.
	if present, qerr := p.ImageAvailable(ctx, image); qerr == nil && !present {
		return report, p.unavailable("boundary image is not available",
			fmt.Errorf("image %q is not in this engine", image),
			fmt.Sprintf("pull or build %s, or bind the profile at an image that exists, then re-run hno sandbox boundary", image))
	}

	envs := make([]sandbox.Environment, 0, boxes)
	// Teardown is unconditional and does not inherit a deadline that may already
	// have passed: a measurement that leaks boxes is worse than one that fails.
	defer func() {
		for i, env := range envs {
			if err := env.Close(context.WithoutCancel(ctx)); err != nil {
				report.Fixes = append(report.Fixes, fmt.Sprintf("box %d (%s) could not be closed by this measurement: run hno sandbox prune to list and hno sandbox prune --yes to delete", i+1, env.ID()))
			}
		}
	}()

	for i := 0; i < boxes; i++ {
		session := fmt.Sprintf("hno-boundary-%d-%d", os.Getpid(), i)
		env, nerr := p.NewEnvironment(ctx, sandbox.EnvSpec{
			SessionID: session,
			Image:     image,
			Isolation: sandbox.IsolationContainer,
		})
		if nerr != nil {
			report.Evidence = append(report.Evidence, fmt.Sprintf("box %d never started: %v", i+1, nerr))
			return report, nerr
		}
		envs = append(envs, env)

		rep, xerr := env.Exec(ctx, sandbox.RunSpec{
			RunID:   fmt.Sprintf("boundary-%d-%d", os.Getpid(), i),
			Request: sandbox.Shell(boundaryProbe),
		}, nil)
		if xerr != nil {
			report.Evidence = append(report.Evidence, fmt.Sprintf("box %d could not be read: %v", i+1, xerr))
			return report, xerr
		}
		if rep == nil {
			report.Evidence = append(report.Evidence, fmt.Sprintf("box %d returned no report", i+1))
			continue
		}
		if rep.ExitCode != 0 {
			// A non-zero exit here is about the guest, not the caller's code: the box
			// has no shell to read from. Say which contract failed rather than
			// reporting a silent "unknown".
			report.Evidence = append(report.Evidence, fmt.Sprintf("box %d exited %d reading its kernel: %s", i+1, rep.ExitCode, strings.TrimSpace(rep.Stderr)))
			report.Fixes = append(report.Fixes, fmt.Sprintf("image %s has no usable shell for the boundary reading; bind WithImage to a minimal image that does (alpine works), then re-run hno sandbox boundary", image))
			continue
		}
		boot, up, kernel := parseBoundaryReading(rep.Stdout)
		report.BootIDs = append(report.BootIDs, boot)
		report.Uptimes = append(report.Uptimes, up)
		if kernel != "" {
			report.GuestKernel = kernel
		}
		report.Evidence = append(report.Evidence, fmt.Sprintf("box %d: boot_id=%s uptime=%ss kernel=%s", i+1, boot, up, kernel))
	}

	report.Evidence = append(report.Evidence, p.boundaryHostEvidence(report))
	report.Verdict = boundaryVerdict(report.BootIDs, boxes)
	switch report.Verdict {
	case sandbox.BoundaryPrivateKernel:
		report.Evidence = append(report.Evidence, fmt.Sprintf("%d boxes produced %d distinct kernel boot ids: one kernel per box", boxes, len(report.BootIDs)))
	case sandbox.BoundarySharedKernel:
		report.Evidence = append(report.Evidence, fmt.Sprintf("%d boxes produced %d distinct kernel boot ids: the boxes share a kernel, so this engine is namespace isolation, not a VM per box", boxes, distinct(report.BootIDs)))
	case sandbox.BoundaryUnknown:
		report.Fixes = append(report.Fixes, "the engine did not give readings worth believing: check that it can exec inside a box, then re-run hno sandbox boundary")
	}
	return report, nil
}

// parseBoundaryReading splits the three-line probe output. Missing lines are empty
// strings rather than errors: a box that answers two of three still contributes.
func parseBoundaryReading(out string) (boot, up, kernel string) {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	get := func(i int) string {
		if i < len(lines) {
			return strings.TrimSpace(lines[i])
		}
		return ""
	}
	return get(0), get(1), get(2)
}

// boundaryVerdict reads the sharing off the readings. All distinct means a kernel
// per box; any collision means sharing; anything short of one reading per box is
// unknown, because a partial measurement cannot support either conclusion.
func boundaryVerdict(ids []string, boxes int) sandbox.KernelBoundary {
	seen := map[string]bool{}
	count := 0
	for _, id := range ids {
		if strings.TrimSpace(id) == "" {
			continue
		}
		seen[id] = true
		count++
	}
	if count < boxes {
		return sandbox.BoundaryUnknown
	}
	if len(seen) == boxes {
		return sandbox.BoundaryPrivateKernel
	}
	return sandbox.BoundarySharedKernel
}

func distinct(ids []string) int {
	seen := map[string]bool{}
	for _, id := range ids {
		if strings.TrimSpace(id) == "" {
			continue
		}
		seen[id] = true
	}
	return len(seen)
}

// boundaryHostEvidence records the independent signal. A Linux guest kernel on a
// non-Linux host already proves a virtual machine exists — the boot-id comparison
// then decides whether it is one per box or one shared, and neither claim depends
// on trusting what the engine is called.
func (p *Provider) boundaryHostEvidence(report sandbox.KernelBoundaryReport) string {
	guest := strings.ToLower(report.GuestKernel)
	if guest == "" {
		return "the guest kernel could not be read, so the host/guest contrast is unavailable"
	}
	if !strings.HasPrefix(guest, "linux") && runtime.GOOS == "linux" {
		return fmt.Sprintf("guest kernel %q is not Linux while this host is: the boxes are not Linux-kernel workloads", report.GuestKernel)
	}
	if runtime.GOOS != "linux" && strings.HasPrefix(guest, "linux") {
		return fmt.Sprintf("guest kernel is Linux (%s) while this host runs %s: a VM boundary exists regardless of the boot-id comparison", report.GuestKernel, runtime.GOOS)
	}
	return fmt.Sprintf("guest kernel %s on a %s host: the boot-id comparison alone decides whether the kernel is shared", report.GuestKernel, runtime.GOOS)
}
