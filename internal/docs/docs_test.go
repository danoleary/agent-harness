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

// adrTreeURL is where docs/README.md links the ADRs. The install bundle does not
// ship docs/adr/, so a relative link to it would be broken in every install
// (#127, scripts/check-bundle-links.sh).
const adrTreeURL = "https://github.com/danoleary/agent-harness/tree/main/docs/adr"

// TestTheIndexListsEveryPage keeps docs/README.md the one map of the docs: a page
// added under docs/ that the index does not link is a page nobody finds.
func TestTheIndexListsEveryPage(t *testing.T) {
	root := repoRoot(t)
	index := filepath.Join(root, "docs", "README.md")
	linked := map[string]bool{}
	for _, l := range prose(t, index) {
		for _, m := range linkRe.FindAllStringSubmatch(l, -1) {
			file, _, _ := strings.Cut(m[1], "#")
			if file == adrTreeURL {
				file = "adr"
			}
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

// section returns a page's "## <prefix>…" section, up to the next "## " heading,
// with its runs of whitespace folded to one space so a phrase can wrap.
func section(t *testing.T, path, prefix string) string {
	t.Helper()
	var out []string
	in := false
	for _, l := range prose(t, path) {
		if strings.HasPrefix(l, "## ") {
			in = strings.HasPrefix(l, "## "+prefix)
		}
		if in {
			out = append(out, l)
		}
	}
	if len(out) == 0 {
		t.Fatalf("%s has no %q section", path, "## "+prefix)
	}
	return strings.Join(strings.Fields(strings.Join(out, "\n")), " ")
}

// TestTheRetrospectiveSkillKeepsFindingsDoableInTheSandbox pins the two rules a
// finding's body leans on, because that body is the next ticket's whole context
// (#379): a CI-flake finding names the failing step the flake washer recorded in
// ci-flake-<run-id>.log (#371), and no suggested fix sends the next session to a
// gh, network or remote step the sandbox cannot take.
func TestTheRetrospectiveSkillKeepsFindingsDoableInTheSandbox(t *testing.T) {
	skill := filepath.Join(repoRoot(t), ".claude/skills/retrospective/SKILL.md")

	friction := section(t, skill, "2. Look for friction")
	for _, want := range []string{"`ci-flake-*.log`", "failing step", "failure line"} {
		if !strings.Contains(friction, want) {
			t.Errorf("the CI flakes bullet does not mention %s; a flake finding must carry the recorded step", want)
		}
	}

	write := section(t, skill, "3. Write findings")
	for _, want := range []string{"sandbox", "`gh`", "network", "remote", "in the body"} {
		if !strings.Contains(write, want) {
			t.Errorf("Write findings does not mention %q; a suggested fix must be doable inside the sandbox", want)
		}
	}
}

// TestTheTDDAndReviewSkillsSayWhereAReleaseNoteGoes pins the rule that ends the
// hunt for a release-notes file (#381): there is none, release.yml generates the
// notes from merged PRs, and a ticket's Release notes item lives in the commit.
// Both the implementing and the reviewing session read it, so both skills carry it.
func TestTheTDDAndReviewSkillsSayWhereAReleaseNoteGoes(t *testing.T) {
	root := repoRoot(t)
	for _, c := range []struct{ skill, section string }{
		{".claude/skills/tdd/SKILL.md", "Finish"},
		{".claude/skills/review-worktree/SKILL.md", "2. Read the diff"},
	} {
		text := section(t, filepath.Join(root, c.skill), c.section)
		for _, want := range []string{"no release-notes file", "`.github/workflows/release.yml`", "--generate-notes", "commit body", "CHANGELOG"} {
			if !strings.Contains(text, want) {
				t.Errorf("%s §%s does not mention %q; say where a ticket's release note goes", c.skill, c.section, want)
			}
		}
	}
}

// TestTheTDDAndReviewSkillsCheckAStreamJSONFieldAgainstTheBundledCLI pins the
// rule that a ticket's sample stream-json line is one instance, not the schema
// (#413): before the harness branches on a field's value, the implementing and
// the reviewing session search the bundled CLI for every value and companion
// field, and a negative test covers each value that must not match.
func TestTheTDDAndReviewSkillsCheckAStreamJSONFieldAgainstTheBundledCLI(t *testing.T) {
	root := repoRoot(t)
	for _, c := range []struct{ skill, section string }{
		{".claude/skills/tdd/SKILL.md", "Repo guards"},
		{".claude/skills/review-worktree/SKILL.md", "2. Read the diff"},
	} {
		text := section(t, filepath.Join(root, c.skill), c.section)
		for _, want := range []string{"stream-json", "`internal/stream`", "§Searching the bundled Claude CLI", "every value", "companion field", "negative test", "not the schema"} {
			if !strings.Contains(text, want) {
				t.Errorf("%s §%s does not mention %q; say to check a stream-json field's full value set in the bundled CLI", c.skill, c.section, want)
			}
		}
	}
}

// TestTheTDDSkillChecksEachNewCallSiteTurnsATestRed pins the wiring check in
// /tdd's Finish (#419): a session that tests a new helper on its own and wires it
// in afterwards never sees a red test for the wiring, so before the commit it
// reverts each new call site, runs the owning package's tests, and writes a test
// through the path's public interface if none fails. Review found the gap after
// the fact in #111, #313 and #416.
func TestTheTDDSkillChecksEachNewCallSiteTurnsATestRed(t *testing.T) {
	text := section(t, filepath.Join(repoRoot(t), ".claude/skills/tdd/SKILL.md"), "Finish")
	for _, want := range []string{"call site", "revert just that line", "`sed -i`", "owning package", "At least one must fail", "restore the line", "public interface", "#111", "#313", "#416"} {
		if !strings.Contains(text, want) {
			t.Errorf("/tdd §Finish does not mention %q; say to check that reverting each new call site's wiring turns a test red", want)
		}
	}
}
