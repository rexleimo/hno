package sandbox

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The site's own dead-link enforcement is a switch in a JavaScript file, which means it can
// be turned off in a one-line edit and nothing in Go would notice. It *was* off, for the
// whole life of this repository: 37 links pointed at pages that never existed, the build
// reported success every time, and the only reason it surfaced is that a guide page needed
// the same guarantee.
//
// So the switch gets a gate. This is the same rule as everywhere else in this package --
// an extension point that can be silently disabled is not enforcement -- applied to the
// documentation build rather than to a provider.

const siteConfigRel = "../../../website/.vitepress/config.mjs"

// TestSiteDeadLinksAreEnforced reads the shipped config and fails if dead-link detection is
// disabled again. Commented-out assignments do not count: a line beginning with "//" is not
// configuration.
func TestSiteDeadLinksAreEnforced(t *testing.T) {
	p := mustAbs(t, siteConfigRel)
	b, err := os.ReadFile(p)
	if err != nil {
		// Unreadable is not "absent": the file is the dependency, and silently passing here
		// would mean a renamed config disables the check that keeps links honest.
		t.Fatalf("read %s: %v", p, err)
	}
	var setting string
	for _, ln := range strings.Split(string(b), "\n") {
		trimmed := strings.TrimSpace(ln)
		if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "/*") {
			continue
		}
		if strings.Contains(trimmed, "ignoreDeadLinks") {
			setting = trimmed
			break
		}
	}
	if setting == "" {
		t.Fatalf("%s no longer mentions ignoreDeadLinks at all; if the option was removed, verify the renderer still fails on dead links and delete this gate", siteConfigRel)
	}
	if !strings.Contains(setting, "ignoreDeadLinks: false") {
		t.Errorf("dead-link detection is disabled again (%q): the site previously ran this way and accumulated 37 dead links while every build reported success; fix the links instead of turning the check off", setting)
	}
}

// localLink is one local link found in a site page.
type localLink struct {
	page   string
	target string
}

// siteLocalLinks collects local links from every markdown page under website/, excluding
// fenced code blocks (their URLs are illustrative and are not resolved by anything).
func siteLocalLinks(t *testing.T, siteRoot string) []localLink {
	t.Helper()
	var out []localLink
	err := filepath.WalkDir(siteRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "node_modules", "dist", ".vitepress":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".md") {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		body := fencedBlockRe.ReplaceAllString(string(b), "")
		for _, m := range siteLinkRe.FindAllStringSubmatch(body, -1) {
			target := strings.TrimSpace(m[1])
			if target == "" || strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") ||
				strings.HasPrefix(target, "mailto:") || strings.HasPrefix(target, "#") {
				continue
			}
			out = append(out, localLink{page: path, target: target})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", siteRoot, err)
	}
	return out
}

var (
	siteLinkRe     = regexp.MustCompile(`(?m)(?:^|[^!])\[[^\]]*\]\(([^)\s]+)\)`)
	fencedBlockRe  = regexp.MustCompile("(?s)```.*?```")
	siteMarkdownRe = regexp.MustCompile(`\.md$`)
)

// resolveSiteTarget answers whether one local target points at something real, using the
// same rules the renderer applies: a rooted link is resolved against the site root (with the
// .md suffix optional and index.md standing in for a directory), and a relative link is
// resolved against the page's own directory.
func resolveSiteTarget(siteRoot, page, target string) bool {
	path, _, _ := strings.Cut(target, "#")
	if path == "" {
		return true // pure fragment: the anchor checks own those
	}
	candidates := func(base string) []string {
		return []string{
			base,
			base + ".md",
			filepath.Join(base, "index.md"),
			filepath.Join(siteRoot, "public", strings.TrimPrefix(path, "/")),
		}
	}
	if strings.HasPrefix(path, "/") {
		base := filepath.Join(siteRoot, strings.TrimPrefix(path, "/"))
		return anyExists(candidates(base))
	}
	if siteMarkdownRe.MatchString(target) || !strings.Contains(target, "#") {
		return anyExists(candidates(filepath.Join(filepath.Dir(page), path)))
	}
	return anyExists(candidates(filepath.Join(filepath.Dir(page), path)))
}

func anyExists(paths []string) bool {
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return true
		}
	}
	return false
}

// TestSiteLinksResolve keeps the whole site honest from `go test`, without needing node.
//
// It is not a replacement for the renderer's own check (TestSiteDeadLinksAreEnforced keeps
// that switched on) and it is deliberately limited to what can be decided from the
// filesystem: it says nothing about fragments, and it does not know about rewrites the
// renderer applies. Its job is to fail early and precisely, with a file name, on a machine
// where the site is never built.
func TestSiteLinksResolve(t *testing.T) {
	siteRoot := mustAbs(t, "../../../website")
	// Assert the root before drawing conclusions from it: a wrong path reads as "every link
	// is dead", which looks like diligence and is the opposite.
	if _, err := os.Stat(filepath.Join(siteRoot, ".vitepress", "config.mjs")); err != nil {
		t.Fatalf("site root %s is not the VitePress site (%v); a misresolved root cannot distinguish a broken page from a broken check", siteRoot, err)
	}

	links := siteLocalLinks(t, siteRoot)
	if len(links) < 100 {
		t.Fatalf("only %d local links found across %s; a scanner that finds almost nothing always passes", len(links), siteRoot)
	}
	broken := 0
	for _, l := range links {
		if !resolveSiteTarget(siteRoot, l.page, l.target) {
			broken++
			rel, _ := filepath.Rel(siteRoot, l.page)
			t.Errorf("website/%s links to %q, which does not resolve to a page, a directory index, or a public asset", rel, l.target)
		}
	}
	if broken > 0 {
		t.Logf("%d of %d local links do not resolve; the site build fails on these too now that ignoreDeadLinks is false", broken, len(links))
	}
}
