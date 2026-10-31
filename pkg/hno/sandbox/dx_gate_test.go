package sandbox

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// The DX gate (design §P5: what a guide, a CLI, or an error tells a user must match what the
// code actually offers).
//
// Remediation text is the only part of a failed run most callers ever read, and it is the part
// nothing compiles. Three classes of drift were found by hand while writing this, each in text
// that sounded plausible:
//
//   - an option named WithMinIsolation, when the function is WithMinimumIsolation;
//   - two commands, "hno sandbox doctor" and "hno sandbox prune --orphans", for a CLI that does
//     not exist in this repository;
//   - hints that referenced methods by name without saying where they live.
//
// So this test parses the package's own exported surface and checks every API name that appears
// inside a string literal against it. A fix that points at something absent is worse than no
// fix: it converts a failure the user can diagnose into a search for a command that was never
// written.

var (
	withOptionRe   = regexp.MustCompile(`\bWith[A-Z][A-Za-z0-9_]*`)
	typeMemberRe   = regexp.MustCompile(`\b([A-Z][A-Za-z0-9]*)\.([A-Z][A-Za-z0-9]*)\b`)
	cliMentionRe   = regexp.MustCompile(`\bhno\s+[a-z]`)
	exportedNameRe = regexp.MustCompile(`^[A-Z]`)
)

// exportedSurface is what a user is allowed to be told to call.
type exportedSurface struct {
	funcs     map[string]bool
	types     map[string]bool
	methods   map[string]map[string]bool // type -> method/field names
	pkgFields map[string]bool            // exported fields per struct type
}

func parseSurface(t *testing.T, dir string) exportedSurface {
	t.Helper()
	s := exportedSurface{
		funcs:     map[string]bool{},
		types:     map[string]bool{},
		methods:   map[string]map[string]bool{},
		pkgFields: map[string]bool{},
	}
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", dir, err)
	}
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			for _, decl := range file.Decls {
				switch d := decl.(type) {
				case *ast.FuncDecl:
					if !exportedNameRe.MatchString(d.Name.Name) {
						continue
					}
					if d.Recv == nil {
						s.funcs[d.Name.Name] = true
						continue
					}
					recv := recvName(d.Recv.List[0].Type)
					if recv == "" || !exportedNameRe.MatchString(recv) {
						continue
					}
					if s.methods[recv] == nil {
						s.methods[recv] = map[string]bool{}
					}
					s.methods[recv][d.Name.Name] = true
				case *ast.GenDecl:
					if d.Tok != token.TYPE && d.Tok != token.CONST && d.Tok != token.VAR {
						continue
					}
					for _, spec := range d.Specs {
						switch sp := spec.(type) {
						case *ast.TypeSpec:
							if exportedNameRe.MatchString(sp.Name.Name) {
								s.types[sp.Name.Name] = true
								if st, ok := sp.Type.(*ast.StructType); ok {
									if s.methods[sp.Name.Name] == nil {
										s.methods[sp.Name.Name] = map[string]bool{}
									}
									for _, f := range st.Fields.List {
										for _, n := range f.Names {
											if exportedNameRe.MatchString(n.Name) {
												s.methods[sp.Name.Name][n.Name] = true
											}
										}
									}
								}
							}
						case *ast.ValueSpec:
							for _, n := range sp.Names {
								if exportedNameRe.MatchString(n.Name) {
									s.funcs[n.Name] = true // same namespace for the "does it exist" question
								}
							}
						}
					}
				}
			}
		}
	}
	return s
}

func recvName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.StarExpr:
		if id, ok := t.X.(*ast.Ident); ok {
			return id.Name
		}
	case *ast.Ident:
		return t.Name
	}
	return ""
}

// stringLiterals returns every unquoted string literal in the package's non-test files, paired
// with the file it came from for a useful failure message.
func stringLiterals(t *testing.T, dir string) []struct{ file, text string } {
	t.Helper()
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, dir, func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", dir, err)
	}
	var out []struct{ file, text string }
	for _, pkg := range pkgs {
		for path, file := range pkg.Files {
			base := filepath.Base(path)
			ast.Inspect(file, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				v, err := strconv.Unquote(lit.Value)
				if err != nil {
					return true
				}
				out = append(out, struct{ file, text string }{base, v})
				return true
			})
		}
	}
	return out
}

// gateDirs are the trees that emit user-facing remediation text. The provider is included
// because its unavailability messages are the first thing an operator reads.
func gateDirs(t *testing.T) []string {
	dirs := []string{".", filepath.Join("provider", "docker")}
	var ok []string
	for _, d := range dirs {
		if _, err := os.Stat(d); err == nil {
			ok = append(ok, d)
		}
	}
	if len(ok) == 0 {
		t.Fatal("no package directories to audit")
		return nil
	}
	return ok
}

// unionSurface merges the exported names of every audited tree, because remediation text in a
// provider legitimately names core options. The question stays "does this name exist anywhere
// a caller could import it", not "does it exist next door".
func unionSurface(t *testing.T, dirs []string) exportedSurface {
	t.Helper()
	all := parseSurface(t, dirs[0])
	for _, d := range dirs[1:] {
		other := parseSurface(t, d)
		for k, v := range other.funcs {
			all.funcs[k] = all.funcs[k] || v
		}
		for k, v := range other.types {
			all.types[k] = all.types[k] || v
		}
		for typ, mem := range other.methods {
			if all.methods[typ] == nil {
				all.methods[typ] = map[string]bool{}
			}
			for m, v := range mem {
				all.methods[typ][m] = all.methods[typ][m] || v
			}
		}
	}
	return all
}

func TestRemediationTextNamesOnlyExistingOptions(t *testing.T) {
	dirs := gateDirs(t)
	all := unionSurface(t, dirs)
	for _, dir := range dirs {
		for _, lit := range stringLiterals(t, dir) {
			for _, name := range withOptionRe.FindAllString(lit.text, -1) {
				if !all.funcs[name] {
					t.Errorf("%s: remediation text mentions %q, which is not an exported function of %s -- a user following this hint cannot compile it (text: %q)",
						lit.file, name, dir, trim(lit.text))
				}
			}
		}
	}
}

func TestRemediationTextPointsAtNoMissingCLI(t *testing.T) {
	for _, dir := range gateDirs(t) {
		for _, lit := range stringLiterals(t, dir) {
			if cliMentionRe.MatchString(lit.text) {
				t.Errorf("%s: text tells the user to run a shell command, but no sandbox CLI ships in this repository: %q",
					lit.file, trim(lit.text))
			}
		}
	}
}

func TestRemediationQualifiedNamesResolve(t *testing.T) {
	dirs := gateDirs(t)
	surface := unionSurface(t, dirs)
	for _, dir := range dirs {
		for _, lit := range stringLiterals(t, dir) {
			for _, m := range typeMemberRe.FindAllStringSubmatch(lit.text, -1) {
				typ, member := m[1], m[2]
				if !surface.types[typ] {
					// Qualified names inside prose can refer to another package
					// (coregraph.Interrupt and friends); only flag ones whose head is a
					// name this package is expected to own.
					if surface.funcs[typ] || strings.Contains(lit.text, "sandbox.") || strings.Contains(lit.text, "Request.") {
						continue
					}
					continue
				}
				if !surface.methods[typ][member] {
					t.Errorf("%s: text references %s.%s, which does not exist on that type (text: %q)",
						lit.file, typ, member, trim(lit.text))
				}
			}
		}
	}
}

func trim(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 90 {
		return s[:90] + "…"
	}
	return s
}
