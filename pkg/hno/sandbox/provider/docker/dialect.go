package docker

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// Dialect names the CLI flavour the provider drives.
//
// The engines share a lifecycle vocabulary (create / start / exec / cp / rm) and
// differ only in how you ask them questions: which subcommand reports a version,
// whether listing can filter by label, and whether inspect accepts a Go
// template. Splitting that into a second provider would duplicate the entire
// lifecycle and let the two copies drift, which is the maintenance shape the
// contract layer exists to prevent. The differences belong in a value.
type Dialect string

const (
	// DialectDocker is the Docker/Podman CLI surface.
	DialectDocker Dialect = "docker"
	// DialectApple is Apple's `container` CLI (Containerization). It creates real
	// standing Linux VM-backed containers on macOS without Docker Desktop, without
	// /dev/kvm, and without a network pull.
	DialectApple Dialect = "apple-container"
)

// dialect knows the argv shapes that differ between engines. Everything else in
// the provider is shared.
type dialect interface {
	// probe reports whether the engine answers at all. It must not create anything
	// and must not touch the network.
	probe(ctx context.Context, r Runner, bin string) (ok bool, detail string, err error)
	// createArgs renders the create invocation. Both dialects accept the same
	// create flags, which is why the lifecycle stays shared.
	createArgs(spec createSpec) []string
	// ids lists sandbox-managed container ids. Docker filters server-side by
	// label; Apple lists everything and filters here.
	ids(ctx context.Context, r Runner, bin string) ([]string, error)
	// listed returns every managed container with the session label it carries,
	// which is how "which sandboxes is this engine holding" is answered from a
	// different process than the one that opened them.
	listed(ctx context.Context, r Runner, bin string) ([]listedContainer, error)
	// running reports whether one container is up.
	running(ctx context.Context, r Runner, bin, id string) (bool, error)
	// hasImage reports whether the image is already in the local store. The
	// provider never pulls implicitly, so this decides whether a run can proceed
	// at all, and the two CLIs answer it in different shapes.
	hasImage(ctx context.Context, r Runner, bin, image string) (bool, error)
	// name is the dialect id carried into diagnostics.
	name() Dialect
}

// listedContainer is one managed container as the engine reports it.
type listedContainer struct {
	ID      string
	Session string
}

// createSpec is everything the create argv needs, kept flat so neither dialect
// reaches back into the provider.
type createSpec struct {
	Name       string
	Image      string
	WorkingDir string
	Env        map[string]string
	Labels     map[string]string
	Mounts     []mount
	Network    string
	CPUCount   int
	MemoryMB   int64
	PidsLimit  int
	Argv       []string
	ReadOnly   bool
}

func newDialect(d Dialect, bin string) dialect {
	if d == DialectApple || strings.Contains(bin, "container") {
		return appleDialect{}
	}
	return dockerDialect{}
}

// ---- Docker / Podman -------------------------------------------------------

type dockerDialect struct{}

func (dockerDialect) name() Dialect { return DialectDocker }

// probe asks two questions: does the CLI answer, and does it accept create. The
// create question is what caught the compatibility shim that reported a healthy
// version and then refused every container.
func (d dockerDialect) probe(ctx context.Context, r Runner, bin string) (bool, string, error) {
	res, err := r.Run(ctx, Command{
		Bin:  bin,
		Args: []string{"version", "--format", "{{.Server.Version}}"},
	})
	if err != nil {
		return false, "the container CLI could not be run", err
	}
	if res.Exit != 0 {
		return false, "the daemon is not answering version", fmt.Errorf("%s", strings.TrimSpace(res.Stderr))
	}
	return true, "", nil
}

