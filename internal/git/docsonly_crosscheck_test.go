package git

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// yamlFrame is one ancestor mapping key on the path from the document root to the
// line under inspection, tagged with the column its key sits at. The parser keeps a
// stack of these so it can tell an `on.pull_request.paths` from a `push.paths`
// purely by nesting, without a YAML library (the harness deliberately carries one
// dependency; a controlled, regularly-formatted set of our own workflow files does
// not justify adding another).
type yamlFrame struct {
	indent int
	key    string
}

// pullRequestPathTriggers returns the `on.pull_request.paths` glob list from a
// workflow YAML source, handling both block (`- 'x'` on their own lines) and flow
// (`[a, b]`) sequences. It scopes strictly to the pull_request trigger — a sibling
// `push.paths` (or any other `paths:` key) is ignored — so the cross-check mirrors
// the PR-time checks the harness watches.
func pullRequestPathTriggers(src string) []string {
	var out []string
	var stack []yamlFrame
	collecting := false
	collectIndent := 0
	for _, raw := range strings.Split(src, "\n") {
		line := strings.TrimRight(raw, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))

		// While collecting a block sequence, a deeper `- item` line is an entry;
		// anything at or above the `paths:` column ends the sequence and is
		// reprocessed as a normal line below.
		if collecting {
			if indent > collectIndent && strings.HasPrefix(trimmed, "-") {
				out = append(out, cleanScalar(strings.TrimSpace(trimmed[1:])))
				continue
			}
			collecting = false
		}

		// Dedent/sibling: drop every frame that does not strictly enclose this line.
		for len(stack) > 0 && stack[len(stack)-1].indent >= indent {
			stack = stack[:len(stack)-1]
		}

		key, val, isKey := splitYAMLKey(trimmed)
		if !isKey {
			continue
		}
		if key == "paths" && underPullRequest(stack) {
			if flow, ok := parseFlowList(val); ok {
				out = append(out, flow...)
			} else {
				collecting = true
				collectIndent = indent
			}
			continue
		}
		stack = append(stack, yamlFrame{indent: indent, key: key})
	}
	return out
}

// underPullRequest reports whether the current ancestor stack is exactly the
// `on:` → `pull_request:` chain, so a `paths:` seen there is the PR trigger list.
func underPullRequest(stack []yamlFrame) bool {
	return len(stack) >= 2 &&
		stack[len(stack)-1].key == "pull_request" &&
		stack[len(stack)-2].key == "on"
}

// splitYAMLKey parses a `key:` or `key: value` mapping line. A sequence entry
// (`- …`) is not a key.
func splitYAMLKey(trimmed string) (key, val string, ok bool) {
	if strings.HasPrefix(trimmed, "-") {
		return "", "", false
	}
	i := strings.IndexByte(trimmed, ':')
	if i < 0 {
		return "", "", false
	}
	return strings.TrimSpace(trimmed[:i]), strings.TrimSpace(trimmed[i+1:]), true
}

// parseFlowList parses an inline `[a, 'b', "c"]` sequence into its scalars. Returns
// ok=false when val is not a flow sequence (e.g. an empty value introducing a block).
func parseFlowList(val string) ([]string, bool) {
	if !strings.HasPrefix(val, "[") || !strings.HasSuffix(val, "]") {
		return nil, false
	}
	inner := strings.TrimSpace(val[1 : len(val)-1])
	if inner == "" {
		return nil, true
	}
	var out []string
	for _, part := range strings.Split(inner, ",") {
		if s := cleanScalar(strings.TrimSpace(part)); s != "" {
			out = append(out, s)
		}
	}
	return out, true
}

