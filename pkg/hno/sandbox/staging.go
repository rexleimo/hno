package sandbox

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// staging.go materialises a run's declared inputs into the environment before the
// payload executes.
//
// This lives in the core rather than in each provider for the same reason the rest
// of the pipeline does: "what I declared is what ran" has to hold for every engine.
// A provider-side copy would mean directory walking, ignore patterns, byte ceilings
// and path confinement get re-implemented per engine and drift — and the drift is
// invisible, because the payload simply finds fewer files than it declared.

// workspaceRoot is where a staged workspace lives inside the sandbox, and what the
// payload's working directory is. It is fixed rather than per-request so a request
// cannot relocate its own confinement by naming a target directory: the mount mode
// decides whether that path is a copy or a share, and the request decides only what
// runs there.
//
// Staging does not send this prefix to a FileService. A provider's file bridge is
// rooted at the workspace and refuses absolute paths on purpose (see
// resolveInRoot), so the paths handed to Write are workspace-relative; this
// constant describes the guest-side view and the CWD the engine is told to use.
const workspaceRoot = "/workspace"

// stageInputs pushes declared inputs into the environment.
//
// Inline files, then a workspace directory, then named inputs. It is a no-op when
// the request declares none of those, so the common single-shot code path pays
// nothing.
func (sb *Sandbox) stageInputs(ctx context.Context, env Environment, req *Request) error {
	if req == nil {
		return nil
	}
	needsBridge := len(req.Files) > 0 ||
		(req.Workspace != nil && req.Workspace.Kind == SourceDir) ||
		len(req.Inputs) > 0
	if !needsBridge {
		return nil
	}
	// validateCapabilities already demands CapFileBridge for these shapes; the
	// check is repeated here because staging is also reachable from the session
	// path, and a silent no-op on a missing bridge would run a payload against an
	// empty directory it believes is populated.
	if !sb.caps.Has(CapFileBridge) {
		return &CapabilityError{Provider: sb.name, Missing: CapFileBridge, Wanted: sb.caps}
	}
	bridge, ok := sb.provider.(FileBridge)
	if !ok {
		return &CapabilityError{Provider: sb.name, Missing: CapFileBridge, Wanted: sb.caps}
	}
	files, err := bridge.Files(ctx, rawEnvironment(env))
	if err != nil {
		return fmt.Errorf("sandbox: open file bridge: %w", err)
	}

	if err := sb.stageInline(ctx, files, req); err != nil {
		return err
	}
	if req.Workspace != nil && req.Workspace.Kind == SourceDir {
		if err := sb.stageDir(ctx, files, req.Workspace); err != nil {
			return err
		}
	}
	return sb.stageInputsList(ctx, files, req)
}

// stageInline writes Request.Files. The map is sorted so identical requests stage
// identically, which keeps a replayed run byte-for-byte comparable (§17.1).
func (sb *Sandbox) stageInline(ctx context.Context, files FileService, req *Request) error {
	paths := make([]string, 0, len(req.Files))
	for p := range req.Files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		f := req.Files[p]
		// The map key is the in-sandbox destination, so it is confinement-checked
		// here exactly as an artifact path is on the way out.
		if err := checkRelPath("inline file", p); err != nil {
			return err
		}
		target := filepath.ToSlash(p)
		if err := files.Write(ctx, target, f.Content, f.Mode); err != nil {
			return fmt.Errorf("sandbox: stage inline file %q: %w", p, err)
		}
	}
	return nil
}

