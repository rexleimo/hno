package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// Tool names a package manager the sanctioned install API understands. The
// point of naming tools instead of accepting free-form shell is that a replay
// can be reasoned about, cached per step, and refused when the environment is
// locked down (§17.1).
type Tool string

const (
	ToolPip  Tool = "pip"
	ToolApt  Tool = "apt"
	ToolNpm  Tool = "npm"
	ToolGo   Tool = "go"
	ToolCurl Tool = "curl"
)

// PackageSpec is one declarative install.
type PackageSpec struct {
	Tool   Tool
	Pkgs   []string
	Flags  []string
	Extras map[string]string // tool-specific, e.g. index url
}

// Pip declares Python package installs.
func Pip(pkgs ...string) PackageSpec { return PackageSpec{Tool: ToolPip, Pkgs: pkgs} }

// Apt declares Debian package installs.
func Apt(pkgs ...string) PackageSpec { return PackageSpec{Tool: ToolApt, Pkgs: pkgs} }

// Npm declares Node package installs.
func Npm(pkgs ...string) PackageSpec { return PackageSpec{Tool: ToolNpm, Pkgs: pkgs} }

// GoGet declares Go module installs.
func GoGet(pkgs ...string) PackageSpec { return PackageSpec{Tool: ToolGo, Pkgs: pkgs} }

// WithFlags adds tool flags; they are part of the cache key.
func (p PackageSpec) WithFlags(flags ...string) PackageSpec {
	p.Flags = append(append([]string(nil), p.Flags...), flags...)
	return p
}

// With sets a tool-specific attribute such as an index URL.
func (p PackageSpec) With(key, value string) PackageSpec {
	if p.Extras == nil {
		p.Extras = map[string]string{}
	}
	p.Extras[key] = value
	return p
}

// String renders the declaration for logs and hints.
func (p PackageSpec) String() string {
	return fmt.Sprintf("%s %s", p.Tool, strings.Join(p.Pkgs, " "))
}

// requirementArgs returns the ordered arguments recorded in a spec step.
func (p PackageSpec) requirementArgs() []string {
	out := make([]string, 0, len(p.Pkgs)+len(p.Flags))
	out = append(out, p.Pkgs...)
	out = append(out, p.Flags...)
	return out
}

// installCommand renders argv for the install, with argv assembled in Go so
// shell metacharacters in package names are data, not code.
func (p PackageSpec) installCommand() []string {
	switch p.Tool {
	case ToolPip:
		return p.argv("python3", "-m", "pip", "install", "--no-input", "--disable-pip-version-check")
	case ToolApt:
		return p.argv("apt-get", "install", "-y", "--no-install-recommends")
	case ToolNpm:
		return p.argv("npm", "install", "--no-audit", "--no-fund")
	case ToolGo:
		return p.argv("go", "get")
	case ToolCurl:
		return p.argv("curl", "-fsSL", "--output-dir", "/tmp")
	default:
		return append([]string{string(p.Tool)}, p.Pkgs...)
	}
}

// argv assembles base command, caller flags, sorted extras, then packages, in
// that order, for every tool. Flags and extras are part of the declaration, so
// dropping them would make Install(...) silently install something else than
// what was recorded in the spec and replayed on rebuild.
func (p PackageSpec) argv(base ...string) []string {
	argv := append([]string(nil), base...)
	argv = append(argv, p.Flags...)
	for _, kv := range p.sortedExtras() {
		argv = append(argv, "--"+kv[0]+"="+kv[1])
	}
	return append(argv, p.Pkgs...)
}

// sortedExtras keeps generated argv deterministic so a step's cache key is
// stable across processes.
func (p PackageSpec) sortedExtras() [][2]string {
	out := make([][2]string, 0, len(p.Extras))
	for k, v := range p.Extras {
		out = append(out, [2]string{k, v})
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	return out
}

// toRequest turns the declaration into the request the provider executes.
func (p PackageSpec) toRequest() Request {
	argv := p.installCommand()
	if p.Tool == ToolApt {
		// apt needs an index refresh first. The composite is assembled from
		// quoted argv elements, never from raw caller strings.
		return Request{Argv: []string{"sh", "-c", "apt-get update -qq && " + strings.Join(shellQuoteAll(argv), " ")}}
	}
	return Request{Argv: argv}
}

// shellQuoteAll quotes each argv element for an sh -c composite command.
func shellQuoteAll(argv []string) []string {
	out := make([]string, 0, len(argv))
	for _, a := range argv {
		out = append(out, shellQuoteWord(a))
	}
	return out
}

// shellQuoteWord single-quotes one word for sh.
func shellQuoteWord(word string) string {
	return "'" + strings.ReplaceAll(word, "'", `'\''`) + "'"
}

// hashString is the shared content digest used by steps and inputs.
func hashString(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:16]
}

// inputDigest hashes an input's declared content without holding it in memory
// when a reader is supplied.
func inputDigest(in Input) string {
	if in.Bytes != nil {
		return hashString(string(in.Bytes))
	}
	if in.HostPath != "" {
		return "host:" + in.HostPath
	}
	if in.Reader != nil {
		return "stream"
	}
	return "empty"
}

// Fingerprint identifies the package declaration for spec caching.
func (p PackageSpec) Fingerprint() string {
	h := sha256.New()
	fmt.Fprintf(h, "tool=%s\n", p.Tool)
	for _, pkg := range p.Pkgs {
		fmt.Fprintf(h, "pkg=%s\n", pkg)
	}
	for _, f := range p.Flags {
		fmt.Fprintf(h, "flag=%s\n", f)
	}
	for _, kv := range p.sortedExtras() {
		fmt.Fprintf(h, "extra=%s=%s\n", kv[0], kv[1])
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}