// cleanScalar strips surrounding single/double quotes and any trailing inline
// comment from a YAML scalar.
func cleanScalar(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 {
		if (s[0] == '\'' && s[len(s)-1] == '\'') || (s[0] == '"' && s[len(s)-1] == '"') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// BEH-705: the docs-only classifier's inert-path allowlist (docsOnlyPath) is a
// hand-maintained list with no automated cross-check against the actual CI-workflow
// `on.pull_request.paths` triggers. A path that triggers a CI job but classifies as
// docs-only would let the harness skip a CI watch for a job that genuinely runs (and
// could go red) — exactly the .agents/skills/** gap manual review caught in BEH-687.
// These tests turn "docs-only ⟹ triggers no CI job" into an enforced invariant by
// parsing the real workflow triggers and asserting none of them classify docs-only.

// Block-sequence form: `paths:` followed by `- 'x'` entries, with a sibling
// `push.paths` that must be ignored.
func TestPullRequestPathTriggersBlockForm(t *testing.T) {
	src := `name: Example
on:
  pull_request:
    branches: [main]
    paths:
      - 'scripts/**'
      - '.github/workflows/**'
  push:
    branches: [main]
    paths:
      - 'never/see/me/**'
`
	got := pullRequestPathTriggers(src)
	want := []string{"scripts/**", ".github/workflows/**"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("pullRequestPathTriggers = %v, want %v", got, want)
	}
}

// Flow-sequence form, quoted and unquoted, single- and multi-element — the shapes
// agent-harness.yaml and terraform.yaml actually use.
func TestPullRequestPathTriggersFlowForm(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  string
		want []string
	}{
		{
			name: "quoted multi",
			src:  "on:\n  pull_request:\n    branches: [main]\n    paths: ['agent-harness/**', '.agents/skills/**']\n",
			want: []string{"agent-harness/**", ".agents/skills/**"},
		},
		{
			name: "unquoted single",
			src:  "on:\n  pull_request:\n    paths: [infra/cloudflare/**]\n",
			want: []string{"infra/cloudflare/**"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := pullRequestPathTriggers(tc.src)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("pullRequestPathTriggers = %v, want %v", got, tc.want)
			}
		})
	}
}

// A `paths:` nested under something other than pull_request (here push, and a job
// step) must never leak into the PR-trigger set — the scoping is what keeps the
// cross-check aligned to the checks the harness actually watches at PR time.
func TestPullRequestPathTriggersIgnoresNonPRPaths(t *testing.T) {
	src := `on:
  push:
    branches: [main]
    paths:
      - 'only/push/**'
jobs:
  build:
    steps:
      - uses: actions/paths-filter
        with:
          paths: 'not/a/trigger/**'
`
	if got := pullRequestPathTriggers(src); len(got) != 0 {
		t.Fatalf("pullRequestPathTriggers = %v, want empty (no pull_request.paths present)", got)
	}
}

// representativeChangedPaths turns one CI-trigger glob into concrete changed paths a
// PR could plausibly carry — including, for any glob that can match an arbitrary file
// under a directory, a markdown file at that directory. The markdown instantiation is
// the load-bearing one: markdown is precisely what docsOnlyPath treats as inert, so a
// `dir/**` trigger whose directory is not excluded (e.g. scripts/**, .agents/skills/**)
// is only caught when we probe it with `dir/SAMPLE.md`.
func TestRepresentativeChangedPathsInjectsMarkdownForOpenGlob(t *testing.T) {
	got := representativeChangedPaths(".agents/skills/**")
	if !containsPath(got, ".agents/skills/SAMPLE.md") {
		t.Fatalf("representativeChangedPaths(.agents/skills/**) = %v, want it to include .agents/skills/SAMPLE.md", got)
	}
}

// An exact-file trigger (no wildcard) yields just that file; there is no directory to
// inject a markdown sibling into.
func TestRepresentativeChangedPathsExactFile(t *testing.T) {
	got := representativeChangedPaths("agent-harness/Dockerfile")
	if strings.Join(got, ",") != "agent-harness/Dockerfile" {
		t.Fatalf("representativeChangedPaths(agent-harness/Dockerfile) = %v, want [agent-harness/Dockerfile]", got)
	}
}

// An extension-pinned glob (`**/*.json`) can never match markdown, so no markdown
// path is injected — probing it with a .md would be a false trigger the workflow
// itself would not fire on.
func TestRepresentativeChangedPathsExtensionPinnedGlobNoMarkdown(t *testing.T) {
	for _, p := range representativeChangedPaths("web/src/tokens/**/*.json") {
		if strings.HasSuffix(p, ".md") {
			t.Fatalf("representativeChangedPaths(web/src/tokens/**/*.json) injected a markdown path %q; the glob only matches .json", p)
		}
	}
}

