package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rexleimo/agno-go/pkg/hno/sandbox"
)

// Environment is one live container.
type Environment struct {
	provider *Provider
	id       string
	session  string

	mu   sync.Mutex
	spec sandbox.SessionSpec

	closed  bool
	lastErr error
}

// ID implements sandbox.Environment.
func (e *Environment) ID() string { return e.id }

// Spec returns the recorded replayable spec for this environment.
func (e *Environment) Spec() sandbox.SessionSpec {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.spec.Clone()
}

// Exec implements sandbox.Environment.
//
// The report is populated even on failure paths. A provider that returns an
// error and an empty report leaves the caller with nothing to audit, and forces
// the core to guess whether anything ran at all.
func (e *Environment) Exec(ctx context.Context, rs sandbox.RunSpec, sink sandbox.ChunkReporter) (*sandbox.Report, error) {
	started := time.Now()
	rep := &sandbox.Report{
		RunID:     rs.RunID,
		SessionID: e.session,
		Provider:  e.provider.Name(),
		Isolation: sandbox.IsolationContainer,
	}

	if e.isClosed() {
		err := &sandbox.LostError{RunID: rs.RunID, SessionID: e.session, Provider: "docker"}
		rep.ExitCode = -1
		rep.Lost = true
		rep.Duration = time.Since(started)
		rep.Failure = sandbox.AsFailure(err)
		return rep, err
	}

	// The container being alive is a fact only the engine can answer. A local
	// map says what we created; docker inspect says what still exists, which is
	// how an externally-killed container becomes ClassLost instead of a confusing
	// "exec failed".
	if alive, err := e.alive(ctx); err != nil {
		rep.ExitCode = -1
		rep.Duration = time.Since(started)
		rep.Failure = sandbox.AsFailure(err)
		return rep, err
	} else if !alive {
		err := &sandbox.LostError{RunID: rs.RunID, SessionID: e.session, Provider: "docker", Cause: errors.New("container is no longer running")}
		rep.ExitCode = -1
		rep.Lost = true
		rep.Duration = time.Since(started)
		rep.Failure = sandbox.AsFailure(err)
		return rep, err
	}

	argv, err := commandFor(rs.Request)
	if err != nil {
		rep.ExitCode = -1
		rep.Duration = time.Since(started)
		rep.Failure = sandbox.AsFailure(err)
		return rep, err
	}

	args := append([]string{"exec", "-i"}, e.id)
	args = append(args, argv...)
	cmd := Command{Bin: e.provider.bin, Args: args}
	if payload := payloadFor(rs.Request); payload != "" {
		cmd.Stdin = []byte(payload)
	}

	res, runErr := e.provider.runner.Run(ctx, cmd)
	rep.Stdout = res.Stdout
	rep.Stderr = res.Stderr
	rep.ExitCode = res.Exit
	rep.Duration = time.Since(started)

	if res.Canceled {
		// Cancellation is the caller's decision, not a failure of the payload or
		// the engine, and it must not be retried as either.
		rep.Canceled = true
		err := context.Canceled
		rep.Failure = sandbox.AsFailure(err)
		return rep, err
	}
	if runErr != nil {
		// The CLI itself could not run: infrastructure, not user code.
		err := e.provider.unavailable("exec could not be dispatched", runErr, "check the daemon is responsive")
		rep.ExitCode = -1
		rep.Failure = sandbox.AsFailure(err)
		return rep, err
	}

	if sink != nil && rep.Stdout != "" {
		// Streaming is declared, so a sink that asked for chunks has to receive
		// them. Emitting one whole buffer is honest for a provider that only gets
		// output at exit; pretending incremental delivery would be worse than
		// declaring no streaming at all.
		if err := sink.Report(sandbox.OutputChunk{RunID: rs.RunID, Stream: sandbox.StreamStdout, Bytes: []byte(rep.Stdout), At: time.Now().UTC()}); err != nil && !errors.Is(err, context.Canceled) {
			rep.Failure = sandbox.AsFailure(err)
			return rep, err
		}
	}
	if rep.ExitCode != 0 {
		// A nonzero exit is the payload speaking, not the sandbox breaking. It
		// returns as a populated report with a user-code failure and a nil error,
		// which is what lets a toolkit distinguish "my code is wrong" from "the
		// sandbox is broken" — the exact conflation the current run path ships.
		rep.Failure = &sandbox.Failure{
			Class:      sandbox.ClassUserCode,
			NextAction: sandbox.ActionModelFix,
			Cause:      fmt.Errorf("exit code %d", rep.ExitCode),
		}
	}
	return rep, nil
}

