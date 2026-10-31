package docker

import (
	"context"
	"strings"
	"testing"

	"github.com/rexleimo/agno-go/pkg/hno/sandbox"
)

// TestRuntimeAttributeReachesCreate checks the attribute is not decoration: it must show
// up in the argv the engine is asked to run, and the probe must ask the same question so
// "engine is usable" means "usable with this runtime".
func TestRuntimeAttributeReachesCreate(t *testing.T) {
	fr := &fakeRunner{}
	p, err := New(Config{Image: "golang:1.26", Runtime: "runsc", Runner: fr})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := p.NewEnvironment(context.Background(), sandbox.EnvSpec{SessionID: "rt", Image: "golang:1.26"}); err != nil {
		t.Fatalf("NewEnvironment: %v", err)
	}
	create := firstCall(fr, "create")
	if create == nil {
		t.Fatal("no create was issued")
	}
	if i := indexOf(create, "--runtime"); i == -1 || i+1 >= len(create) || create[i+1] != "runsc" {
		t.Fatalf("create did not request the configured runtime: %v", create)
	}
	// The probe has to answer for the same runtime.
	pb := &fakeRunner{}
	pp, _ := New(Config{Image: "golang:1.26", Runtime: "runsc", Runner: pb})
	_ = pp.Probe(context.Background())
	probeCreate := lastCall(pb, "create")
	if probeCreate == nil {
		t.Fatal("the probe never attempted a create")
	}
	if indexOf(probeCreate, "--runtime") == -1 {
		t.Fatalf("probe asked the engine to create without the runtime it will use: %v", probeCreate)
	}
}

// TestUnknownRuntimeAttributeIsRefused keeps the passthrough honest.
func TestUnknownRuntimeAttributeIsRefused(t *testing.T) {
	_, err := NewProvider(sandbox.ProviderConfig{
		Image: "golang:1.26",
		Raw:   map[string]any{"runtme": "runsc"},
	})
	if err == nil {
		t.Fatal("a misspelled engine attribute must be refused, not quietly ignored")
	}
	if !strings.Contains(err.Error(), "runtime") {
		t.Fatalf("the refusal should list the known keys including runtime: %v", err)
	}
}

func indexOf(hay []string, needle string) int {
	for i, h := range hay {
		if h == needle {
			return i
		}
	}
	return -1
}

// firstCall returns the argv of the first recorded call matching want, or nil.
func firstCall(f *fakeRunner, want ...string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.seen {
		if hasArgPrefix(c.Args, want) {
			return c.Args
		}
	}
	return nil
}

func lastCall(f *fakeRunner, want ...string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.seen {
		if hasArgPrefix(c.Args, want) {
			out = c.Args
		}
	}
	return out
}
