package boxlite

import (
	"strings"
	"testing"

	"github.com/rexleimo/agno-go/pkg/hno/sandbox"
)

// The verdict must follow from the readings. A provider that claims a micro-VM and
// then shares a kernel is the over-claim D-38 exists to catch, so the collision case is
// the one worth pinning: it is the branch a wrong implementation never reaches.
func TestBoundaryVerdictReadsSharing(t *testing.T) {
	cases := []struct {
		name  string
		ids   []string
		boxes int
		want  sandbox.KernelBoundary
	}{
		{name: "one kernel per box", ids: []string{"a", "b"}, boxes: 2, want: sandbox.BoundaryPrivateKernel},
		{name: "shared kernel", ids: []string{"a", "a"}, boxes: 2, want: sandbox.BoundarySharedKernel},
		{name: "three boxes, one shared", ids: []string{"a", "b", "b"}, boxes: 3, want: sandbox.BoundarySharedKernel},
		{name: "one box unread", ids: []string{"a", ""}, boxes: 2, want: sandbox.BoundaryUnknown},
		{name: "no readings", ids: nil, boxes: 2, want: sandbox.BoundaryUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := boundaryVerdict(tc.ids, tc.boxes); got != tc.want {
				t.Fatalf("boundaryVerdict(%v, %d) = %s, want %s", tc.ids, tc.boxes, got, tc.want)
			}
		})
	}
}

// A partial reading cannot support either conclusion, and reporting "private" from a
// missing reading is how a shared engine passes an isolation audit.
func TestBoundaryVerdictNeverExtrapolates(t *testing.T) {
	if got := boundaryVerdict([]string{"a"}, 2); got == sandbox.BoundaryPrivateKernel {
		t.Fatal("one reading for two boxes was reported as one kernel per box")
	}
}

func TestHostEvidenceTreatsACrossOSGuestAsAVM(t *testing.T) {
	ev := boundaryHostEvidence(sandbox.KernelBoundaryReport{GuestKernel: "Linux 6.12.87"})
	if !strings.Contains(ev, "VM boundary exists") && !strings.Contains(ev, "boot-id comparison alone") {
		t.Fatalf("host evidence %q neither claims a VM nor defers to the comparison", ev)
	}
}