// Close implements sandbox.Environment. It is idempotent: a second close reports
// success rather than a spurious engine error.
func (e *Environment) Close(ctx context.Context) error {
	if e.isClosed() {
		return nil
	}
	err := e.provider.remove(ctx, e.id)
	e.mu.Lock()
	e.closed = true
	e.lastErr = err
	e.mu.Unlock()
	return err
}

// files returns the workspace file view for this environment. The bridge is
// exposed at provider level (sandbox.FileBridge), matching how the core reaches
// it: Files(ctx, env).
func (e *Environment) files() *fileService {
	return &fileService{env: e}
}

func (e *Environment) isClosed() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.closed
}

// alive asks the engine whether the container still exists and is running.
// alive asks the engine whether the container is up, through the dialect.
//
// This used to issue `inspect --format {{.State.Running}}` directly. Against
// Apple's CLI, which has no --format on inspect, that command exits nonzero, the
// provider read "not running", and a healthy container was reported as a lost
// environment — the worst possible mistake, because ClassLost drives rebuild and
// retry against work that was going fine. The question belongs to the dialect for
// exactly this reason.
func (e *Environment) alive(ctx context.Context) (bool, error) {
	return e.provider.dial.running(ctx, e.provider.runner, e.provider.bin, e.id)
}

// commandFor turns a request into a container argv. Payload bytes go to stdin
// or to a file path, never into the argument list, so a program that starts
// with a dash cannot be mistaken for a flag and shell metacharacters in code
// stay data.
func commandFor(req sandbox.Request) ([]string, error) {
	switch {
	case len(req.Argv) > 0:
		out := append([]string(nil), req.Argv...)
		if req.Cwd == "" {
			return out, nil
		}
		// A working directory is applied inside the container. The path is
		// workspace-relative and re-quoted, so a Cwd containing shell syntax
		// changes a directory name, not the command.
		if err := checkWorkspacePath(req.Cwd); err != nil {
			return nil, err
		}
		wrapped := []string{"sh", "-c", "cd " + shellQuote("/workspace/"+req.Cwd) + " && exec " + strings.Join(quoteAll(out), " ")}
		return wrapped, nil
	case req.Runtime != "":
		argv, err := runtimeEntrypoint(req.Runtime)
		if err != nil {
			return nil, err
		}
		if req.Code == "" && len(req.Inputs) == 0 {
			return nil, fmt.Errorf("sandbox/provider/docker: runtime %q needs code or inputs", req.Runtime)
		}
		return argv, nil
	default:
		return nil, fmt.Errorf("sandbox/provider/docker: a run needs either argv or a runtime with code")
	}
}

// payloadFor returns stdin content for a request, if any.
func payloadFor(req sandbox.Request) string {
	if req.Code != "" && req.Runtime != "" {
		return req.Code
	}
	var b strings.Builder
	for _, in := range req.Inputs {
		b.Write(in.Bytes)
	}
	return b.String()
}

func quoteAll(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = shellQuote(s)
	}
	return out
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// fileService is the docker-cp-backed view of one environment's workspace.
type fileService struct {
	env *Environment
}

// Write stores one file by streaming it in through tar, so path handling stays
// inside the container's own filesystem rather than in a host temp tree.
func (f *fileService) Write(ctx context.Context, target string, content []byte, mode uint32) error {
	path := target
	if err := checkWorkspacePath(path); err != nil {
		return err
	}
	// mkdir -p the parent, then write through a stdin pipe. The path is quoted on
	// the container side, so a filename containing a space or a shell metacharacter
	// stays a filename.
	dir := filepath.Dir(path)
	if dir != "." && dir != "" {
		if _, err := f.exec(ctx, "mkdir", "-p", "/workspace/"+dir); err != nil {
			return err
		}
	}
	res, err := f.providerRun(ctx, Command{
		Bin:   f.env.provider.bin,
		Args:  []string{"exec", "-i", f.env.id, "sh", "-c", "cat > " + shellQuote("/workspace/"+path)},
		Stdin: content,
	})
	if err != nil {
		return err
	}
	if res.Exit != 0 {
		return &sandbox.PathError{Path: path, Reason: strings.TrimSpace(res.Stderr)}
	}
	if mode != 0 {
		if _, err := f.exec(ctx, "chmod", octal(mode), "/workspace/"+path); err != nil {
			return err
		}
	}
	return nil
}