func (dockerDialect) createArgs(s createSpec) []string {
	args := []string{"create", "--pull=never"}
	if s.Name != "" {
		args = append(args, "--name", s.Name)
	}
	if s.Network != "" {
		args = append(args, "--network", s.Network)
	}
	if s.WorkingDir != "" {
		args = append(args, "--workdir", s.WorkingDir)
	}
	if s.ReadOnly {
		args = append(args, "--read-only")
	}
	for k, v := range s.Env {
		args = append(args, "--env", k+"="+v)
	}
	for k, v := range s.Labels {
		args = append(args, "--label", k+"="+v)
	}
	for _, m := range s.Mounts {
		args = append(args, "--volume", bindSource(m)+":"+m.Dest+roSuffix(m))
	}
	if s.CPUCount > 0 {
		args = append(args, "--cpus", fmt.Sprintf("%d", s.CPUCount))
	}
	if s.MemoryMB > 0 {
		args = append(args, "--memory", memString(s.MemoryMB<<20))
	}
	if s.PidsLimit > 0 {
		args = append(args, "--pids-limit", fmt.Sprintf("%d", s.PidsLimit))
	}
	return append(args, append([]string{s.Image}, s.Argv...)...)
}

// pidsLimit is docker-only: Apple's CLI has no equivalent flag, so pretending
// otherwise would silently accept a limit the engine ignores.
func (dockerDialect) supportsPidsLimit() bool { return true }

func (dockerDialect) ids(ctx context.Context, r Runner, bin string) ([]string, error) {
	out, err := r.Run(ctx, Command{
		Bin: bin,
		Args: []string{"ps", "-a", "--no-trunc",
			"--filter", "label=" + LabelManaged,
			"--format", "{{.ID}}"},
	})
	if err != nil {
		return nil, err
	}
	if out.Exit != 0 {
		return nil, fmt.Errorf("listing containers failed: %s", strings.TrimSpace(out.Stderr))
	}
	return splitLines(out.Stdout), nil
}

// hasImage uses `image inspect`, which exits nonzero for a missing image.
func (dockerDialect) hasImage(ctx context.Context, r Runner, bin, image string) (bool, error) {
	out, err := r.Run(ctx, Command{Bin: bin, Args: []string{"image", "inspect", "--format", "{{.Id}}", image}})
	if err != nil {
		return false, err
	}
	return out.Exit == 0 && strings.TrimSpace(out.Stdout) != "", nil
}

// listed reuses the ids query but asks for the session label too, in the one
// pipe-delimited shape docker's template supports.
func (d dockerDialect) listed(ctx context.Context, r Runner, bin string) ([]listedContainer, error) {
	out, err := r.Run(ctx, Command{
		Bin: bin,
		Args: []string{"ps", "--all", "--no-trunc",
			"--filter", "label=" + LabelManaged,
			"--format", "{{.ID}}|{{.Label \"` + LabelSession + `\"}}"},
	})
	if err != nil {
		return nil, err
	}
	if out.Exit != 0 {
		return nil, fmt.Errorf("listing containers failed: %s", strings.TrimSpace(out.Stderr))
	}
	var items []listedContainer
	for _, line := range splitLines(out.Stdout) {
		// A line without the separator is tolerated rather than skipped. Docker
		// always emits `id|label`, but a version that changes the template must not
		// turn the sweep into a silent no-op: a container the sweeper skips is a
		// container that leaks.
		id, session, _ := strings.Cut(line, "|")
		if session == "<no value>" {
			session = ""
		}
		if id = strings.TrimSpace(id); id == "" {
			continue
		}
		items = append(items, listedContainer{ID: id, Session: strings.TrimSpace(session)})
	}
	return items, nil
}

func (dockerDialect) running(ctx context.Context, r Runner, bin, id string) (bool, error) {
	out, err := r.Run(ctx, Command{
		Bin:  bin,
		Args: []string{"inspect", "--format", "{{.State.Running}}", id},
	})
	if err != nil {
		return false, err
	}
	return out.Exit == 0 && strings.TrimSpace(out.Stdout) == "true", nil
}

// ---- Apple container -------------------------------------------------------

type appleDialect struct{}

func (appleDialect) name() Dialect { return DialectApple }

// probe uses `--version`, because Apple's CLI has no `version` subcommand at all
// and would otherwise fail a healthy engine.
func (d appleDialect) probe(ctx context.Context, r Runner, bin string) (bool, string, error) {
	res, err := r.Run(ctx, Command{Bin: bin, Args: []string{"--version"}})
	if err != nil {
		return false, "the container CLI could not be run", err
	}
	if res.Exit != 0 {
		return false, "Apple container is not answering; its system service may be stopped",
			fmt.Errorf("%s", strings.TrimSpace(res.Stderr))
	}
	return true, "", nil
}

