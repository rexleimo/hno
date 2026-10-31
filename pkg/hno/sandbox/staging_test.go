package sandbox

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stageRecorder is a FileService that records writes, so staging is testable
// without an engine and the assertions are about path shape rather than storage.
type stageRecorder struct {
	written map[string]string
	err     error
}

func (s *stageRecorder) Write(_ context.Context, path string, content []byte, _ uint32) error {
	if s.err != nil {
		return s.err
	}
	if s.written == nil {
		s.written = map[string]string{}
	}
	s.written[path] = string(content)
	return nil
}
func (s *stageRecorder) Read(context.Context, string) ([]byte, error) {
	return nil, ErrFileNotFound
}
func (s *stageRecorder) Open(context.Context, string, OpenOptions) (io.ReadWriteCloser, error) {
	return nil, ErrFileNotFound
}
func (s *stageRecorder) Stat(context.Context, string) (FileInfo, error) {
	return FileInfo{}, ErrFileNotFound
}
func (s *stageRecorder) List(context.Context, string) ([]FileInfo, error) { return nil, nil }
func (s *stageRecorder) Remove(context.Context, string) error             { return nil }
func (s *stageRecorder) MkdirAll(context.Context, string, uint32) error   { return nil }
func (s *stageRecorder) CopyIn(context.Context, string, io.Reader) error  { return nil }
func (s *stageRecorder) CopyOut(context.Context, string, io.Writer) error { return nil }

// TestStageDirWritesWorkspaceRelativePaths pins the contract that broke the first
// real project run: a provider's file bridge is rooted at the workspace and refuses
// absolute paths, so staging must hand it relative ones.
func TestStageDirWritesWorkspaceRelativePaths(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module demo\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "src", "main.go"), []byte("package main\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "skip.me"), []byte("noise\n"), 0o644)

	rec := &stageRecorder{}
	sb := &Sandbox{}
	err := sb.stageDir(context.Background(), rec, &WorkspaceSource{
		Kind: SourceDir, Dir: dir, Ignore: []string{"skip*"},
	})
	if err != nil {
		t.Fatalf("stageDir: %v", err)
	}
	for _, want := range []string{"go.mod", "src/main.go"} {
		if _, ok := rec.written[want]; !ok {
			t.Fatalf("staged %q missing; got %v", want, rec.written)
		}
	}
	if _, ok := rec.written["skip.me"]; ok {
		t.Fatal("Ignore pattern did not exclude the file")
	}
	for got := range rec.written {
		if strings.HasPrefix(got, "/") || strings.HasPrefix(got, workspaceRoot) {
			t.Fatalf("staged %q as an in-guest path; the bridge roots it itself", got)
		}
	}
}

func TestStageDirEnforcesByteLimitAndRejectsEmptyTree(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("12345"), 0o644)

	sb := &Sandbox{}
	err := sb.stageDir(context.Background(), &stageRecorder{}, &WorkspaceSource{
		Kind: SourceDir, Dir: dir, MaxBytes: 3,
	})
	var we *WorkspaceError
	if !errors.As(err, &we) || we.Reason != "over byte limit" {
		t.Fatalf("over-limit directory: %v (%T)", err, err)
	}

	// An empty snapshot is a failure, not a success: a payload that runs against a
	// workspace with no files produces confusing errors, so "nothing was copied"
	// must stop before Exec.
	empty := t.TempDir()
	if err := sb.stageDir(context.Background(), &stageRecorder{}, &WorkspaceSource{
		Kind: SourceDir, Dir: empty,
	}); err == nil {
		t.Fatal("a directory staging zero files must fail")
	}
}

// TestStageInputsOrdering checks precedence: a workspace tree is authoritative over
// inline files that name the same path, because the tree is the project.
func TestStageInputsOrdering(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "only.txt"), []byte("tree\n"), 0o644)

	rec := &stageRecorder{}
	sb := &Sandbox{}
	req := Request{
		Workspace: &WorkspaceSource{Kind: SourceDir, Dir: dir},
		Files:     map[string]FileContent{"only.txt": {Content: []byte("inline")}},
	}
	if err := sb.stageInline(context.Background(), rec, &req); err != nil {
		t.Fatalf("stageInline: %v", err)
	}
	if err := sb.stageDir(context.Background(), rec, req.Workspace); err != nil {
		t.Fatalf("stageDir: %v", err)
	}
	if got := rec.written["only.txt"]; got != "tree\n" {
		t.Fatalf("workspace directory must win over inline files for the same path, got %q", got)
	}
}

// TestStageDirSkipsSymlinks keeps a snapshot from becoming a link into the host: the
// point of CopySnapshot is that the container cannot reach outside what was declared.
func TestStageDirSkipsSymlinks(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, "..", "outside-secret.txt")
	os.WriteFile(secret, []byte("do not ship\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "real.txt"), []byte("ok\n"), 0o644)
	if err := os.Symlink(secret, filepath.Join(dir, "link.txt")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	rec := &stageRecorder{}
	sb := &Sandbox{}
	if err := sb.stageDir(context.Background(), rec, &WorkspaceSource{Kind: SourceDir, Dir: dir}); err != nil {
		t.Fatalf("stageDir: %v", err)
	}
	if _, ok := rec.written["link.txt"]; ok {
		t.Fatal("a symlink was staged; the snapshot would resolve to a host path outside the workspace")
	}
	if _, ok := rec.written["real.txt"]; !ok {
		t.Fatalf("regular files must still stage: %v", rec.written)
	}
}
