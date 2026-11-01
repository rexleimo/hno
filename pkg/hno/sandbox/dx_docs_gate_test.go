package sandbox

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode"
)

// The documentation half of the DX gate (design §P5: guide numbers must match what the runtime
// reports). A doc page that drifts is worse than no page: the reader has no reason to distrust
// it, so the drift becomes an operating assumption.
//
// Two things are checked against the code rather than against anyone's memory:
//
//  1. every default in the guide's Limits/Timeouts tables equals the resolved value, formatted
//     the way the code itself renders it (Timeouts.Provision is 2m0s, not "120s");
//  2. every qualified API name in the guide resolves to an exported symbol of the package it
//     claims it belongs to, including names used only inside prose or code fences.

// guidePages lists every page that documents this runtime, in each locale the site
// publishes. The list is deliberately explicit and a missing page is a failure rather
// than a skip: with t.Skipf, deleting the guide would have turned all five doc gates green,
// which is the same "gate that cannot fail" defect this batch keeps finding. Adding a locale
// here is part of adding that locale's page.
var guidePages = []string{
	"../../../website/guide/sandbox-runtime.md",
	"../../../website/zh/guide/sandbox-runtime.md",
	// The task-oriented walkthrough. It is gated by the same rules as the reference
	// page: every API name it mentions must exist, and both locales must mention the
	// same set — a tutorial that drifts from the code teaches the wrong thing faster
	// than a reference page does.
	"../../../website/guide/agent-sandbox-quickstart.md",
	"../../../website/zh/guide/agent-sandbox-quickstart.md",
}

func guidePath(t *testing.T, rel string) string {
	t.Helper()
	p, err := filepath.Abs(rel)
	if err != nil {
		t.Fatalf("resolve guide path: %v", err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Errorf("documented guide page %s is missing (%v); delete it from guidePages only as part of removing the page from the site", p, err)
	}
	return p
}

// forEachGuide runs fn once per documented page, with the page name in the subtest.
func forEachGuide(t *testing.T, fn func(t *testing.T, lines []string, page string)) {
	t.Helper()
	forEachGuides(t, guidePages, fn)
}

// forEachGuides runs fn over an explicit page list, for gates that apply to a subset
// (for example the resolved-defaults table, which belongs to the reference page).
func forEachGuides(t *testing.T, pages []string, fn func(t *testing.T, lines []string, page string)) {
	t.Helper()
	for _, rel := range pages {
		rel := rel
		t.Run(strings.ReplaceAll(rel, "../", ""), func(t *testing.T) {
			p := guidePath(t, rel)
			b, err := os.ReadFile(p)
			if err != nil {
				t.Fatalf("read guide %s: %v", p, err)
			}
			fn(t, strings.Split(string(b), "\n"), p)
		})
	}
}

// forEachGuideLine visits every line of every documented page.
func forEachGuideLine(t *testing.T, fn func(page string, lineNo int, line string)) {
	t.Helper()
	for _, rel := range guidePages {
		p := guidePath(t, rel)
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read guide %s: %v", p, err)
		}
		for i, ln := range strings.Split(string(b), "\n") {
			fn(rel, i+1, ln)
		}
	}
}

func readGuide(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(guidePath(t, guidePages[0]))
	if err != nil {
		t.Fatalf("read guide: %v", err)
	}
	return strings.Split(string(b), "\n")
}

// docValue formats a default the way the guide writes it. Keeping the formatter here and the
// strings there means one of them fails when the other drifts.
func docValue(v any) string {
	switch t := v.(type) {
	case time.Duration:
		return t.String()
	case int64:
		switch {
		case t == 0:
			return "0"
		case t%(1<<30) == 0 && t >= 1<<30:
			return fmt.Sprintf("%d GiB", t/(1<<30))
		case t%(1<<20) == 0:
			return fmt.Sprintf("%d MiB", t/(1<<20))
		default:
			return strconv.FormatInt(t, 10)
		}
	default:
		return fmt.Sprint(v)
	}
}

// defaultsPages are the pages that carry the resolved-defaults table. The table is a
// reference artifact, so a task-oriented walkthrough must not be forced to duplicate it:
// requiring every documentation page to copy the same table would make the gate fail for
// the right reason on the wrong page, and inviting a copy is how two tables start to drift.
func defaultsPages() []string {
	return guidePages[:2]
}