// Read returns one file's content.
func (f *fileService) Read(ctx context.Context, path string) ([]byte, error) {
	if err := checkWorkspacePath(path); err != nil {
		return nil, err
	}
	res, err := f.providerRun(ctx, Command{
		Bin:  f.env.provider.bin,
		Args: []string{"exec", f.env.id, "cat", "/workspace/" + path},
	})
	if err != nil {
		return nil, err
	}
	if res.Exit != 0 {
		if looksAbsent(res.Stderr) {
			return nil, sandbox.ErrFileNotFound
		}
		return nil, &sandbox.PathError{Path: path, Reason: strings.TrimSpace(res.Stderr)}
	}
	return []byte(res.Stdout), nil
}

// Open returns a buffered handle. The engine has no persistent file descriptor
// reachable through the CLI, so reads materialise on open and writes commit on
// Close. Callers that need true streaming use Read/Write or CopyIn/CopyOut; this
// is stated instead of faked, because a handle that silently loses a partially
// written file is worse than no handle.
func (f *fileService) Open(ctx context.Context, path string, opts sandbox.OpenOptions) (io.ReadWriteCloser, error) {
	if !opts.Read && !opts.Write {
		return nil, &sandbox.PathError{Path: path, Reason: "Open needs Read or Write"}
	}
	if opts.Write && !opts.Read {
		return &rwHandle{fs: f, path: path}, nil
	}
	data, err := f.Read(ctx, path)
	if err != nil {
		return nil, err
	}
	return &rwHandle{fs: f, path: path, data: data, readPos: 0, readOnly: true}, nil
}

// Stat reports one entry.
func (f *fileService) Stat(ctx context.Context, path string) (sandbox.FileInfo, error) {
	if err := checkWorkspacePath(path); err != nil {
		return sandbox.FileInfo{}, err
	}
	res, err := f.providerRun(ctx, Command{
		Bin:  f.env.provider.bin,
		Args: []string{"exec", f.env.id, "sh", "-c", statProbe("/workspace/" + path)},
	})
	if err != nil {
		return sandbox.FileInfo{}, err
	}
	if res.Exit != 0 || strings.TrimSpace(res.Stdout) == "" {
		return sandbox.FileInfo{}, sandbox.ErrFileNotFound
	}
	return parseStat(path, strings.TrimSpace(res.Stdout))
}

// List enumerates one directory non-recursively.
func (f *fileService) List(ctx context.Context, dir string) ([]sandbox.FileInfo, error) {
	if dir != "" {
		if err := checkWorkspacePath(dir); err != nil {
			return nil, err
		}
	}
	target := "/workspace"
	if dir != "" {
		target = "/workspace/" + dir
	}
	res, err := f.providerRun(ctx, Command{
		Bin:  f.env.provider.bin,
		Args: []string{"exec", f.env.id, "sh", "-c", "for p in " + target + "/*; do [ -e \"$p\" ] && stat -c '%n|%s|%F' \"$p\"; done"},
	})
	if err != nil {
		return nil, err
	}
	var out []sandbox.FileInfo
	for _, line := range strings.Split(res.Stdout, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		info, err := parseStat(strings.TrimPrefix(line, target+"/"), line)
		if err != nil {
			continue
		}
		out = append(out, info)
	}
	return out, nil
}

// Remove deletes a path inside the workspace.
func (f *fileService) Remove(ctx context.Context, path string) error {
	if err := checkWorkspacePath(path); err != nil {
		return err
	}
	if cleanWorkspace(path) == "" || cleanWorkspace(path) == "." {
		return &sandbox.PathError{Path: path, Reason: "removing the workspace root is refused"}
	}
	res, err := f.providerRun(ctx, Command{
		Bin:  f.env.provider.bin,
		Args: []string{"exec", f.env.id, "rm", "-rf", "/workspace/" + path},
	})
	if err != nil {
		return err
	}
	if res.Exit != 0 && !looksAbsent(res.Stderr) {
		return &sandbox.PathError{Path: path, Reason: strings.TrimSpace(res.Stderr)}
	}
	return nil
}

// MkdirAll creates a directory tree.
func (f *fileService) MkdirAll(ctx context.Context, dir string, _ uint32) error {
	if err := checkWorkspacePath(dir); err != nil {
		return err
	}
	_, err := f.exec(ctx, "mkdir", "-p", "/workspace/"+dir)
	return err
}