// TestDocsOnlyClassifierNeverMatchesCITriggerPath is the drift-proof invariant: for
// EVERY `on.pull_request.paths` glob across EVERY workflow, no representative path
// that glob matches may classify as docs-only. If it did, a PR touching only such a
// path would trigger that workflow's CI job while the harness — trusting the
// classifier — skips the CI watch, silently merging past a job that could go red
// (the .agents/skills/** hole BEH-687 fixed by hand). Parsing the real triggers
// means a newly-added prose-ish trigger (a scripts/*.md gate, an infra/** doc job)
// fails this test until the classifier's exclusion list is brought back in sync.
func TestDocsOnlyClassifierNeverMatchesCITriggerPath(t *testing.T) {
	// The roots are read from the Consumer's OWN committed config, not from a
	// constant in this package, so the cross-check measures what herd actually
	// declares. A root dropped from that file fails here, which is the point:
	// the declaration and the workflow triggers must stay in step.
	roots := declaredDocsOnlyRoots(t)

	dir := workflowsDir(t)
	files, err := filepath.Glob(filepath.Join(dir, "*.y*ml"))
	if err != nil {
		t.Fatalf("glob workflows: %v", err)
	}
	if len(files) == 0 {
		t.Fatalf("no workflow files found under %s — the cross-check would be vacuous", dir)
	}

	checked := 0
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		name := filepath.Base(f)
		for _, glob := range pullRequestPathTriggers(string(src)) {
			for _, p := range representativeChangedPaths(glob) {
				checked++
				if DocsOnlyPaths([]string{p}, roots) {
					t.Errorf("%s triggers CI on %q; a changed path %q it matches classifies as docs-only, "+
						"so the harness would skip the CI watch for a job this workflow runs. "+
						"Add the trigger's root to docs_only_excluded_roots in .agent-harness/config.toml.", name, glob, p)
				}
			}
		}
	}
	if checked == 0 {
		t.Fatal("parsed zero pull_request path triggers across all workflows — the cross-check is not exercising anything")
	}
}

// workflowsDir walks up from the test's working directory to the repo root (the
// directory holding .github/workflows) so the check does not hard-code the depth of
// this package under agent-harness/.
// It skips the HARNESS's own .github, which is inert inside a Consumer subtree
// (GitHub reads only the repo-root one) and carries the standalone repo's
// workflows, not this Consumer's. Those have no path triggers, so matching them
// would make this cross-check vacuously green while herd's real triggers went
// unchecked. The harness's directory is the one holding go.mod.
func workflowsDir(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		candidate := filepath.Join(dir, ".github", "workflows")
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			if _, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil {
				return candidate
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("the Consumer's .github/workflows not found walking up from the test working directory")
		}
		dir = parent
	}
}

func containsPath(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// representativeChangedPaths turns a CI-trigger glob into concrete changed paths a PR
// could carry. See the doc on TestRepresentativeChangedPathsInjectsMarkdownForOpenGlob
// for why the markdown instantiation is the binding probe.
func representativeChangedPaths(glob string) []string {
	paths := []string{instantiateGlob(glob)}
	if dir, ok := openDirPrefix(glob); ok {
		paths = append(paths, dir+"SAMPLE.md")
	}
	return paths
}

// instantiateGlob replaces wildcard segments with a literal placeholder to yield one
// concrete path the glob matches (`agent-harness/**` → `agent-harness/x`,
// `web/src/tokens/**/*.json` → `web/src/tokens/x/x.json`, exact files unchanged).
func instantiateGlob(glob string) string {
	segs := strings.Split(glob, "/")
	for i, s := range segs {
		switch {
		case s == "**":
			segs[i] = "x"
		case strings.Contains(s, "*"):
			segs[i] = strings.ReplaceAll(s, "*", "x")
		}
	}
	return strings.Join(segs, "/")
}

// openDirPrefix reports whether the glob can match an arbitrary file (any name,
// any extension) under a directory, and if so returns that directory with a trailing
// slash. Only `**`-terminated globs qualify; an extension-pinned tail like `**/*.json`
// cannot match markdown and so is not open.
func openDirPrefix(glob string) (string, bool) {
	if glob == "**" {
		return "", true
	}
	if strings.HasSuffix(glob, "/**") {
		return glob[:len(glob)-len("**")], true
	}
	return "", false
}

// declaredDocsOnlyRoots reads `docs_only_excluded_roots` out of the Consumer's
// committed .agent-harness/config.toml. It is parsed with a small scanner rather
// than the config loader so this test stays in the git package with no dependency
// on config, matching how the workflow triggers above are parsed.
func declaredDocsOnlyRoots(t *testing.T) []string {
	t.Helper()
	dir := filepath.Dir(workflowsDir(t)) // .github -> repo root's parent of .github
	path := filepath.Join(filepath.Dir(dir), ".agent-harness", "config.toml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	m := regexp.MustCompile(`(?s)docs_only_excluded_roots\s*=\s*\[(.*?)\]`).FindStringSubmatch(string(raw))
	if m == nil {
		t.Fatalf("no docs_only_excluded_roots in %s — the Consumer must declare its roots or the "+
			"docs-only short-circuit is disabled entirely", path)
	}

	var roots []string
	for _, q := range regexp.MustCompile(`"([^"]+)"`).FindAllStringSubmatch(m[1], -1) {
		roots = append(roots, q[1])
	}
	if len(roots) == 0 {
		t.Fatalf("docs_only_excluded_roots in %s is empty — the cross-check would be vacuous", path)
	}
	return roots
}
