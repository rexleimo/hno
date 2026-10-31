package sandbox

import (
	"fmt"
	"path"
	"strings"
)

// resolveInRoot maps a caller-supplied path to a path guaranteed to stay inside
// root. It implements the "no guessing, report the refusal" rule the file
// sandbox already uses (§5): every escape attempt becomes a *PathError naming
// both the path and the root, never a silent cleanup.
//
// root may be an os.Root handle or a directory path; the result is always the
// in-sandbox path, so providers translate it however their transport wants.
func resolveInRoot(root, p string) (string, error) {
	if root == "" {
		return "", &PathError{Path: p, Root: root, Reason: "no sandbox root configured"}
	}
	if p == "" {
		return "", &PathError{Path: p, Root: root, Reason: "empty path"}
	}
	if strings.ContainsRune(p, 0) {
		return "", &PathError{Path: p, Root: root, Reason: "path contains a NUL byte"}
	}
	// An absolute request path is refused, not re-rooted. Re-rooting turns
	// "/etc/passwd" into "<root>/etc/passwd", which looks safe but silently
	// accepts a caller who meant the host path — the confusion is the bug.
	if strings.HasPrefix(p, "/") {
		return "", &PathError{Path: p, Root: root, Reason: "absolute paths are refused; paths are workspace-relative"}
	}
	// Walk the segments and track depth: a path that climbs above the root is
	// refused instead of being silently re-rooted. path.Clean alone would turn
	// "a/../../b" into "b", which looks harmless but is a caller asking for a
	// different tree than the one they were given.
	if escapesRoot(p) {
		return "", &PathError{Path: p, Root: root, Reason: "parent traversal is not permitted"}
	}
	clean := path.Clean("/" + p)
	if strings.HasPrefix(p, "\\") {
		return "", &PathError{Path: p, Root: root, Reason: "backslash-prefixed paths are not interpreted"}
	}
	inside := strings.TrimPrefix(clean, "/")
	if inside == "" {
		inside = "."
	}
	// A root that is itself a prefix of the resolved path is fine; anything that
	// could climb out (.. segments surviving Clean) is impossible by
	// construction, so the remaining check is the explicit symlink/absolute
	// escape hint for callers using wildcards.
	if strings.Contains(inside, "..") {
		return "", &PathError{Path: p, Root: root, Reason: "parent traversal is not permitted"}
	}
	return inside, nil
}

// checkRelPath validates a caller-declared relative path used in inputs or
// artefacts. It is the same rule as file operations so "path" means one thing
// package-wide.
func checkRelPath(kind, p string) error {
	if p == "" {
		return fmt.Errorf("%s path must not be empty", kind)
	}
	if strings.HasPrefix(p, "/") {
		return fmt.Errorf("%s path %q must be relative to the workspace root", kind, p)
	}
	if strings.Contains(p, "..") {
		return fmt.Errorf("%s path %q must not traverse upwards", kind, p)
	}
	if strings.ContainsRune(p, 0) {
		return fmt.Errorf("%s path %q contains a NUL byte", kind, p)
	}
	// Backslashes are refused rather than translated: a workspace path is POSIX
	// inside the sandbox, and accepting "\..\x" would turn a separator
	// disagreement into an escape.
	if strings.Contains(p, "\\") {
		return fmt.Errorf("%s path %q must use forward slashes", kind, p)
	}
	return nil
}

// validEnvEntry accepts KEY=VALUE with a shell-safe key.
func validEnvEntry(entry string) bool {
	key, value, ok := strings.Cut(entry, "=")
	if !ok || key == "" {
		return false
	}
	_ = value
	for i, r := range key {
		if r == '_' {
			continue
		}
		if r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' {
			continue
		}
		if i > 0 && r >= '0' && r <= '9' {
			continue
		}
		return false
	}
	return true
}

// globish reports whether a pattern contains wildcard characters, which lets
// ArtifactSpec.Kind default honestly.
func globish(p string) bool {
	return strings.ContainsAny(p, "*?[")
}

// inferKind resolves KindUnset artefact specs so declarations stay short
// without becoming ambiguous.
func inferKind(spec ArtifactSpec) ArtifactKind {
	if spec.Kind != KindUnset {
		return spec.Kind
	}
	switch {
	case globish(spec.Path):
		return KindGlob
	case strings.HasSuffix(spec.Path, "/"):
		return KindDir
	default:
		return KindFile
	}
}

// isDirKind reports whether the artefact should be collected as a tree.
func (k ArtifactKind) isDirKind() bool { return k == KindDir }

// String names the artefact kind.
func (k ArtifactKind) String() string {
	switch k {
	case KindFile:
		return "file"
	case KindDir:
		return "dir"
	case KindGlob:
		return "glob"
	default:
		return "unset"
	}
}

// isRelPath reports whether p stays inside the workspace root. It is the single
// rule every request path is checked against, so "absolute paths are refused"
// is one function rather than an aspiration (§13.1).
func isRelPath(p string) bool {
	return checkRelPath("path", p) == nil
}

// escapesRoot reports whether a relative path climbs above its starting root.
func escapesRoot(p string) bool {
	depth := 0
	for _, seg := range strings.Split(p, "/") {
		switch seg {
		case "", ".":
			continue
		case "..":
			depth--
			if depth < 0 {
				return true
			}
		default:
			depth++
		}
	}
	return false
}
