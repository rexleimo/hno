package sandbox

import (
	"context"
	"io"
	"time"
)

// FileService is the workspace file view of one session. It is the single root
// that fixes the current split where the execution sandbox and the file sandbox
// are two worlds and E2B workspace mounts are simply not implemented
// (e2b.go:101). Paths are workspace-relative; anything else is a *PathError.
type FileService interface {
	// Write creates or replaces a file. mode 0 means "provider default".
	Write(ctx context.Context, path string, content []byte, mode uint32) error
	// Read returns the whole file, bounded by Limits.ArtifactMaxBytes; larger
	// content must go through Open.
	Read(ctx context.Context, path string) ([]byte, error)
	// Open streams a file for reading or writing.
	Open(ctx context.Context, path string, opts OpenOptions) (io.ReadWriteCloser, error)
	// Stat reports one entry; missing files return ErrFileNotFound.
	Stat(ctx context.Context, path string) (FileInfo, error)
	// List enumerates a directory non-recursively.
	List(ctx context.Context, dir string) ([]FileInfo, error)
	// Remove deletes a file or directory tree. It is never a silent no-op:
	// removing the workspace root is a *PathError.
	Remove(ctx context.Context, path string) error
	// MkdirAll creates a directory tree.
	MkdirAll(ctx context.Context, dir string, mode uint32) error
	// CopyIn streams host content into the sandbox. Bulk project input belongs
	// to Request.Workspace instead (§13.4).
	CopyIn(ctx context.Context, dir string, r io.Reader) error
	// CopyOut streams a subtree out as a tar archive.
	CopyOut(ctx context.Context, dir string, w io.Writer) error
}

// ErrFileNotFound reports a missing path without leaking whether the parent
// directory exists.
var ErrFileNotFound = errNotFound("sandbox: file not found")

type errNotFound string

func (e errNotFound) Error() string { return string(e) }

// OpenOptions tunes Open.
type OpenOptions struct {
	Read   bool
	Write  bool
	Append bool
	Mode   uint32
}

// FileInfo describes one workspace entry.
type FileInfo struct {
	Path    string
	Size    int64
	Mode    uint32
	Dir     bool
	Symlink bool
	ModTime time.Time
}