// createArgs matches the Docker flag spellings this provider relies on: `-l`,
// `--network`, `-w`, `-v`, `--name`, `--memory`, `--cpus` all exist upstream, so
// only the create-safety flags are omitted (`--pull=never` is not a create flag
// here, and the engine refuses to pull when the image is absent anyway).
func (appleDialect) createArgs(s createSpec) []string {
	args := []string{"create"}
	if s.Name != "" {
		args = append(args, "--name", s.Name)
	}
	if s.Network != "" {
		args = append(args, "--network", s.Network)
	}
	if s.WorkingDir != "" {
		args = append(args, "--workdir", s.WorkingDir)
	}
	if s.ReadOnly {
		args = append(args, "--read-only")
	}
	for k, v := range s.Env {
		args = append(args, "--env", k+"="+v)
	}
	for k, v := range s.Labels {
		args = append(args, "--label", k+"="+v)
	}
	for _, m := range s.Mounts {
		args = append(args, "--volume", bindSource(m)+":"+m.Dest+roSuffix(m))
	}
	if s.CPUCount > 0 {
		args = append(args, "--cpus", fmt.Sprintf("%d", s.CPUCount))
	}
	if s.MemoryMB > 0 {
		args = append(args, "--memory", memString(s.MemoryMB<<20))
	}
	return append(args, append([]string{s.Image}, s.Argv...)...)
}