func TestGuideNumbersMatchResolvedDefaults(t *testing.T) {
	forEachGuides(t, defaultsPages(), func(t *testing.T, lines []string, page string) {
		rows := map[string]string{}
		for _, ln := range lines {
			if !strings.HasPrefix(ln, "| ") {
				continue
			}
			cells := strings.Split(strings.Trim(ln, "|"), "|")
			if len(cells) < 2 {
				continue
			}
			key := strings.Trim(strings.TrimSpace(cells[0]), "`")
			rows[key] = strings.TrimSpace(cells[1])
		}

		check := func(structName string, sample any) {
			v := reflect.ValueOf(sample)
			tp := v.Type()
			for i := 0; i < tp.NumField(); i++ {
				field := tp.Field(i)
				want := docValue(v.Field(i).Interface())
				got, ok := rows[field.Name]
				if !ok {
					t.Errorf("%s: guide does not document %s.%s at all", structName, structName, field.Name)
					continue
				}
				// The guide may add a qualifier after the value ("0 means the engine decides").
				if !strings.HasPrefix(got, want) {
					t.Errorf("%s: guide says %s.%s = %q, the code says %q", structName, structName, field.Name, got, want)
				}
			}
		}
		check("Limits", DefaultLimits())
		check("Timeouts", DefaultTimeouts())
	})
}

// pkgSurface is the exported surface of one package, for names the guide references.
type pkgSurface struct {
	names   map[string]bool
	members map[string]map[string]bool
}