// stageDir walks a host directory into the workspace under CopySnapshot rules:
// the container reads a snapshot and cannot write the host tree (D-12).
func (sb *Sandbox) stageDir(ctx context.Context, files FileService, src *WorkspaceSource) error {
	root := src.Dir
	if root == "" {
		return errors.New("sandbox: workspace declares SourceDir with an empty path")
	}
	info, err := os.Stat(root)
	if err != nil {
		return fmt.Errorf("sandbox: workspace directory %q: %w", root, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("sandbox: workspace %q is not a directory", root)
	}
	limit := src.MaxBytes
	if limit == 0 {
		limit = sb.cfg.Limits.WorkspaceMaxBytes
	}
	// limit <= 0 means "not bounded here", matching how a zero ceiling reads
	// elsewhere in Limits. Without this, a host that builds Limits{} field by field
	// (so WorkspaceMaxBytes stays zero) had every single directory file rejected as
	// "over byte limit", and a zero-value Sandbox could not stage anything at all.
	bounded := limit > 0

	var staged, total int64
	// Sorted traversal keeps materialisation deterministic across filesystems,
	// which matters because the same request must produce the same environment on
	// rebuild, not merely a similar one.
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			// An unreadable entry is a real gap in the declared input. Skipping it
			// would hand the payload a project that is quietly missing a file.
			return fmt.Errorf("sandbox: workspace entry %q: %w", path, werr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		if rel == "." {
			return nil
		}
		if matchesIgnore(src.Ignore, rel) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return files.MkdirAll(ctx, filepath.ToSlash(rel), 0)
		}
		if !d.Type().IsRegular() {
			// Symlinks, sockets and devices are not copied. A symlink out of the
			// tree would turn "snapshot of this directory" into "whatever the link
			// pointed at", which is the escape this confinement exists to prevent.
			return nil
		}
		entryInfo, ierr := d.Info()
		if ierr != nil {
			return fmt.Errorf("sandbox: stat workspace entry %q: %w", path, ierr)
		}
		if bounded && total+entryInfo.Size() > limit {
			return &WorkspaceError{Root: root, Reason: "over byte limit",
				Found: total + entryInfo.Size(), Limit: limit}
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return fmt.Errorf("sandbox: read workspace entry %q: %w", path, rerr)
		}
		if err := files.Write(ctx, filepath.ToSlash(rel), data, uint32(entryInfo.Mode().Perm())); err != nil {
			return fmt.Errorf("sandbox: stage %q: %w", rel, err)
		}
		total += entryInfo.Size()
		staged++
		return nil
	})
	if err != nil {
		return err
	}
	if staged == 0 {
		// An empty workspace is usually a mistake: the caller declared a project
		// directory and the payload will fail in a way that looks like its own
		// fault. Saying so here is the difference between a useful error and a
		// confusing one.
		return &WorkspaceError{Root: root, Reason: "no files matched the declared workspace"}
	}
	sb.emit(ctx, Event{Kind: EventPhaseStarted, Detail: map[string]any{
		"stage": "workspace", "files": staged, "bytes": total,
	}})
	return nil
}

// stageInputsList writes explicitly named inputs, each of which is a file on the
// host resolved relative to nothing: an input names exactly what it says.
func (sb *Sandbox) stageInputsList(ctx context.Context, files FileService, req *Request) error {
	for _, in := range req.Inputs {
		if err := checkRelPath("input", in.Path); err != nil {
			return err
		}
		data, src, err := readInput(in)
		if err != nil {
			return err
		}
		mode := in.Mode
		if mode == 0 && in.Executable {
			mode = 0o755
		}
		if err := files.Write(ctx, filepath.ToSlash(in.Path), data, mode); err != nil {
			return fmt.Errorf("sandbox: stage input %q: %w", src, err)
		}
	}
	return nil
}

// matchesIgnore reports whether a workspace-relative path hits an ignore pattern.
// Matching is on the path or any parent directory, so ".git" excludes everything
// beneath it the way a caller expects a gitignore-style list to behave.
func matchesIgnore(patterns []string, rel string) bool {
	if len(patterns) == 0 {
		return false
	}
	clean := filepath.ToSlash(rel)
	segments := strings.Split(clean, "/")
	for _, pat := range patterns {
		p := strings.TrimSpace(pat)
		if p == "" {
			continue
		}
		p = strings.TrimSuffix(strings.TrimPrefix(p, "./"), "/")
		for i, seg := range segments {
			if seg == p || matchGlob(p, strings.Join(segments[i:], "/")) {
				return true
			}
		}
	}
	return false
}

// readInput resolves one declared input into bytes, naming the source it came from
// so a failure says which of the three alternatives was at fault.
func readInput(in Input) ([]byte, string, error) {
	switch {
	case len(in.Bytes) > 0:
		return in.Bytes, "inline bytes", nil
	case in.HostPath != "":
		data, err := os.ReadFile(in.HostPath)
		if err != nil {
			return nil, in.HostPath, fmt.Errorf("sandbox: read input %q: %w", in.HostPath, err)
		}
		return data, in.HostPath, nil
	case in.Reader != nil:
		data, err := io.ReadAll(in.Reader)
		if err != nil {
			return nil, "reader", fmt.Errorf("sandbox: read input stream: %w", err)
		}
		return data, "reader", nil
	default:
		return nil, "", fmt.Errorf("sandbox: input %q declares no content source", in.Path)
	}
}

// WorkspaceError explains that a declared workspace could not be materialised.
type WorkspaceError struct {
	Root   string
	Reason string
	Found  int64
	Limit  int64
}

func (e *WorkspaceError) Error() string {
	if e.Limit > 0 && strings.Contains(e.Reason, "byte limit") {
		return fmt.Sprintf("sandbox: workspace %q rejected: %s (%d of %d bytes)", e.Root, e.Reason, e.Found, e.Limit)
	}
	return fmt.Sprintf("sandbox: workspace %q rejected: %s", e.Root, e.Reason)
}

// Remediation names the two ways out of a rejected workspace.
func (e *WorkspaceError) Remediation() []string {
	if strings.Contains(e.Reason, "byte limit") {
		return []string{"raise Limits.WorkspaceMaxBytes", "exclude large directories with FromDir(dir, ignore...)"}
	}
	return []string{"check the directory path", "relax the Ignore patterns", "or declare the files with Request.Files"}
}
