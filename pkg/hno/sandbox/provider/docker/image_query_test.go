package docker

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rexleimo/agno-go/pkg/hno/sandbox"
)

// D-36: the provider must answer "is this image here?" in the engine's own vocabulary, and
// a wrong vocabulary turns a usage error into a claim that an image is missing.

func TestImageAvailableAsksTheDialectItsOwnWay(t *testing.T) {
	tests := []struct {
		name  string
		dial  dialect
		bin   string
		image string
		// script answers the engine's lookup call
		exit   int
		stdout string
		want   bool
		// wantArgs is the shape the dialect must ask in
		mustContain []string
	}{
		{
			name:        "docker inspects the image directly",
			dial:        dockerDialect{},
			bin:         "docker",
			image:       "golang:1.26",
			exit:        0,
			stdout:      "sha256:abc123\n",
			want:        true,
			mustContain: []string{"image", "inspect", "golang:1.26"},
		},
		{
			name:        "docker treats a nonzero inspect as absent",
			dial:        dockerDialect{},
			bin:         "docker",
			image:       "nope:1",
			exit:        1,
			stdout:      "",
			want:        false,
			mustContain: []string{"image", "inspect"},
		},
		{
			name:        "apple parses the image table",
			dial:        appleDialect{},
			bin:         "container",
			image:       "golang:1.26",
			exit:        0,
			stdout:      "NAME   TAG   DIGEST\ngolang 1.26 sha256:abc\n",
			want:        true,
			mustContain: []string{"image", "list"},
		},
		{
			name:        "apple reports absent when the table has no row",
			dial:        appleDialect{},
			bin:         "container",
			image:       "golang:1.26",
			exit:        0,
			stdout:      "NAME   TAG   DIGEST\npython 3.12 sha256:ddd\n",
			want:        false,
			mustContain: []string{"image", "list"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fr := &fakeRunner{
				respond: func(c Command) (Outcome, bool) {
					if len(c.Args) > 0 && c.Args[0] == "image" {
						return Outcome{Exit: tc.exit, Stdout: tc.stdout}, true
					}
					return Outcome{}, false
				},
			}
			p := &Provider{
				d:      Config{Image: tc.image},
				runner: fr,
				bin:    tc.bin,
				dial:   tc.dial,
			}
			got, err := p.ImageAvailable(context.Background(), tc.image)
			if err != nil {
				t.Fatalf("ImageAvailable: %v", err)
			}
			if got != tc.want {
				t.Errorf("ImageAvailable(%q) = %v, want %v", tc.image, got, tc.want)
			}
			fr.mu.Lock()
			var asked string
			for _, c := range fr.seen {
				if len(c.Args) > 0 && c.Args[0] == "image" {
					asked = strings.Join(c.Args, " ")
				}
			}
			fr.mu.Unlock()
			if asked == "" {
				t.Fatal("the provider asked the engine nothing about the image")
			}
			for _, frag := range tc.mustContain {
				if !strings.Contains(asked, frag) {
					t.Errorf("the dialect must ask in its own vocabulary, missing %q in %q", frag, asked)
				}
			}
		})
	}
}

// An empty image is not "absent", it is a question that cannot be asked. Returning
// (false, nil) here would let Diagnose report a missing image for a handle that never
// named one.
func TestImageAvailableRefusesAnEmptyQuestion(t *testing.T) {
	p := &Provider{d: Config{}, runner: &fakeRunner{}, bin: "docker", dial: dockerDialect{}}
	ok, err := p.ImageAvailable(context.Background(), "")
	if err == nil {
		t.Fatalf("an empty image was answered as %v with no error; the caller cannot tell absent from unasked", ok)
	}
	if ok {
		t.Errorf("an empty image must not read as present")
	}
	if !strings.Contains(err.Error(), "image") {
		t.Errorf("the error should say what was missing, got %q", err)
	}
}

// The engine refusing to answer has to reach the caller as an error, not as "absent",
// otherwise a broken registry looks like an image that needs pulling.
func TestImageUnavailableEngineIsAnErrorNotAnAbsence(t *testing.T) {
	want := errors.New("docker: Cannot connect to the Docker daemon")
	fr := &fakeRunner{
		failFor: func(c Command) error {
			if len(c.Args) > 0 && c.Args[0] == "image" {
				return want
			}
			return nil
		},
	}
	p := &Provider{d: Config{Image: "golang:1.26"}, runner: fr, bin: "docker", dial: dockerDialect{}}
	ok, err := p.ImageAvailable(context.Background(), "golang:1.26")
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want the engine's own failure to travel up", err)
	}
	if ok {
		t.Errorf("a failed lookup reported the image as present")
	}
}

// The provider must satisfy the core's optional interface, otherwise the image half of
// Diagnose silently degrades to "provider offers no image query" for the one engine that
// can actually answer it.
func TestProviderSatisfiesImageQueryable(t *testing.T) {
	var p any = &Provider{}
	if _, ok := p.(sandbox.ImageQueryable); !ok {
		t.Fatal("*docker.Provider does not implement sandbox.ImageQueryable")
	}
}