func surfaceOf(t *testing.T, dir string) pkgSurface {
	t.Helper()
	s := pkgSurface{names: map[string]bool{}, members: map[string]map[string]bool{}}
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
					if !strings.HasSuffix(d.Name.Name, "_test") && exportedNameRe.MatchString(d.Name.Name) {
						if d.Recv == nil {
							s.names[d.Name.Name] = true
						} else if rn := recvName(d.Recv.List[0].Type); rn != "" && exportedNameRe.MatchString(rn) {
							if s.members[rn] == nil {
								s.members[rn] = map[string]bool{}
							}
							s.members[rn][d.Name.Name] = true
						}
					}
				case *ast.GenDecl:
					for _, spec := range d.Specs {
						switch sp := spec.(type) {
						case *ast.TypeSpec:
							if exportedNameRe.MatchString(sp.Name.Name) {
								s.names[sp.Name.Name] = true
								// An interface's methods are exported capability too. The gate
								// only knew struct fields, so documenting sess.Files() —
								// FileService.Write/MkdirAll/Read — was reported as an invented
								// method even though the API exists.
								if it, ok := sp.Type.(*ast.InterfaceType); ok {
									if s.members[sp.Name.Name] == nil {
										s.members[sp.Name.Name] = map[string]bool{}
									}
									for _, m := range it.Methods.List {
										for _, n := range m.Names {
											if exportedNameRe.MatchString(n.Name) {
												s.members[sp.Name.Name][n.Name] = true
											}
										}
									}
								}
								if st, ok := sp.Type.(*ast.StructType); ok {
									if s.members[sp.Name.Name] == nil {
										s.members[sp.Name.Name] = map[string]bool{}
									}
									for _, f := range st.Fields.List {
										for _, n := range f.Names {
											if exportedNameRe.MatchString(n.Name) {
												s.members[sp.Name.Name][n.Name] = true
											}
										}
									}
								}
							}
						case *ast.ValueSpec:
							for _, n := range sp.Names {
								if exportedNameRe.MatchString(n.Name) {
									s.names[n.Name] = true
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

// Guide package aliases: which import path each short name in the page stands for.
var guideAliases = map[string]string{
	"sandbox":   ".",
	"run":       "../tools/run",
	"sgraph":    "./graph",
	"docker":    "./provider/docker",
	"coregraph": "../graph",
}

var qualifiedRe = regexp.MustCompile(`\b(sandbox|run|sgraph|docker|coregraph)\.([A-Z][A-Za-z0-9]*)`)

func TestGuideAPINamesResolve(t *testing.T) {
	surfaces := map[string]pkgSurface{}
	for alias, rel := range guideAliases {
		dir, err := filepath.Abs(rel)
		if err != nil {
			t.Fatalf("resolve %s: %v", alias, err)
		}
		if _, err := os.Stat(dir); err != nil {
			t.Skipf("no %s package at %s", alias, dir)
		}
		surfaces[alias] = surfaceOf(t, dir)
	}

	seenPerPage := map[string]map[string]bool{}
	for _, rel := range guidePages {
		seenPerPage[rel] = map[string]bool{}
	}
	forEachGuideLine(t, func(page string, lineNo int, ln string) {
		seen := seenPerPage[page]
		for _, m := range qualifiedRe.FindAllStringSubmatch(ln, -1) {
			alias, name := m[1], m[2]
			key := alias + "." + name
			if seen[key] {
				continue
			}
			seen[key] = true
			s := surfaces[alias]
			if s.names[name] {
				continue
			}
			// A qualified name may be a member of a type used in the same line
			// ("ApprovalPendingError" then ".Remediation"), or a field of a struct.
			found := false
			for typ, mem := range s.members {
				if mem[name] {
					found = true
					break
				}
				_ = typ
			}
			if found {
				continue
			}
			t.Errorf("guide %s line %d references %s, which is not exported by the %s package", page, lineNo, key, alias)
		}
	})
	for page, seen := range seenPerPage {
		if len(seen) < 25 {
			t.Errorf("guide %s audits only %d qualified names; a rewrite that drops API references would silently empty this gate", page, len(seen))
		}
	}
	// Locales must not drift in API coverage: a translated page that quietly drops an
	// option leaves readers of that locale with an incomplete contract.
	//
	// The comparison is per page pair, not across the whole list. Comparing every page to
	// guidePages[0] made adding a task-oriented walkthrough impossible: a tutorial and a
	// reference page legitimately mention different subsets, and the gate reported each
	// page as "lacking" the other page's subjects.
	for i := 0; i+1 < len(guidePages); i += 2 {
		base := seenPerPage[guidePages[i]]
		rel := guidePages[i+1]
		{
			other := seenPerPage[rel]
			var onlyBase, onlyOther []string
			for k := range base {
				if !other[k] {
					onlyBase = append(onlyBase, k)
				}
			}
			for k := range other {
				if !base[k] {
					onlyOther = append(onlyOther, k)
				}
			}
			if len(onlyBase) > 0 || len(onlyOther) > 0 {
				sort.Strings(onlyBase)
				sort.Strings(onlyOther)
				t.Errorf("guide locales disagree on API references: %s lacks %v; %s lacks %v", rel, capList(onlyBase), rel, capList(onlyOther))
			}
		}
	}
}

// Bare identifier families. This package names options, actions, capabilities, isolation
// levels, and sentinels with fixed prefixes, so a word in the guide wearing one of those
// prefixes is a claim about the API even when the package qualifier is left off -- which is
// exactly how "Action: ActDeny" would otherwise slip past the qualified-name check.
var bareFamilyRe = regexp.MustCompile(`\b(?:With|Act|Isolation|Cap|Overflow|Mount|Profile|Err|Class)[A-Z][A-Za-z0-9]*`)

func TestGuideBareAPINamesResolve(t *testing.T) {
	// Every documented locale gets the same treatment: a broken fence or an invented
	// method call in the translated page is just as broken for its readers, and before
	// D-37 these three gates read only the first page.
	forEachGuide(t, guideBareNames)
}

func guideBareNames(t *testing.T, lines []string, page string) {
	// The universe is the documented family, not one directory: prose about graph nodes
	// legitimately names sandbox/graph options, and a too-narrow universe reports real API
	// as invented -- which trains readers to ignore the gate.
	core := surfaceOf(t, mustAbs(t, "."))
	runPkg := surfaceOf(t, mustAbs(t, "../tools/run"))
	graphPkg := surfaceOf(t, mustAbs(t, "./graph"))
	var buf strings.Builder
	depth := 0
	for _, ln := range lines {
		if strings.HasPrefix(strings.TrimSpace(ln), "```") {
			depth++
			continue
		}
		if depth%2 == 1 {
			continue // inside a fence: the syntax gate owns those, names may be locals
		}
		buf.WriteString(ln)
		buf.WriteByte('\n')
	}
	seen := map[string]bool{}
	for _, name := range bareFamilyRe.FindAllString(buf.String(), -1) {
		if seen[name] {
			continue
		}
		seen[name] = true
		if core.names[name] || runPkg.names[name] || graphPkg.names[name] {
			continue
		}
		t.Errorf("%s prose uses %q, which looks like an API name from a fixed prefix family but is exported by neither pkg/hno/sandbox, pkg/hno/tools/run, nor pkg/hno/sandbox/graph", page, name)
	}
}

func capList(s []string) []string {
	if len(s) > 8 {
		return append(s[:8:8], "...")
	}
	return s
}

func mustAbs(t *testing.T, rel string) string {
	t.Helper()
	p, err := filepath.Abs(rel)
	if err != nil {
		t.Fatalf("resolve %s: %v", rel, err)
	}
	return p
}

// TestGuideCodeFencesParse keeps the copy-paste path honest: a fenced example that is not
// valid Go is not an example.
func TestGuideCodeFencesParse(t *testing.T) {
	// Every documented locale gets the same treatment: a broken fence or an invented
	// method call in the translated page is just as broken for its readers, and before
	// D-37 these three gates read only the first page.
	forEachGuide(t, guideCodeFences)
}

func guideCodeFences(t *testing.T, lines []string, page string) {
	var (
		fence []string
		in    bool
		start int
		n     int
	)
	for i, ln := range lines {
		trimmed := strings.TrimSpace(ln)
		if strings.HasPrefix(trimmed, "```") {
			if !in {
				if !strings.Contains(trimmed, "go") {
					in = false
					continue
				}
				in = true
				start = i + 1
				fence = nil
				continue
			}
			in = false
			n++
			src := strings.Join(fence, "\n")
			fset := token.NewFileSet()
			if _, err := parser.ParseFile(fset, "fence.go", "package p\nfunc _() {\n"+src+"\n}\n", parser.SkipObjectResolution); err != nil {
				// Try as declarations, since some fences are top-level blocks.
				if _, err2 := parser.ParseFile(fset, "fence.go", "package p\n"+src, parser.SkipObjectResolution); err2 != nil {
					t.Errorf("guide fence starting at line %d is not valid Go: %v", start, err)
				}
			}
			continue
		}
		if in {
			fence = append(fence, ln)
		}
	}
	if n < 8 {
		t.Errorf("guide has only %d Go fences; this gate has nothing to say about a page that stopped showing code", n)
	}
}

// memberCallRe catches `something.Name(` inside code fences. A fence can be syntactically
// perfect and still call a method that does not exist -- the guide wrote diag.Summary() and
// diag.OK() against DiagReport, which is a struct with fields and no methods at all, and
// neither the syntax gate nor the prose-name gate noticed. The check is deliberately
// conservative: the name only has to exist as an exported function or as a method of some
// exported type anywhere in the documented family. That is weaker than type checking, which
// is honest about what it cannot catch, but it makes "invented method" a failing state.
var memberCallRe = regexp.MustCompile(`\.([A-Z][A-Za-z0-9]*)\s*\(`)

// stdlibCalls are the packages an example may call without that being a claim about this API.
// Without the allowance the gate drowns in fmt.Println and people turn it off.
var stdlibCalls = map[string]bool{
	"Println": true, "Printf": true, "Print": true, "Sprintf": true, "Fprintf": true,
	"Fprintln": true, "Fatalf": true, "Fatal": true, "Errorf": true, "New": true,
	"WithoutCancel": true, "WithCancel": true, "WithTimeout": true, "Is": true, "As": true,
	"Join": true, "Contains": true, "HasPrefix": true, "TrimSpace": true, "Sleep": true,
	"Since": true, "Now": true, "Getenv": true, "Exit": true, "Stdout": true,
}

func TestGuideFencedMethodCallsExist(t *testing.T) {
	// Every documented locale gets the same treatment: a broken fence or an invented
	// method call in the translated page is just as broken for its readers, and before
	// D-37 these three gates read only the first page.
	forEachGuide(t, guideFencedMethodCalls)
}

func guideFencedMethodCalls(t *testing.T, lines []string, page string) {
	universe := map[string]bool{}
	for _, rel := range []string{".", "../tools/run", "./graph", "./provider/docker", "../graph"} {
		dir := mustAbs(t, rel)
		if _, err := os.Stat(dir); err != nil {
			continue
		}
		s := surfaceOf(t, dir)
		for n := range s.names {
			universe[n] = true
		}
		for _, mem := range s.members {
			for n := range mem {
				universe[n] = true
			}
		}
	}
	in := false
	start := 0
	checked := 0
	for i, ln := range lines {
		trimmed := strings.TrimSpace(ln)
		if strings.HasPrefix(trimmed, "```") {
			if !in {
				in = strings.Contains(trimmed, "go")
				start = i + 1
			} else {
				in = false
			}
			continue
		}
		if !in {
			continue
		}
		// Skip the import block and comments: a comment may describe a defect being fixed.
		if strings.HasPrefix(trimmed, "//") {
			continue
		}
		for _, m := range memberCallRe.FindAllStringSubmatch(ln, -1) {
			name := m[1]
			if stdlibCalls[name] {
				continue
			}
			checked++
			if !universe[name] {
				t.Errorf("guide fence near line %d calls .%s(), which no exported symbol in the sandbox family provides (line %d: %q)",
					start, name, i+1, strings.TrimSpace(ln))
			}
		}
	}
	if checked < 10 {
		t.Errorf("fence call gate saw only %d calls; it would silently pass a page with no API usage left in it", checked)
	}
}

// linkRe matches a markdown link with its target.
var linkRe = regexp.MustCompile(`(?m)\[[^\]]*\]\(([^)]+)\)`)

// headingRe matches an ATX heading, capturing the text.
var headingRe = regexp.MustCompile("(?m)^#{1,6}\\s+(.+?)\\s*$")

// Two facts govern the link checks below, and both were measured rather than assumed.
//
// The site used to set ignoreDeadLinks: true, so `npm run docs:build` completed happily no
// matter how many links pointed at pages that were never written -- a deliberately dead
// /guide/does-not-exist built clean in 68 seconds with exit 0. That flag is now false
// (TestSiteDeadLinksAreEnforced pins it), and the same planted link fails the build with
// "Found dead link /guide/does-not-exist in file guide/sandbox-runtime.md".
//
// The Go checks stay because they cover what the renderer does not: in-page anchors (the
// renderer validates file targets, not fragments), and they run without node installed. So
// `go test` catches guide rot and `npm run docs:build` catches site-wide rot; neither
// replaces the other.
// guideSlug reproduces the anchor slug the VitePress renderer produces, and it was derived by
// comparing against the built HTML for both locales rather than by reading the docs and hoping:
//
//  1. compatibility folding (NFKD) -- full-width forms U+FF01-U+FF5E fold to their ASCII
//     equivalents and U+3000 to a space, so "，" lands on the same path as ",";
//  2. every run of markup/punctuation in the renderer's special set becomes one hyphen (note
//     that this set includes "-", "_" and ".");
//  3. characters with no decomposition and no special meaning are kept verbatim -- which is why
//     the ideographic comma in "项目、文件与输出" survives into the id intact while a full-width
//     comma in the same page becomes a hyphen;
//  4. collapse repeated hyphens, trim the ends, lowercase.
//
// Combining marks (U+0300-U+036F) and C0 controls are stripped by the renderer too; no heading
// here uses them, so the gap is documented rather than silently wrong.
func guideSlug(text string) string {
	folded := make([]rune, 0, len(text))
	for _, r := range text {
		switch {
		case r >= 0xFF01 && r <= 0xFF5E:
			folded = append(folded, rune(int(r)-0xFEE0))
		case r == 0x3000:
			folded = append(folded, ' ')
		default:
			folded = append(folded, r)
		}
	}
	special := func(r rune) bool {
		// The renderer's "special" set, transcribed rather than approximated. It includes "-"
		// and "_" and ".", which is why "P1_2" slugs to "p1-2".
		switch r {
		case ' ', '\t', '\n', '\r', '~', '`', '!', '@', '#', '$', '%', '^', '&', '*', '(', ')', '-', '_',
			'+', '=', '[', ']', '{', '}', '|', '\\', ';', ':', '"', '\'', '“', '”', '‘', '’',
			'<', '>', ',', '.', '?', '/':
			return true
		}
		return false
	}
	var b strings.Builder
	lastDash := false
	for _, r := range folded {
		if special(r) {
			if !lastDash {
				b.WriteRune('-')
				lastDash = true
			}
			continue
		}
		b.WriteRune(unicode.ToLower(r))
		lastDash = false
	}
	return strings.Trim(b.String(), "-")
}

// TestGuideSlugMatchesTheRenderer pins the derivation against ids taken from the built page. The
// two ideographic-punctuation cases are the ones a naive rule gets wrong in opposite directions:
// one keeps the character, the other folds it into a separator.
func TestGuideSlugMatchesTheRenderer(t *testing.T) {
	cases := []struct {
		heading string
		want    string
	}{
		{"From a shell: `hno sandbox`", "from-a-shell-hno-sandbox"},
		{"Failure visibility in the toolkit", "failure-visibility-in-the-toolkit"},
		{"Defaults you are actually getting", "defaults-you-are-actually-getting"},
		{"项目、文件与输出", "项目、文件与输出"},
		{"排查一次运行之前，先问句柄", "排查一次运行之前-先问句柄"},
		{"从遗留 Executor 迁移", "从遗留-executor-迁移"},
		{"P1_2 and sub--two", "p1-2-and-sub-two"},
	}
	for _, c := range cases {
		if got := guideSlug(c.heading); got != c.want {
			t.Errorf("guideSlug(%q) = %q, the renderer produces %q", c.heading, got, c.want)
		}
	}
}

func TestGuideLinksResolve(t *testing.T) {
	forEachGuide(t, guideLinksResolve)
}

func guideLinksResolve(t *testing.T, lines []string, page string) {
	body := strings.Join(lines, "\n")

	// Slugs are derived the way the renderer does it: lowercase, drop markup punctuation,
	// spaces to hyphens. Verified against the built page, which produced
	// "from-a-shell-hno-sandbox" for "From a shell: `hno sandbox`".
	anchors := map[string]bool{}

	for _, m := range headingRe.FindAllStringSubmatch(body, -1) {
		anchors[guideSlug(m[1])] = true
	}

	// Third depth from a nested package to the repo root, and the second time this exact
	// off-by-one made a gate invent findings: a wrong root reads as "every link is dead".
	// The root is asserted before anything is concluded from it.
	siteRoot := mustAbs(t, "../../../website")
	if _, err := os.Stat(filepath.Join(siteRoot, ".vitepress", "config.mjs")); err != nil {
		t.Fatalf("site root %s is not the VitePress site (%v); the link gate cannot tell a wrong path from a dead link", siteRoot, err)
	}
	checked := 0
	for _, m := range linkRe.FindAllStringSubmatch(body, -1) {
		target := strings.TrimSpace(m[1])
		if target == "" {
			t.Errorf("empty link target in the guide")
			continue
		}
		// External and asset links are out of scope for a local-resolution check.
		if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") ||
			strings.HasPrefix(target, "mailto:") {
			continue
		}
		checked++
		path, frag, _ := strings.Cut(target, "#")

		if frag != "" && path == "" {
			if !anchors[guideSlug(frag)] {
				t.Errorf("guide link %q points at an anchor this page does not produce; headings present: %d", target, len(anchors))
			}
			continue
		}

		if strings.HasPrefix(path, "/") {
			rel := strings.TrimPrefix(path, "/")
			if rel == "" {
				continue
			}
			if !strings.HasSuffix(rel, ".md") {
				rel += ".md"
			}
			if _, err := os.Stat(filepath.Join(siteRoot, rel)); err != nil {
				t.Errorf("guide link %q points at %s, which does not exist under website/; this site ignores dead links, so nothing else will catch it", target, rel)
			}
			continue
		}

		// Relative link: resolve against this page's directory.
		abs, err := filepath.Abs(page)
		if err != nil {
			t.Fatalf("resolve %s: %v", page, err)
		}
		rel := filepath.Join(filepath.Dir(abs), path)
		if _, err := os.Stat(rel); err != nil {
			t.Errorf("guide link %q does not resolve to a file (%s)", target, rel)
		}
	}
	if checked == 0 {
		t.Errorf("%s contains no local links at all; the link gate would be silently vacuous", page)
	}
}