// CopyIn streams a tar into the workspace. The engine's own cp is not used here
// because it needs a host path, and this provider's contract is that project
// bytes arrive as a stream (D-12, §13).
func (f *fileService) CopyIn(ctx context.Context, dir string, r io.Reader) error {
	if dir != "" {
		if err := checkWorkspacePath(dir); err != nil {
			return err
		}
		if err := f.MkdirAll(ctx, dir, 0o755); err != nil {
			return err
		}
	}
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	res, err := f.providerRun(ctx, Command{
		Bin:   f.env.provider.bin,
		Args:  []string{"exec", "-i", f.env.id, "tar", "-x", "-f", "-", "-C", "/workspace/" + dir},
		Stdin: data,
	})
	if err != nil {
		return err
	}
	if res.Exit != 0 {
		return &sandbox.PathError{Path: dir, Reason: "tar extract failed: " + strings.TrimSpace(res.Stderr)}
	}
	return nil
}

// CopyOut streams a subtree out as tar.
func (f *fileService) CopyOut(ctx context.Context, dir string, w io.Writer) error {
	if dir != "" {
		if err := checkWorkspacePath(dir); err != nil {
			return err
		}
	}
	res, err := f.providerRun(ctx, Command{
		Bin:  f.env.provider.bin,
		Args: []string{"exec", f.env.id, "tar", "-c", "-f", "-", "-C", "/workspace", dir},
	})
	if err != nil {
		return err
	}
	if res.Exit != 0 {
		return &sandbox.PathError{Path: dir, Reason: "tar create failed: " + strings.TrimSpace(res.Stderr)}
	}
	_, err = w.Write([]byte(res.Stdout))
	return err
}

func (f *fileService) exec(ctx context.Context, argv ...string) (Outcome, error) {
	args := append([]string{"exec", f.env.id}, argv...)
	return f.providerRun(ctx, Command{Bin: f.env.provider.bin, Args: args})
}

func (f *fileService) providerRun(ctx context.Context, cmd Command) (Outcome, error) {
	if f.env.isClosed() {
		return Outcome{}, &sandbox.LostError{SessionID: f.env.session, Provider: "docker", Cause: errors.New("environment is closed")}
	}
	return f.env.provider.runner.Run(ctx, cmd)
}

// rwHandle is the CLI-backed file handle: buffered reads, commit-on-close writes.
type rwHandle struct {
	fs       *fileService
	path     string
	data     []byte
	readPos  int
	written  bool
	readOnly bool
	closed   bool
}

// Read fills p from the materialised content.
func (h *rwHandle) Read(p []byte) (int, error) {
	if h.closed {
		return 0, io.ErrClosedPipe
	}
	if h.readPos >= len(h.data) {
		if len(h.data) == 0 {
			return 0, io.EOF
		}
		return 0, io.EOF
	}
	n := copy(p, h.data[h.readPos:])
	h.readPos += n
	return n, nil
}

// Write buffers content until Close.
func (h *rwHandle) Write(p []byte) (int, error) {
	if h.closed {
		return 0, io.ErrClosedPipe
	}
	if h.readOnly {
		return 0, &sandbox.PathError{Path: h.path, Reason: "handle was opened read-only"}
	}
	h.data = append(h.data, p...)
	h.written = true
	return len(p), nil
}

// Close commits a write handle. A write that never commits would leave the
// caller believing a file exists when the container has nothing.
func (h *rwHandle) Close() error {
	if h.closed {
		return nil
	}
	h.closed = true
	if h.written {
		return h.fs.Write(context.WithoutCancel(context.Background()), h.path, h.data, 0)
	}
	return nil
}

// checkWorkspacePath refuses anything that could reach outside /workspace.
func checkWorkspacePath(p string) error {
	if p == "" {
		return &sandbox.PathError{Path: p, Reason: "empty path"}
	}
	if strings.HasPrefix(p, "/") {
		return &sandbox.PathError{Path: p, Reason: "absolute paths are refused; the workspace root is implicit"}
	}
	if strings.Contains(p, "\\") {
		return &sandbox.PathError{Path: p, Reason: "backslashes are not path separators here"}
	}
	depth := 0
	for _, seg := range strings.Split(p, "/") {
		switch seg {
		case "", ".":
			continue
		case "..":
			depth--
			if depth < 0 {
				return &sandbox.PathError{Path: p, Reason: "parent traversal is not permitted"}
			}
		default:
			depth++
		}
	}
	return nil
}