// ids lists everything and filters on the label here, because Apple's `list` has
// no server-side label filter and `--format` only accepts json/table/yaml.
func (appleDialect) ids(ctx context.Context, r Runner, bin string) ([]string, error) {
	out, err := r.Run(ctx, Command{Bin: bin, Args: []string{"list", "--all", "--format", "json"}})
	if err != nil {
		return nil, err
	}
	if out.Exit != 0 {
		return nil, fmt.Errorf("listing containers failed: %s", strings.TrimSpace(out.Stderr))
	}
	// Measured against a real engine, labels are NOT a top-level key on the list
	// entry: they live under `configuration.labels`, and there is no top-level
	// `name` — the id doubles as the name when one was supplied. Reading the
	// obvious keys would silently return an empty set, which makes the orphan
	// sweep look healthy while it reclaims nothing.
	var items []struct {
		ID            string `json:"id"`
		Configuration struct {
			Labels map[string]string `json:"labels"`
		} `json:"configuration"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.Stdout)), &items); err != nil {
		return nil, fmt.Errorf("parsing container list: %w", err)
	}
	var ids []string
	for _, it := range items {
		if it.Configuration.Labels[LabelManaged] != "" && it.ID != "" {
			ids = append(ids, it.ID)
		}
	}
	return ids, nil
}

// running filters the json listing rather than using `inspect --format`, which
// Apple's CLI does not accept.
// hasImage reads the compact table from `image list`. It deliberately does not
// ask for json: measured against a real engine, Apple's `image list --format
// json` prints every image's full manifest and build history, which is tens of
// kilobytes per image and pointless when all that is needed is a name and a tag.
// Table names appear without the registry prefix, so both sides are normalised.
func (appleDialect) hasImage(ctx context.Context, r Runner, bin, image string) (bool, error) {
	out, err := r.Run(ctx, Command{Bin: bin, Args: []string{"image", "list"}})
	if err != nil {
		return false, err
	}
	if out.Exit != 0 {
		return false, fmt.Errorf("listing images failed: %s", strings.TrimSpace(out.Stderr))
	}
	want := shortImageName(image)
	rows := strings.Split(out.Stdout, "\n")
	if len(rows) > 0 && strings.HasPrefix(strings.ToUpper(strings.TrimSpace(rows[0])), "NAME") {
		rows = rows[1:] // header row
	}
	for _, row := range rows {
		fields := strings.Fields(row)
		if len(fields) < 2 {
			continue
		}
		// "golang  1.26  <digest>" is what `create golang:1.26` accepts.
		candidate := shortImageName(fields[0])
		if fields[1] != "<none>" {
			candidate += ":" + fields[1]
		}
		if strings.EqualFold(candidate, want) || strings.EqualFold(candidate, image) {
			return true, nil
		}
	}
	return false, nil
}

// shortImageName strips registry and library namespaces so
// "docker.io/library/golang:1.26" and "golang:1.26" compare equal.
func shortImageName(s string) string {
	if i := strings.LastIndex(s, "/"); i >= 0 {
		s = s[i+1:]
	}
	return s
}

// listed reads the same nested-label schema as ids.
func (d appleDialect) listed(ctx context.Context, r Runner, bin string) ([]listedContainer, error) {
	out, err := r.Run(ctx, Command{Bin: bin, Args: []string{"list", "--all", "--format", "json"}})
	if err != nil {
		return nil, err
	}
	if out.Exit != 0 {
		return nil, fmt.Errorf("listing containers failed: %s", strings.TrimSpace(out.Stderr))
	}
	var items []struct {
		ID            string `json:"id"`
		Configuration struct {
			Labels map[string]string `json:"labels"`
		} `json:"configuration"`
	}
	trimmed := strings.TrimSpace(out.Stdout)
	if trimmed == "" || trimmed == "[]" {
		return nil, nil
	}
	if err := json.Unmarshal([]byte(trimmed), &items); err != nil {
		return nil, fmt.Errorf("parsing container list: %w", err)
	}
	var out2 []listedContainer
	for _, it := range items {
		if it.Configuration.Labels[LabelManaged] == "" || it.ID == "" {
			continue
		}
		out2 = append(out2, listedContainer{ID: it.ID, Session: it.Configuration.Labels[LabelSession]})
	}
	return out2, nil
}

func (d appleDialect) running(ctx context.Context, r Runner, bin, id string) (bool, error) {
	out, err := r.Run(ctx, Command{Bin: bin, Args: []string{"list", "--format", "json"}})
	if err != nil {
		return false, err
	}
	if out.Exit != 0 {
		return false, fmt.Errorf("listing containers failed: %s", strings.TrimSpace(out.Stderr))
	}
	// `running` asks about one id, so the question goes to the running-only
	// listing: filtering client-side over `--all` would pull every stopped
	// container through the capture ceiling for no reason.
	var items []struct {
		ID     string `json:"id"`
		Status struct {
			State string `json:"state"`
		} `json:"status"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out.Stdout)), &items); err != nil {
		return false, fmt.Errorf("parsing container list: %w", err)
	}
	for _, it := range items {
		if it.ID == id {
			return strings.EqualFold(it.Status.State, "running"), nil
		}
	}
	// Absent from the running list means it is stopped or gone, which is exactly
	// what a lost environment looks like.
	return false, nil
}

// ---- shared renderers ------------------------------------------------------

// mount is the host-side bind this provider asks the engine to make. It is local
// to the provider on purpose: the contract layer's workspace vocabulary is
// CopySnapshot/Bind/Ephemeral, and how a given engine expresses a bind is engine
// detail that must not leak into profiles.
type mount struct {
	Source   string
	Dest     string
	ReadOnly bool
}

// bindSource is the host side of a mount. The caller already resolved it.
func bindSource(m mount) string { return m.Source }

// roSuffix marks a mount read-only in the flag syntax both engines accept.
func roSuffix(m mount) string {
	if m.ReadOnly {
		return ":ro"
	}
	return ""
}

// memString renders bytes as the `<n>M` form both CLIs parse.
func memString(b int64) string {
	mib := b / (1 << 20)
	if mib < 1 {
		mib = 1
	}
	return fmt.Sprintf("%dM", mib)
}

// ImagePresent reports whether an image already sits in the local store, using
// the same dialect rules the provider uses. It is exported so callers (and the
// integration gate) can check a prerequisite through the engine's own vocabulary
// instead of hard-coding a docker-shaped probe that a different CLI will answer
// with a usage error.
func ImagePresent(ctx context.Context, image, binary string, d Dialect) (bool, error) {
	dial := newDialect(d, binary)
	return dial.hasImage(ctx, execRunner{}, binary, image)
}
