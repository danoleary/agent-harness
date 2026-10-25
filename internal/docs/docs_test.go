package docs

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// repoRoot walks up from the package dir to the directory holding go.mod.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test dir")
		}
		dir = parent
	}
}

// pages returns every Markdown file the link check covers, relative to root: the
// root READMEs, scripts/README.md and everything under docs/.
func pages(t *testing.T, root string) []string {
	t.Helper()
	out := []string{"README.md", "CONTEXT.md", "scripts/README.md"}
	err := filepath.WalkDir(filepath.Join(root, "docs"), func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(p, ".md") {
			rel, _ := filepath.Rel(root, p)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

var (
	linkRe    = regexp.MustCompile(`\]\(([^)\s]+)\)`)
	headingRe = regexp.MustCompile(`^#{1,6}\s+(.*?)\s*#*\s*$`)
	slugDrop  = regexp.MustCompile(`[^\p{L}\p{N} _-]`)
)

// prose returns a page's lines with fenced code blocks blanked, so a `](x)` or a
// `# comment` inside a code sample is neither a link nor a heading.
func prose(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(string(b), "\n")
	fenced := false
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "```") {
			fenced = !fenced
			lines[i] = ""
			continue
		}
		if fenced {
			lines[i] = ""
		}
	}
	return lines
}

// anchors returns the heading anchors GitHub generates for a page: lowercased,
// punctuation dropped, spaces to dashes, and -1, -2 … on a repeated heading.
func anchors(t *testing.T, path string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	seen := map[string]int{}
	for _, l := range prose(t, path) {
		m := headingRe.FindStringSubmatch(l)
		if m == nil {
			continue
		}
		text := strings.NewReplacer("`", "", "*", "").Replace(m[1])
		slug := strings.ReplaceAll(slugDrop.ReplaceAllString(strings.ToLower(text), ""), " ", "-")
		if n := seen[slug]; n > 0 {
			out[slug+"-"+strconv.Itoa(n)] = true
		} else {
			out[slug] = true
		}
		seen[slug]++
	}
	return out
}

func TestEveryRelativeLinkResolves(t *testing.T) {
	root := repoRoot(t)
	for _, page := range pages(t, root) {
		for n, l := range prose(t, filepath.Join(root, page)) {
			for _, m := range linkRe.FindAllStringSubmatch(l, -1) {
				target := m[1]
				if strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") {
					continue
				}
				file, anchor, _ := strings.Cut(target, "#")
				resolved := filepath.Join(root, page)
				if file != "" {
					resolved = filepath.Join(filepath.Dir(resolved), file)
					if rel, err := filepath.Rel(root, resolved); err != nil || strings.HasPrefix(rel, "..") {
						continue // a GitHub-relative link like ../../releases, outside the tree
					}
					if _, err := os.Stat(resolved); err != nil {
						t.Errorf("%s:%d: link %q: no such file", page, n+1, target)
						continue
					}
				}
				if anchor != "" && strings.HasSuffix(resolved, ".md") && !anchors(t, resolved)[anchor] {
					t.Errorf("%s:%d: link %q: no heading with anchor #%s", page, n+1, target, anchor)
				}
			}
		}
	}
}

// TestTheIndexListsEveryPage keeps docs/README.md the one map of the docs: a page
// added under docs/ that the index does not link is a page nobody finds.
func TestTheIndexListsEveryPage(t *testing.T) {
	root := repoRoot(t)
	index := filepath.Join(root, "docs", "README.md")
	linked := map[string]bool{}
	for _, l := range prose(t, index) {
		for _, m := range linkRe.FindAllStringSubmatch(l, -1) {
			file, _, _ := strings.Cut(m[1], "#")
			linked[filepath.ToSlash(filepath.Clean(filepath.Join("docs", file)))] = true
		}
	}
	for _, page := range pages(t, root) {
		if !strings.HasPrefix(page, "docs/") || page == "docs/README.md" || strings.HasPrefix(page, "docs/adr/") {
			continue
		}
		if !linked[page] {
			t.Errorf("docs/README.md does not link %s", page)
		}
	}
	if !linked["docs/adr"] {
		t.Error("docs/README.md does not link the ADR directory, docs/adr/")
	}
}

// TestTheTDDSkillListsEveryGuard keeps the /tdd skill's "Repo guards" list in step
// with `make check`: every check-* prerequisite of check: (bar check-scripts,
// which runs the guards' own tests) must be named there, with the script it runs,
// so a session learns a guard before writing code rather than when the gate goes
// red (#136).
func TestTheTDDSkillListsEveryGuard(t *testing.T) {
	root := repoRoot(t)
	b, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	makefile := strings.Split(string(b), "\n")

	var guards []string
	for _, l := range makefile {
		if prereqs, ok := strings.CutPrefix(l, "check:"); ok {
			for _, p := range strings.Fields(prereqs) {
				if strings.HasPrefix(p, "check-") && p != "check-scripts" {
					guards = append(guards, p)
				}
			}
		}
	}
	if len(guards) == 0 {
		t.Fatal("found no check-* prerequisites on the Makefile's check: line")
	}

	const skill = ".claude/skills/tdd/SKILL.md"
	var section []string
	in := false
	for _, l := range prose(t, filepath.Join(root, skill)) {
		if strings.HasPrefix(l, "## ") {
			in = strings.HasPrefix(l, "## Repo guards")
		}
		if in {
			section = append(section, l)
		}
	}
	if len(section) == 0 {
		t.Fatalf("%s has no \"## Repo guards\" section", skill)
	}
	listed := strings.Join(section, "\n")

	scriptRe := regexp.MustCompile(`scripts/[\w.-]+\.sh`)
	for _, guard := range guards {
		if !strings.Contains(listed, "`"+guard+"`") {
			t.Errorf("%s: the Repo guards section does not name `%s`; add a line saying what it rejects", skill, guard)
		}
		script := ""
		for i, l := range makefile {
			if l == guard+":" {
				for _, r := range makefile[i+1:] {
					if !strings.HasPrefix(r, "\t") {
						break
					}
					if s := scriptRe.FindString(r); s != "" {
						script = s
						break
					}
				}
			}
		}
		if script != "" && !strings.Contains(listed, "`"+script+"`") {
			t.Errorf("%s: the Repo guards section does not name `%s`, which `%s` runs", skill, script, guard)
		}
	}
}