func cleanWorkspace(p string) string {
	return strings.TrimPrefix(cleanPath(p), "/")
}

// cleanPath collapses . and .. segments lexically.
func cleanPath(p string) string {
	if p == "" {
		return ""
	}
	var segs []string
	for _, s := range strings.Split(p, "/") {
		switch s {
		case "", ".":
		case "..":
			if len(segs) > 0 {
				segs = segs[:len(segs)-1]
			}
		default:
			segs = append(segs, s)
		}
	}
	return "/" + strings.Join(segs, "/")
}

// statProbe is the engine-side question behind Stat.
func statProbe(target string) string {
	// No awk, and no single-quoted string inside the script. The whole thing is
	// handed to the container's `sh -c` as one argv, so a nested quote here used
	// to terminate the outer quoting and produced "Unterminated quoted string".
	// The container then exited nonzero, and because nonzero is also how "file
	// missing" is signalled, every Stat came back ErrFileNotFound on files that
	// existed and read fine — artefact collection failed while the payload itself
	// worked, which is the kind of split-brain that makes a sandbox untrustworthy.
	//
	// `stat -c` output is split on the first space only: size is always the first
	// field, and the type description is everything after it.
	return "p=" + shellQuote(target) + "; " +
		"if [ -d \"$p\" ]; then echo \"d 0 directory\"; " +
		"elif [ -e \"$p\" ] || [ -L \"$p\" ]; then " +
		"sz=$(stat -c %s \"$p\" 2>/dev/null || echo 0); " +
		"ty=$(stat -c %F \"$p\" 2>/dev/null || echo unknown); " +
		"echo \"f $sz $ty\"; " +
		"else echo; fi"
}

// parseStat turns an engine stat line into FileInfo.
func parseStat(path, line string) (sandbox.FileInfo, error) {
	// Shape is "<kind> <size> <type>", emitted by statProbe with spaces because the
	// type description itself contains spaces ("regular file", "directory").
	fields := strings.SplitN(line, " ", 3)
	if len(fields) < 2 {
		return sandbox.FileInfo{}, &sandbox.PathError{Path: path, Reason: "unparseable stat: " + line}
	}
	info := sandbox.FileInfo{Path: path}
	if fields[0] == "d" || (len(fields) == 3 && strings.Contains(fields[2], "directory")) {
		info.Dir = true
		return info, nil
	}
	var n int64
	for _, r := range fields[1] {
		if r < '0' || r > '9' {
			n = 0
			break
		}
		n = n*10 + int64(r-'0')
	}
	info.Size = n
	if len(fields) == 3 && strings.Contains(fields[2], "symbolic link") {
		info.Symlink = true
	}
	return info, nil
}

// octal renders a mode the way chmod expects it: three permission digits, and a
// leading digit only when the setuid/setgid/sticky bits are actually set.
// Padding every mode to four digits works but prints 00755 in reports, which
// reads like a typo to anyone scanning an audit line.
func octal(mode uint32) string {
	if mode == 0 {
		return "644"
	}
	return strconv.FormatUint(uint64(mode&0o7777), 8)
}

// Snapshotter-facing helper: the provider does not declare CapSnapshot, so this
// file deliberately contains no commit/restore implementation. Declaring a
// capability the engine cannot honour is the exact over-claim the contract
// forbids.

// runtimeEntrypoint maps a declared runtime to the argv that reads the payload
// from stdin. The payload never enters the argument list, so code beginning with
// a dash cannot be misread as a flag.
var runtimeEntrypoints = map[string][]string{
	"python":  {"python3", "-"},
	"python3": {"python3", "-"},
	"node":    {"node", "-"},
	"shell":   {"sh", "-s"},
	"sh":      {"sh", "-s"},
	"bash":    {"bash", "-s"},
}

func runtimeEntrypoint(name string) ([]string, error) {
	if argv, ok := runtimeEntrypoints[name]; ok {
		return argv, nil
	}
	return nil, fmt.Errorf("sandbox/provider/docker: unknown runtime %q (known: %s)", name, strings.Join(knownRuntimes(), ", "))
}

func knownRuntimes() []string {
	out := make([]string, 0, len(runtimeEntrypoints))
	for k := range runtimeEntrypoints {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
